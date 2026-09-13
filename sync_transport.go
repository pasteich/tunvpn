package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// publicResolver is a fallback DNS resolver (1.1.1.1) used when the server's
// local resolver fails — systemd-resolved (127.0.0.53) intermittently returns
// "no such host" for some CDNs, which would otherwise drop the connection.
var publicResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, "udp", "1.1.1.1:53")
	},
}

// dialTarget dials host:port on the given network (tcp/udp), falling back to
// public DNS on a resolver error.
func (b *bridge) dialTarget(network, target string) (net.Conn, error) {
	conn, err := net.DialTimeout(network, target, 15*time.Second)
	if err == nil {
		return conn, nil
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return nil, err // not a DNS problem
	}
	host, port, splitErr := net.SplitHostPort(target)
	if splitErr != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	ips, resErr := publicResolver.LookupHost(ctx, host)
	if resErr != nil || len(ips) == 0 {
		return nil, err
	}
	for _, ip := range ips {
		if c, e := net.DialTimeout(network, net.JoinHostPort(ip, port), 15*time.Second); e == nil {
			return c, nil
		}
	}
	return nil, err
}

// Sync transport: bulk data over the Yjs document (sync) channel instead of
// awareness. The relay applies our updates to a shared doc and rebroadcasts
// them reliably and IN ORDER, with no coalescing and raw-byte (ContentBinary)
// payloads — so we just fire frames as fast as the socket allows. No ARQ,
// windows, or reassembly needed.
//
// Wire: each tunnel message is one ContentBinary item appended to root array
// "tun". Item bytes = frame: [connID:4 BE][kind:1][data].
//
// Growth is bounded by syncPruneLoop (delete + server GC). We act only on LIVE
// updates after connect (the initial catch-up state, which also holds unrelated
// note content, is ignored).

const syncRoot = "tun"

func makeSyncFrame(connID uint32, kind byte, data []byte) []byte {
	out := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(out[:4], connID)
	out[4] = kind
	copy(out[5:], data)
	return out
}

func parseSyncFrame(b []byte) (connID uint32, kind byte, data []byte, ok bool) {
	if len(b) < 5 {
		return 0, 0, nil, false
	}
	return binary.BigEndian.Uint32(b[:4]), b[4], b[5:], true
}

// peerStream tracks how far we've consumed one peer client's append stream.
// Each item is a self-contained frame, so we deliver strictly by increasing
// clock and simply skip gaps (e.g. a range lost across a reconnect) instead of
// stalling — the affected connections are reset anyway.
type peerStream struct {
	inited      bool
	delivered   uint64 // highest clock delivered
	deletedUpTo uint64 // highest clock we've asked the server to GC
}

// --- outbound -------------------------------------------------------------

// syncEnqueue queues a frame for the sender loop.
func (b *bridge) syncEnqueue(connID uint32, kind byte, data []byte) {
	if kind == kindData {
		b.bytesSent.Add(uint64(len(data)))
	}
	select {
	case b.syncOutCh <- makeSyncFrame(connID, kind, data):
	case <-b.ctx.Done():
	}
}

// syncSendLoop batches queued frames into Yjs updates and fires them at the
// relay as fast as it will take them.
func (b *bridge) syncSendLoop() {
	const maxItems = 64
	const maxUpdateBytes = 180 * 1024 // hard cap per WS message; the relay drops oversized ones
	var pending []byte                // a frame taken from the queue that didn't fit this batch
	for {
		var first []byte
		if pending != nil {
			first, pending = pending, nil
		} else {
			select {
			case <-b.ctx.Done():
				return
			case first = <-b.syncOutCh:
			}
		}
		chunks := [][]byte{first}
		total := len(first)
		// Drain more, but NEVER let the batch exceed maxUpdateBytes: a frame
		// that wouldn't fit is carried over to the next batch.
		fill := true
		for fill && len(chunks) < maxItems {
			select {
			case f := <-b.syncOutCh:
				if total+len(f) > maxUpdateBytes {
					pending = f
					fill = false
				} else {
					chunks = append(chunks, f)
					total += len(f)
				}
			default:
				fill = false
			}
		}
		start := b.syncClock.Load()
		update := encodeBinaryAppends(b.syncClientID, start, syncRoot, chunks)
		b.syncClock.Store(start + uint64(len(chunks)))
		// Retry the SAME update until it's actually sent — never drop data on a
		// transient error (a dropped batch is a gap that corrupts a live TLS
		// stream). During a reconnect connected flips false; we wait it out and
		// resend on the fresh socket (resending the same clocks is idempotent).
		for {
			for !b.connected.Load() {
				select {
				case <-b.ctx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
			if err := b.writeFrameCurrent(dispatchSync, encodeSyncUpdate(update)); err == nil {
				b.lastSendOK.Store(time.Now().UnixNano())
				break
			}
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// writeFrameCurrent writes to whatever socket 0 currently is (it may be swapped
// by a reconnect between calls).
func (b *bridge) writeFrameCurrent(msgType uint8, body []byte) error {
	return b.writeFrame(b.socks[0], msgType, body)
}

// --- inbound --------------------------------------------------------------

// handleSyncUpdate processes a live update: extract peer frames in clock order
// and demux them into connections.
func (b *bridge) handleSyncUpdate(items []yjsItem) {
	// keep only the peer's binary items (skip our own echo and note content)
	type ci struct {
		clock uint64
		data  []byte
	}
	byClient := map[uint64][]ci{}
	for _, it := range items {
		if it.ref != yContentBinary || it.client == b.syncClientID {
			continue
		}
		byClient[it.client] = append(byClient[it.client], ci{it.clock, it.data})
	}

	if len(byClient) > 0 {
		b.lastPeerSeen.Store(time.Now().UnixNano()) // heard from the peer -> linked
	}

	// Advance per-peer clocks under the lock, collect frames to deliver, then
	// deliver OUTSIDE the lock — delivery can block on socket backpressure and
	// must never stall the reader while holding syncMu.
	var frames [][]byte
	b.syncMu.Lock()
	for client, list := range byClient {
		ps := b.syncPeers[client]
		if ps == nil {
			ps = &peerStream{}
			b.syncPeers[client] = ps
		}
		sort.Slice(list, func(i, j int) bool { return list[i].clock < list[j].clock })
		for _, c := range list {
			if ps.inited && c.clock <= ps.delivered {
				continue // duplicate / already delivered
			}
			if !ps.inited {
				ps.inited = true
				ps.deletedUpTo = c.clock
			}
			ps.delivered = c.clock
			frames = append(frames, c.data)
		}
	}
	b.syncMu.Unlock()

	for _, f := range frames {
		b.syncDeliver(f)
	}
}

// syncPruneLoop periodically deletes already-delivered items (ours and the
// peer's) so the shared doc stays small: the relay GCs deleted content, which
// keeps catch-up states and message sizes bounded. Without this the doc grows
// with every byte sent and the connection eventually dies on size.
func (b *bridge) syncPruneLoop() {
	const margin = 1024 // keep this many recent items per client undeleted
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		if !b.connected.Load() {
			continue // don't touch the socket mid-reconnect
		}

		// our own appended items (never delete clock 0 — it anchors the root type)
		if b.syncSentDeleted == 0 {
			b.syncSentDeleted = 1
		}
		cur := b.syncClock.Load()
		if cur > b.syncSentDeleted+margin {
			to := cur - margin
			del := encodeDeleteUpdate(b.syncClientID, b.syncSentDeleted, to-b.syncSentDeleted)
			if err := b.writeFrameCurrent(dispatchSync, encodeSyncUpdate(del)); err == nil {
				b.syncSentDeleted = to
			}
		}

		// the peer's delivered items
		type pd struct{ client, from, length uint64 }
		var dels []pd
		b.syncMu.Lock()
		for client, ps := range b.syncPeers {
			if ps.delivered > ps.deletedUpTo+margin {
				to := ps.delivered - margin
				dels = append(dels, pd{client, ps.deletedUpTo, to - ps.deletedUpTo})
				ps.deletedUpTo = to
			}
		}
		b.syncMu.Unlock()
		for _, d := range dels {
			del := encodeDeleteUpdate(d.client, d.from, d.length)
			_ = b.writeFrameCurrent(dispatchSync, encodeSyncUpdate(del))
		}
	}
}

// syncWatchdog self-heals a stuck send path: if frames are queued but nothing
// has been sent for a while, the socket is wedged (relay not accepting) — force
// a reconnect so we recover without a manual restart.
func (b *bridge) syncWatchdog() {
	const stallAfter = 20 * time.Second
	b.lastSendOK.Store(time.Now().UnixNano())
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		if !b.connected.Load() || len(b.syncOutCh) == 0 {
			continue // nothing pending, or already reconnecting
		}
		if time.Since(time.Unix(0, b.lastSendOK.Load())) > stallAfter {
			log.Printf("send stalled >%s with %d frames queued — forcing reconnect", stallAfter, len(b.syncOutCh))
			sk := b.socks[0]
			sk.mu.Lock()
			c := sk.c
			sk.mu.Unlock()
			if c != nil {
				c.CloseNow() // readLoop will observe the error and reconnect
			}
			b.lastSendOK.Store(time.Now().UnixNano()) // avoid re-triggering immediately
		}
	}
}

// flowConsumed records bytes written to the local socket and periodically acks
// them back to the peer so it can advance its send window. Sending ~4 acks per
// window keeps credit flowing without ack spam.
func (b *bridge) flowConsumed(s *conn2, n int) {
	s.flowMu.Lock()
	s.consumed += uint64(n)
	c := s.consumed
	send := c-s.ackedPeer >= uint64(b.flowWindow)/4
	if send {
		s.ackedPeer = c
	}
	s.flowMu.Unlock()
	if send {
		var p [8]byte
		binary.BigEndian.PutUint64(p[:], c)
		b.syncEnqueue(uint32(s.id), kindAck, p[:])
	}
}

// syncPingLoop sends a tiny heartbeat so the peer can tell we're alive even
// when no data is flowing, and so our own link indicator stays fresh.
func (b *bridge) syncPingLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		b.syncEnqueue(0, kindPing, nil)
	}
}

func (b *bridge) syncDeliver(frame []byte) {
	connID, kind, data, ok := parseSyncFrame(frame)
	if !ok {
		return
	}
	if kind == kindPing {
		return // liveness only; lastPeerSeen already bumped in handleSyncUpdate
	}
	b.mu.Lock()
	s := b.conns[uint64(connID)]
	if s == nil {
		if b.isServer && kind == kindOpen && len(data) > 0 {
			s = b.newConn(uint64(connID))
			b.conns[uint64(connID)] = s
			target := string(data)
			b.mu.Unlock()
			go b.syncDialAndAttach(s, target)
			return
		}
		if b.isServer && kind == kindOpenUDP && len(data) > 0 {
			s = b.newConn(uint64(connID))
			s.isUDP = true
			b.conns[uint64(connID)] = s
			target := string(data)
			b.mu.Unlock()
			go b.udpServerDial(s, target)
			return
		}
		b.mu.Unlock()
		return // unknown/closed conn — drop
	}
	b.mu.Unlock()

	switch kind {
	case kindData:
		// NON-BLOCKING: the single reader must never block on one slow stream
		// (head-of-line blocking would wedge everything).
		select {
		case s.writeCh <- data:
			b.bytesRecv.Add(uint64(len(data)))
		default:
			if s.isUDP {
				// UDP is lossy — just drop this datagram, keep the flow alive.
			} else {
				// TCP with flow control: shouldn't happen; safety-net teardown.
				b.syncTeardown(s)
			}
		}
	case kindAck:
		if len(data) >= 8 {
			cum := binary.BigEndian.Uint64(data[:8])
			s.flowMu.Lock()
			if cum > s.acked {
				s.acked = cum
			}
			s.flowMu.Unlock()
			s.flowCond.Broadcast() // wake the reader if it was window-blocked
		}
	case kindClose:
		s.signalEOF()
	}
}

func humanBytes(n uint64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// meterLoop shows one live, in-place status line (no per-second scroll spam).
func (b *bridge) meterLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastUp, lastDown uint64
	for {
		select {
		case <-b.ctx.Done():
			fmt.Fprint(os.Stderr, "\n")
			return
		case <-t.C:
		}
		up := b.bytesSent.Load()
		down := b.bytesRecv.Load()
		dUp, dDown := up-lastUp, down-lastDown
		lastUp, lastDown = up, down
		b.mu.Lock()
		conns := len(b.conns)
		b.mu.Unlock()

		var status string
		switch {
		case !b.connected.Load():
			status = "\033[31m● reconnecting\033[0m"
		case time.Since(time.Unix(0, b.lastPeerSeen.Load())) < 6*time.Second:
			status = "\033[32m● linked\033[0m"
		default:
			status = "\033[33m● waiting for peer\033[0m"
		}
		// \r + clear-line: overwrite the same line each tick.
		fmt.Fprintf(os.Stderr, "\r\033[2K%s  down %s (%s/s)  up %s (%s/s)  conns %d",
			status, humanBytes(down), humanBytes(dDown), humanBytes(up), humanBytes(dUp), conns)
	}
}

// syncTeardown removes a connection and unblocks anything waiting on it. Safe to
// call multiple times.
func (b *bridge) syncTeardown(s *conn2) {
	b.mu.Lock()
	delete(b.conns, s.id)
	b.mu.Unlock()
	s.flowMu.Lock()
	s.flowClosed = true
	s.flowMu.Unlock()
	s.flowCond.Broadcast() // unblock a window-stalled reader
	s.finish()
	if c := s.getConn(); c != nil {
		c.Close()
	}
}

// --- attach / dial --------------------------------------------------------

func (b *bridge) syncDialAndAttach(s *conn2, target string) {
	b.dbg("open connID=%d -> %s", s.id, target)
	conn, err := b.dialTarget("tcp", target)
	if err != nil {
		b.dbg("dial %s: %v", target, err)
		b.syncEnqueue(uint32(s.id), kindClose, nil)
		return
	}
	select {
	case <-s.closed:
		conn.Close()
		return
	default:
	}
	b.syncAttach(s, conn)
}

func (b *bridge) syncAttach(s *conn2, conn net.Conn) {
	s.setConn(conn)

	go func() { // writer: inbound frames -> socket
		defer b.syncTeardown(s)
		for {
			select {
			case <-s.closed:
				return
			case <-s.eofCh:
				for {
					select {
					case data := <-s.writeCh:
						if len(data) > 0 {
							if _, err := conn.Write(data); err != nil {
								return
							}
							b.flowConsumed(s, len(data))
						}
					default:
						return
					}
				}
			case data := <-s.writeCh:
				if _, err := conn.Write(data); err != nil {
					return
				}
				b.flowConsumed(s, len(data))
			}
		}
	}()

	go func() { // reader: socket -> outbound frames
		// Only the writer tears the conn down (it's the last to deliver inbound
		// bytes); the reader just tells the peer we're done sending.
		defer b.syncEnqueue(uint32(s.id), kindClose, nil)
		win := uint64(b.flowWindow)
		buf := make([]byte, b.maxData)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				// Per-stream flow control: block until the peer has consumed
				// enough that our outstanding bytes fit the window. This paces
				// this connection's source without touching any other stream.
				s.flowMu.Lock()
				for s.sent-s.acked >= win && !s.flowClosed {
					s.flowCond.Wait()
				}
				closed := s.flowClosed
				if !closed {
					s.sent += uint64(n)
				}
				s.flowMu.Unlock()
				if closed {
					return
				}
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				b.syncEnqueue(uint32(s.id), kindData, chunk)
			}
			if err != nil {
				return
			}
		}
	}()
}

func (b *bridge) syncClientSession(conn net.Conn, target string) {
	if len(target) > b.maxData {
		log.Printf("target too long: %s", target)
		conn.Close()
		return
	}
	id := uint32(randID())
	b.dbg("open connID=%d -> %s", id, target)
	s := b.newConn(uint64(id))
	b.mu.Lock()
	b.conns[uint64(id)] = s
	b.mu.Unlock()
	b.syncEnqueue(id, kindOpen, []byte(target))
	b.syncAttach(s, conn)
}

// syncServeSOCKS is the client-mode SOCKS listener for the sync transport.
func (b *bridge) syncServeSOCKS(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("SOCKS5 listening on %s (sync transport)", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			cmd, target, err := socks5Handshake(c)
			if err != nil {
				log.Printf("socks5: %v", err)
				c.Close()
				return
			}
			switch cmd {
			case socksCmdUDP:
				b.socksUDPAssociate(c)
			default: // CONNECT
				_ = writeSocksReply(c, 0x00, net.IPv4zero, 0)
				b.syncClientSession(c, target)
			}
		}()
	}
}

// --- UDP (SOCKS5 UDP ASSOCIATE over the reliable sync stream) --------------
//
// Each UDP flow is one tunnel connection (kindOpenUDP). Because each syncEnqueue
// becomes one discrete frame, one datagram = one frame end-to-end — no length
// framing needed. UDP is best-effort: on overflow we drop datagrams rather than
// block or tear down.

// syncEnqueueDrop enqueues a frame without blocking; if the send queue is full
// the datagram is dropped (UDP semantics).
func (b *bridge) syncEnqueueDrop(connID uint32, kind byte, data []byte) {
	select {
	case b.syncOutCh <- makeSyncFrame(connID, kind, data):
		if kind == kindData {
			b.bytesSent.Add(uint64(len(data)))
		}
	default:
	}
}

// server side: dial the UDP target and pump datagrams both ways.
func (b *bridge) udpServerDial(s *conn2, target string) {
	b.dbg("open UDP connID=%d -> %s", s.id, target)
	conn, err := b.dialTarget("udp", target)
	if err != nil {
		b.dbg("udp dial %s: %v", target, err)
		b.syncEnqueue(uint32(s.id), kindClose, nil)
		return
	}
	select {
	case <-s.closed:
		conn.Close()
		return
	default:
	}
	b.udpPump(s, conn)
}

// udpPump relays between a connected UDP socket and the tunnel conn.
func (b *bridge) udpPump(s *conn2, conn net.Conn) {
	s.setConn(conn)
	go func() { // tunnel datagrams -> UDP
		defer b.syncTeardown(s)
		for {
			select {
			case <-s.closed:
				return
			case <-s.eofCh:
				return
			case dg := <-s.writeCh:
				s.lastActive.Store(time.Now().UnixNano())
				_, _ = conn.Write(dg)
			}
		}
	}()
	go func() { // UDP datagrams -> tunnel
		defer b.syncEnqueue(uint32(s.id), kindClose, nil)
		rb := make([]byte, 65535)
		for {
			n, err := conn.Read(rb)
			if n > 0 {
				dg := make([]byte, n)
				copy(dg, rb[:n])
				s.lastActive.Store(time.Now().UnixNano())
				b.syncEnqueueDrop(uint32(s.id), kindData, dg)
			}
			if err != nil {
				return
			}
		}
	}()
}

// parseSocksUDPReq parses a SOCKS5 UDP datagram header.
// Layout: [RSV(2)][FRAG(1)][ATYP][DST.ADDR][DST.PORT(2)][DATA].
// Returns the target, the raw ATYP..PORT header (reused verbatim in replies),
// and the payload.
func parseSocksUDPReq(p []byte) (target string, hdr []byte, data []byte, ok bool) {
	if len(p) < 4 || p[2] != 0x00 { // no fragmentation support
		return "", nil, nil, false
	}
	atyp := p[3]
	pos := 4
	var host string
	switch atyp {
	case 0x01:
		if len(p) < pos+4+2 {
			return "", nil, nil, false
		}
		host = net.IP(p[pos : pos+4]).String()
		pos += 4
	case 0x04:
		if len(p) < pos+16+2 {
			return "", nil, nil, false
		}
		host = net.IP(p[pos : pos+16]).String()
		pos += 16
	case 0x03:
		if len(p) < pos+1 {
			return "", nil, nil, false
		}
		l := int(p[pos])
		pos++
		if len(p) < pos+l+2 {
			return "", nil, nil, false
		}
		host = string(p[pos : pos+l])
		pos += l
	default:
		return "", nil, nil, false
	}
	port := int(p[pos])<<8 | int(p[pos+1])
	pos += 2
	hdr = append([]byte{}, p[3:pos]...) // ATYP..PORT
	data = p[pos:]
	return net.JoinHostPort(host, fmt.Sprintf("%d", port)), hdr, data, true
}

// client side: handle a SOCKS5 UDP ASSOCIATE. Binds a UDP relay socket, keeps
// the association alive for the lifetime of the TCP control connection, and
// relays datagrams to per-target tunnel connections.
func (b *bridge) socksUDPAssociate(tcp net.Conn) {
	defer tcp.Close()
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = writeSocksReply(tcp, 0x01, net.IPv4zero, 0)
		return
	}
	defer uc.Close()
	la := uc.LocalAddr().(*net.UDPAddr)
	if err := writeSocksReply(tcp, 0x00, la.IP, la.Port); err != nil {
		return
	}
	b.dbg("UDP ASSOCIATE relay on %s", la)

	type ut struct {
		s   *conn2
		hdr []byte
	}
	targets := map[string]*ut{}
	var mu sync.Mutex
	var appAddr atomic.Pointer[net.UDPAddr]

	go func() {
		rb := make([]byte, 65535)
		for {
			n, from, err := uc.ReadFromUDP(rb)
			if err != nil {
				return
			}
			target, hdr, data, ok := parseSocksUDPReq(rb[:n])
			if !ok {
				continue
			}
			appAddr.Store(from)
			mu.Lock()
			t := targets[target]
			if t == nil {
				s := b.newConn(randID())
				s.isUDP = true
				b.mu.Lock()
				b.conns[s.id] = s
				b.mu.Unlock()
				t = &ut{s: s, hdr: hdr}
				targets[target] = t
				b.syncEnqueue(uint32(s.id), kindOpenUDP, []byte(target))
				b.udpClientWriter(s, uc, &appAddr, hdr)
			}
			sc := t.s
			mu.Unlock()
			dg := make([]byte, len(data))
			copy(dg, data)
			sc.lastActive.Store(time.Now().UnixNano())
			b.syncEnqueueDrop(uint32(sc.id), kindData, dg)
		}
	}()

	io.Copy(io.Discard, tcp) // blocks until the control connection closes
	mu.Lock()
	for _, t := range targets {
		b.syncTeardown(t.s)
	}
	mu.Unlock()
}

// udpClientWriter delivers tunnel datagrams (replies) back to the app, wrapped
// in the SOCKS5 UDP reply header.
func (b *bridge) udpClientWriter(s *conn2, uc *net.UDPConn, appAddr *atomic.Pointer[net.UDPAddr], hdr []byte) {
	go func() {
		defer b.syncTeardown(s) // note: does not close the shared uc (netc unset)
		pre := append([]byte{0x00, 0x00, 0x00}, hdr...)
		for {
			select {
			case <-s.closed:
				return
			case dg := <-s.writeCh:
				addr := appAddr.Load()
				if addr == nil {
					continue
				}
				out := make([]byte, 0, len(pre)+len(dg))
				out = append(out, pre...)
				out = append(out, dg...)
				s.lastActive.Store(time.Now().UnixNano())
				_, _ = uc.WriteToUDP(out, addr)
			}
		}
	}()
}

// syncReapLoop closes UDP flows that have been idle too long (they have no
// natural EOF; the association tears them down, but reap protects against a
// client that vanished without closing).
func (b *bridge) syncReapLoop() {
	const idle = 2 * time.Minute
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		var dead []*conn2
		b.mu.Lock()
		for _, s := range b.conns {
			if s.isUDP && now.Sub(time.Unix(0, s.lastActive.Load())) > idle {
				dead = append(dead, s)
			}
		}
		b.mu.Unlock()
		for _, s := range dead {
			b.syncTeardown(s)
		}
	}
}

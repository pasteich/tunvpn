package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// --- lib0 varint/string/byte-array primitives -----------------------------

var errUnexpectedEnd = errors.New("tunnel: unexpected end of buffer")

type varDecoder struct {
	buf []byte
	pos int
}

func newVarDecoder(buf []byte) *varDecoder { return &varDecoder{buf: buf} }

func (d *varDecoder) readByte() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, errUnexpectedEnd
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

func (d *varDecoder) readVarUint() (uint64, error) {
	var result uint64
	var shift uint
	for {
		b, err := d.readByte()
		if err != nil {
			return 0, err
		}
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, nil
		}
		shift += 7
		if shift > 63 {
			return 0, errors.New("tunnel: varint too long")
		}
	}
}

func (d *varDecoder) readBytes(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.buf) {
		return nil, errUnexpectedEnd
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *varDecoder) readVarUint8Array() ([]byte, error) {
	n, err := d.readVarUint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.buf)) {
		return nil, errUnexpectedEnd
	}
	return d.readBytes(int(n))
}

func (d *varDecoder) readVarString() (string, error) {
	b, err := d.readVarUint8Array()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type varEncoder struct{ buf []byte }

func newVarEncoder() *varEncoder    { return &varEncoder{} }
func (e *varEncoder) bytes() []byte { return e.buf }

func (e *varEncoder) writeVarUint(v uint64) {
	for v > 0x7f {
		e.buf = append(e.buf, byte(v&0x7f)|0x80)
		v >>= 7
	}
	e.buf = append(e.buf, byte(v))
}

func (e *varEncoder) writeBytes(b []byte) { e.buf = append(e.buf, b...) }

func (e *varEncoder) writeVarUint8Array(b []byte) {
	e.writeVarUint(uint64(len(b)))
	e.writeBytes(b)
}

func (e *varEncoder) writeVarString(s string) { e.writeVarUint8Array([]byte(s)) }

// --- notes.mail.ru pipe envelope ------------------------------------------

const (
	dispatchSync      = 0
	dispatchAwareness = 1
	dispatchAuth      = 2
)

const pipeKeyLen = 37

type pipeFrame struct {
	pipeKey string
	msgType uint8
	body    []byte
}

func encodeFrame(pipeKey string, msgType uint8, body []byte) []byte {
	out := make([]byte, 0, len(pipeKey)+1+len(body))
	out = append(out, pipeKey...)
	out = append(out, msgType)
	out = append(out, body...)
	return out
}

func decodeFrame(raw []byte) (pipeFrame, error) {
	if len(raw) < pipeKeyLen+1 {
		return pipeFrame{}, errors.New("tunnel: frame too short")
	}
	if raw[0] != '$' {
		return pipeFrame{}, fmt.Errorf("tunnel: bad pipe prefix %q", raw[0])
	}
	return pipeFrame{
		pipeKey: string(raw[:pipeKeyLen]),
		msgType: raw[pipeKeyLen],
		body:    raw[pipeKeyLen+1:],
	}, nil
}

// --- sync / auth / awareness ---------------------------------------------

const (
	syncStep1 = 0
	syncStep2 = 1
)

func encodeSyncStep1(stateVector []byte) []byte {
	e := newVarEncoder()
	e.writeVarUint(syncStep1)
	e.writeVarUint8Array(stateVector)
	return e.bytes()
}

func encodeSyncStep2(update []byte) []byte {
	e := newVarEncoder()
	e.writeVarUint(syncStep2)
	e.writeVarUint8Array(update)
	return e.bytes()
}

const syncUpdate = 2 // messageYjsUpdate

func encodeSyncUpdate(update []byte) []byte {
	e := newVarEncoder()
	e.writeVarUint(syncUpdate)
	e.writeVarUint8Array(update)
	return e.bytes()
}

type syncMessage struct {
	subtype uint64
	payload []byte
}

func decodeSync(body []byte) (syncMessage, error) {
	d := newVarDecoder(body)
	subtype, err := d.readVarUint()
	if err != nil {
		return syncMessage{}, err
	}
	payload, err := d.readVarUint8Array()
	if err != nil {
		return syncMessage{}, err
	}
	return syncMessage{subtype: subtype, payload: payload}, nil
}

func encodeAuth(permission uint64, reason string) []byte {
	e := newVarEncoder()
	e.writeVarUint(permission)
	e.writeVarString(reason)
	return e.bytes()
}

func decodeAuth(body []byte) (permission uint64, reason string, err error) {
	d := newVarDecoder(body)
	permission, err = d.readVarUint()
	if err != nil {
		return 0, "", err
	}
	reason, err = d.readVarString()
	return permission, reason, err
}

type awarenessEntry struct {
	clientID  uint64
	clock     uint64
	stateJSON string
}

func encodeAwareness(entries []awarenessEntry) []byte {
	inner := newVarEncoder()
	inner.writeVarUint(uint64(len(entries)))
	for _, e := range entries {
		inner.writeVarUint(e.clientID)
		inner.writeVarUint(e.clock)
		inner.writeVarString(e.stateJSON)
	}
	outer := newVarEncoder()
	outer.writeVarUint8Array(inner.bytes())
	return outer.bytes()
}

func decodeAwareness(body []byte) ([]awarenessEntry, error) {
	d := newVarDecoder(body)
	inner, err := d.readVarUint8Array()
	if err != nil {
		return nil, err
	}
	id := newVarDecoder(inner)
	n, err := id.readVarUint()
	if err != nil {
		return nil, err
	}
	entries := make([]awarenessEntry, 0, n)
	for i := uint64(0); i < n; i++ {
		clientID, err := id.readVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := id.readVarUint()
		if err != nil {
			return nil, err
		}
		state, err := id.readVarString()
		if err != nil {
			return nil, err
		}
		entries = append(entries, awarenessEntry{clientID: clientID, clock: clock, stateJSON: state})
	}
	return entries, nil
}

var (
	emptyStateVector = []byte{0x00}
	emptyUpdate      = []byte{0x00, 0x00}
)

// --- tunnel wire format ---------------------------------------------------
//
// The notes.mail.ru awareness channel is a Yjs "presence" register: last state
// per clientID wins, bursts are debounced/coalesced, and the state is UTF-8
// round-tripped (payloads must be valid UTF-8 -> base64). So one clientID gives
// a reliable stream of at most ~one-state-worth of bytes per round-trip.
//
// SPEED comes from parallel LANES: each side publishes under K distinct virtual
// clientIDs, each an independent register. One connection's byte stream is
// striped across the K lanes (round-robin, tagged with a global seq) and
// reassembled in order at the receiver. This multiplies throughput ~K x.
//
// Per lane, per connection, per direction we run sliding-window ARQ:
//   - payload carries lane-local seq S (for the lane's cumulative ack A) and a
//     global seq G (for cross-lane reassembly);
//   - the receiver accepts a lane's contiguous S-run into a per-connection
//     reorder buffer keyed by G, then flushes the contiguous G-prefix to the
//     socket.
// The logical lane index travels in the state ("l") so lanes pair by index
// regardless of which clientID transports them.

const (
	wireVersion = 2

	idleTimeout = 45 * time.Second

	// defaults for the tunables (overridable by flags)
	defLanes     = 1
	defStateCap  = 4000
	defChunk     = 700
	defWindow    = 4
	defWriteBuf  = 512
	defRepublish = 500 * time.Millisecond
)

const (
	kindOpen  byte = 1 // payload = target "host:port" (client -> server, global seq 1)
	kindData  byte = 2 // payload = stream bytes
	kindClose byte = 3 // payload = empty
	kindAck   byte = 4 // sync flow control: payload = 8-byte BE cumulative bytes consumed
	kindPing  byte = 5 // sync heartbeat (connID 0): liveness only, no payload
	kindOpenUDP byte = 6 // open a UDP flow to target (payload = "host:port"); each frame = one datagram
)

// wire structs (JSON codec).
type wirePayload struct {
	S uint64 `json:"s"`           // lane-local seq (for ARQ)
	G uint64 `json:"g"`           // global seq (for reassembly)
	K byte   `json:"k,omitempty"` // kind
	D string `json:"d,omitempty"` // base64 payload
}

type wireConn struct {
	A uint64        `json:"a"`           // lane-local cumulative ack
	P []wirePayload `json:"p,omitempty"` // our in-flight window on this lane
}

type wireState struct {
	V int                 `json:"v"`
	L int                 `json:"l"` // logical lane index
	C map[string]wireConn `json:"c"`
}

// --- debug helpers --------------------------------------------------------

func hexPreview(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	s := hex.EncodeToString(b[:n])
	if len(b) > n {
		s += fmt.Sprintf("...(+%d)", len(b)-n)
	}
	return s
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...(+%d)", len(s)-n)
}

// --- connection state -----------------------------------------------------

type outItem struct {
	seq  uint64 // lane-local
	g    uint64 // global
	kind byte
	data []byte
}

// laneSend is one connection's outbound ARQ state on one lane.
type laneSend struct {
	outNext uint64    // next lane-local seq to assign
	outBase uint64    // highest lane-local seq the peer acked
	outWin  []outItem // assigned but unacked
	outQ    []outItem // backlog (seq unassigned)
}

type rItem struct {
	kind byte
	data []byte
}

type conn2 struct {
	id uint64

	writeCh   chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	eofCh     chan struct{}
	eofOnce   sync.Once

	connMu sync.Mutex
	netc   net.Conn

	// outbound (this side -> peer)
	gNext     uint64 // next global seq
	nextLane  int    // round-robin dispatch cursor
	ls        []laneSend
	sentClose bool

	// inbound (peer -> this side)
	lrAck   []uint64          // per-lane cumulative ack of peer's lane stream
	rNext   uint64            // next global seq to deliver to the socket
	reorder map[uint64]rItem  // globally-out-of-order chunks awaiting their turn
	opened  bool              // server: OPEN (g=1) has been processed
	pending bool              // server: created before OPEN seen
	peerClosed bool

	lastProgress time.Time

	isUDP      bool         // datagram flow: best-effort, drop on overflow, no window
	lastActive atomic.Int64 // unixnano of last UDP activity (for the idle reaper)

	// sync per-connection flow control (yamux-style window)
	flowMu     sync.Mutex
	flowCond   *sync.Cond
	flowClosed bool
	sent       uint64 // our outbound bytes handed to the send queue
	acked      uint64 // peer-reported cumulative bytes it has consumed of our stream
	consumed   uint64 // inbound bytes we've written to the local socket
	ackedPeer  uint64 // last consumed value we reported back as an ack
}

func (s *conn2) finish()    { s.closeOnce.Do(func() { close(s.closed) }) }
func (s *conn2) signalEOF() { s.eofOnce.Do(func() { close(s.eofCh) }) }

func (s *conn2) setConn(c net.Conn) {
	s.connMu.Lock()
	s.netc = c
	s.connMu.Unlock()
}

func (s *conn2) getConn() net.Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.netc
}

func (s *conn2) outboundDone() bool {
	if !s.sentClose {
		return false
	}
	for i := range s.ls {
		if len(s.ls[i].outWin) > 0 || len(s.ls[i].outQ) > 0 {
			return false
		}
	}
	return true
}

// --- bridge ---------------------------------------------------------------

type wsSock struct {
	c  *websocket.Conn
	mu sync.Mutex // serializes writes on this socket
}

type bridge struct {
	ctx      context.Context
	cancel   context.CancelFunc
	socks    []*wsSock // one or more WebSocket connections to the relay
	pipeKey  string
	isServer bool
	base64   bool
	debug    bool
	probeSync bool

	// reconnect
	dialURL    string
	dialHeader http.Header
	authToken  string
	readLimit  int64
	connected  atomic.Bool // false while a socket is reconnecting

	bytesSent atomic.Uint64 // payload bytes sent to the peer (up)
	bytesRecv atomic.Uint64 // payload bytes delivered locally (down)
	lastSendOK  atomic.Int64 // unixnano of the last successful WS write (sync)
	lastPeerSeen atomic.Int64 // unixnano we last heard anything from the peer (sync)

	// sync transport
	useSync      bool
	syncClientID uint64
	syncClock    atomic.Uint64 // next append clock
	syncOutCh    chan []byte

	syncMu       sync.Mutex // guards syncPeers + syncSentDeleted
	syncPeers    map[uint64]*peerStream
	syncSentDeleted uint64

	// lanes / identity
	lanes     int
	laneIDs   []uint64
	selfSet   map[uint64]bool
	laneClock []uint64 // touched only by publishLoop

	// tunables
	window     int
	maxData    int
	writeBuf   int
	stateCap   int
	republish  time.Duration
	flowWindow int // sync per-connection flow-control window (bytes)

	mu        sync.Mutex // guards conns + rotate + laneDirty
	conns     map[uint64]*conn2
	rotate    int
	laneDirty []bool
	wakeCh    chan struct{}

	laneWarn sync.Once
}

func (b *bridge) dbg(format string, args ...any) {
	if b.debug {
		log.Printf("[dbg] "+format, args...)
	}
}

func (b *bridge) writeFrame(sk *wsSock, msgType uint8, body []byte) error {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	frame := encodeFrame(b.pipeKey, msgType, body)
	b.dbg("TX raw  type=%d bodyLen=%d preview=%s", msgType, len(body), hexPreview(body, 48))
	// No per-write timeout: a slow relay applying backpressure is normal and
	// must not be mistaken for failure (that would drop data). A genuinely
	// dead socket is unblocked by reconnect()'s CloseNow().
	return sk.c.Write(b.ctx, websocket.MessageBinary, frame)
}

// sockForLane maps a lane to the socket that transports it.
func (b *bridge) sockForLane(lane int) *wsSock {
	return b.socks[lane%len(b.socks)]
}

func (b *bridge) sendAwareness(lane int, stateJSON string) error {
	sk := b.sockForLane(lane)
	sk.mu.Lock()
	defer sk.mu.Unlock()
	clk := b.laneClock[lane]
	b.laneClock[lane]++
	entries := []awarenessEntry{{clientID: b.laneIDs[lane], clock: clk, stateJSON: stateJSON}}
	body := encodeAwareness(entries)
	frame := encodeFrame(b.pipeKey, dispatchAwareness, body)
	b.dbg("TX aw   lane=%d clock=%d stateLen=%d state=%s", lane, clk, len(stateJSON), truncStr(stateJSON, 80))
	return sk.c.Write(b.ctx, websocket.MessageBinary, frame)
}

// markLaneDirtyLocked flags a lane for (re)publish; caller holds b.mu.
func (b *bridge) markLaneDirtyLocked(lane int) {
	b.laneDirty[lane] = true
	select {
	case b.wakeCh <- struct{}{}:
	default:
	}
}

func randID() uint64 {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return uint64(time.Now().UnixNano() & 0x7fffffff)
	}
	v := uint64(binary.BigEndian.Uint32(buf[:]))
	if v == 0 {
		v = 1
	}
	return v
}

func (b *bridge) newConn(id uint64) *conn2 {
	// Size the inbound buffer to hold a full flow window (so a sender that fills
	// its window never overflows writeCh before an ack flows back).
	bufN := b.writeBuf
	if b.flowWindow > 0 && b.maxData > 0 {
		if need := b.flowWindow/b.maxData + 8; need > bufN {
			bufN = need
		}
	}
	s := &conn2{
		id:           id,
		writeCh:      make(chan []byte, bufN),
		closed:       make(chan struct{}),
		eofCh:        make(chan struct{}),
		gNext:        1,
		rNext:        1,
		ls:           make([]laneSend, b.lanes),
		lrAck:        make([]uint64, b.lanes),
		reorder:      make(map[uint64]rItem),
		lastProgress: time.Now(),
	}
	s.flowCond = sync.NewCond(&s.flowMu)
	s.lastActive.Store(time.Now().UnixNano())
	for i := range s.ls {
		s.ls[i].outNext = 1
	}
	return s
}

// enqueueOutLocked assigns a global seq and a lane to an outbound message.
func (b *bridge) enqueueOutLocked(s *conn2, kind byte, data []byte) {
	if s.sentClose {
		return
	}
	if kind == kindClose {
		s.sentClose = true
	}
	g := s.gNext
	s.gNext++
	l := s.nextLane
	s.nextLane = (s.nextLane + 1) % b.lanes
	s.ls[l].outQ = append(s.ls[l].outQ, outItem{g: g, kind: kind, data: data})
	b.markLaneDirtyLocked(l)
}

func (b *bridge) promoteLocked(s *conn2, l int) {
	ls := &s.ls[l]
	for len(ls.outWin) < b.window && len(ls.outQ) > 0 {
		it := ls.outQ[0]
		ls.outQ = ls.outQ[1:]
		it.seq = ls.outNext
		ls.outNext++
		ls.outWin = append(ls.outWin, it)
	}
}

func (b *bridge) enqueueClose(s *conn2) {
	b.mu.Lock()
	already := s.sentClose
	if !already {
		b.enqueueOutLocked(s, kindClose, nil)
	}
	b.mu.Unlock()
}

// --- publisher ------------------------------------------------------------

func (b *bridge) publishLoop() {
	t := time.NewTicker(b.republish)
	defer t.Stop()
	for {
		onTick := false
		select {
		case <-b.ctx.Done():
			return
		case <-b.wakeCh:
		case <-t.C:
			onTick = true
		}
		b.mu.Lock()
		var lanes []int
		for l := 0; l < b.lanes; l++ {
			if onTick || b.laneDirty[l] {
				b.laneDirty[l] = false
				lanes = append(lanes, l)
			}
		}
		b.mu.Unlock()
		for _, l := range lanes {
			js := b.buildState(l)
			if js == "" {
				continue
			}
			if err := b.sendAwareness(l, js); err != nil {
				log.Printf("publish: %v", err)
				return
			}
		}
	}
}

type payloadMsg struct {
	s    uint64
	g    uint64
	k    byte
	data []byte
}

type connMsg struct {
	id       uint64
	a        uint64
	payloads []payloadMsg
}

func (b *bridge) buildState(lane int) string {
	b.mu.Lock()
	if len(b.conns) == 0 {
		b.mu.Unlock()
		return ""
	}
	msgs := make([]connMsg, 0, len(b.conns))
	for id, s := range b.conns {
		b.promoteLocked(s, lane)
		m := connMsg{id: id, a: s.lrAck[lane]}
		for _, it := range s.ls[lane].outWin {
			m.payloads = append(m.payloads, payloadMsg{s: it.seq, g: it.g, k: it.kind, data: it.data})
		}
		msgs = append(msgs, m)
	}
	rot := b.rotate
	b.rotate++
	b.mu.Unlock()

	sort.Slice(msgs, func(i, j int) bool { return msgs[i].id < msgs[j].id })
	if n := len(msgs); n > 1 {
		r := rot % n
		rotated := make([]connMsg, 0, n)
		rotated = append(rotated, msgs[r:]...)
		rotated = append(rotated, msgs[:r]...)
		msgs = rotated
	}
	return b.encodeWithLimit(lane, msgs)
}

func (b *bridge) encodeWithLimit(lane int, msgs []connMsg) string {
	for {
		s := b.encodeState(lane, msgs)
		if len(s) <= b.stateCap {
			return s
		}
		bi, pj := -1, -1
		var maxSeq uint64
		for i := range msgs {
			for j := range msgs[i].payloads {
				if msgs[i].payloads[j].s >= maxSeq {
					maxSeq = msgs[i].payloads[j].s
					bi, pj = i, j
				}
			}
		}
		if bi < 0 {
			return s
		}
		msgs[bi].payloads = append(msgs[bi].payloads[:pj], msgs[bi].payloads[pj+1:]...)
	}
}

func (b *bridge) encodeState(lane int, msgs []connMsg) string {
	if b.base64 {
		return encodeStateJSON(lane, msgs)
	}
	return encodeStateBinary(lane, msgs)
}

func (b *bridge) decodeState(s string) (int, []connMsg, bool) {
	if b.base64 {
		return decodeStateJSON(s)
	}
	return decodeStateBinary(s)
}

func encodeStateJSON(lane int, msgs []connMsg) string {
	C := make(map[string]wireConn, len(msgs))
	for _, m := range msgs {
		wc := wireConn{A: m.a}
		for _, p := range m.payloads {
			wc.P = append(wc.P, wirePayload{S: p.s, G: p.g, K: p.k, D: base64.StdEncoding.EncodeToString(p.data)})
		}
		C[strconv.FormatUint(m.id, 10)] = wc
	}
	out, err := json.Marshal(wireState{V: wireVersion, L: lane, C: C})
	if err != nil {
		return ""
	}
	return string(out)
}

func decodeStateJSON(s string) (int, []connMsg, bool) {
	if len(s) == 0 || s[0] != '{' {
		return 0, nil, false
	}
	var ws wireState
	if err := json.Unmarshal([]byte(s), &ws); err != nil || ws.V != wireVersion {
		return 0, nil, false
	}
	msgs := make([]connMsg, 0, len(ws.C))
	for idStr, wc := range ws.C {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}
		m := connMsg{id: id, a: wc.A}
		for _, p := range wc.P {
			data, derr := base64.StdEncoding.DecodeString(p.D)
			if derr != nil {
				continue
			}
			m.payloads = append(m.payloads, payloadMsg{s: p.S, g: p.G, k: p.K, data: data})
		}
		msgs = append(msgs, m)
	}
	return ws.L, msgs, true
}

// Binary codec:
//
//	[0x00][wireVersion] varuint lane, varuint nConns
//	repeat: varuint id, varuint a, varuint nPayloads,
//	        repeat: varuint s, varuint g, byte kind, varUint8Array data
func encodeStateBinary(lane int, msgs []connMsg) string {
	e := newVarEncoder()
	e.buf = append(e.buf, 0x00, byte(wireVersion))
	e.writeVarUint(uint64(lane))
	e.writeVarUint(uint64(len(msgs)))
	for _, m := range msgs {
		e.writeVarUint(m.id)
		e.writeVarUint(m.a)
		e.writeVarUint(uint64(len(m.payloads)))
		for _, p := range m.payloads {
			e.writeVarUint(p.s)
			e.writeVarUint(p.g)
			e.buf = append(e.buf, p.k)
			e.writeVarUint8Array(p.data)
		}
	}
	return string(e.bytes())
}

func decodeStateBinary(s string) (int, []connMsg, bool) {
	buf := []byte(s)
	if len(buf) < 2 || buf[0] != 0x00 || buf[1] != byte(wireVersion) {
		return 0, nil, false
	}
	d := newVarDecoder(buf[2:])
	lane, err := d.readVarUint()
	if err != nil {
		return 0, nil, false
	}
	n, err := d.readVarUint()
	if err != nil {
		return 0, nil, false
	}
	msgs := make([]connMsg, 0, n)
	for i := uint64(0); i < n; i++ {
		id, err := d.readVarUint()
		if err != nil {
			return 0, nil, false
		}
		a, err := d.readVarUint()
		if err != nil {
			return 0, nil, false
		}
		np, err := d.readVarUint()
		if err != nil {
			return 0, nil, false
		}
		m := connMsg{id: id, a: a}
		for j := uint64(0); j < np; j++ {
			seq, err := d.readVarUint()
			if err != nil {
				return 0, nil, false
			}
			g, err := d.readVarUint()
			if err != nil {
				return 0, nil, false
			}
			k, err := d.readByte()
			if err != nil {
				return 0, nil, false
			}
			data, err := d.readVarUint8Array()
			if err != nil {
				return 0, nil, false
			}
			m.payloads = append(m.payloads, payloadMsg{s: seq, g: g, k: k, data: data})
		}
		msgs = append(msgs, m)
	}
	return int(lane), msgs, true
}

// --- reconnect ------------------------------------------------------------

// reconnect redials one socket, re-auths and re-syncs, then swaps it in. It
// drops all active connections (their in-flight bytes can't be recovered
// without a full resync) so streams fail cleanly rather than corrupt, while
// the SOCKS listener stays up for new requests. Returns false only if the
// context is done.
func (b *bridge) reconnect(sk *wsSock, idx int) bool {
	b.connected.Store(false) // stop senders from hammering the dead socket
	// Close the old conn immediately WITHOUT the lock: CloseNow() drops the
	// underlying TCP without a close handshake, so any writer stuck in Write
	// on the dead socket errors out at once and releases sk.mu (which we need
	// below). sk.c is only written by this goroutine, so reading it is safe.
	if sk.c != nil {
		sk.c.CloseNow()
	}

	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		if b.ctx.Err() != nil {
			return false
		}
		ws, _, err := websocket.Dial(b.ctx, b.dialURL, &websocket.DialOptions{HTTPHeader: b.dialHeader})
		if err != nil {
			log.Printf("ws[%d] reconnect attempt %d failed: %v", idx, attempt, err)
			select {
			case <-b.ctx.Done():
				return false
			case <-time.After(backoff):
			}
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}
		ws.SetReadLimit(b.readLimit)
		if err := b.writeFrameConn(ws, dispatchAuth, encodeAuth(0, b.authToken)); err == nil {
			err = b.writeFrameConn(ws, dispatchSync, encodeSyncStep1(emptyStateVector))
		}
		if err != nil {
			log.Printf("ws[%d] reconnect handshake failed: %v", idx, err)
			ws.Close(websocket.StatusInternalError, "")
			continue
		}
		sk.mu.Lock()
		sk.c = ws
		sk.mu.Unlock()
		b.dropAllConns()
		b.connected.Store(true)
		log.Printf("ws[%d] reconnected", idx)
		return true
	}
}

// writeFrameConn writes one frame directly to a raw connection (used during the
// reconnect handshake before the socket is swapped in).
func (b *bridge) writeFrameConn(c *websocket.Conn, msgType uint8, body []byte) error {
	return c.Write(b.ctx, websocket.MessageBinary, encodeFrame(b.pipeKey, msgType, body))
}

// dropAllConns tears down every active connection cleanly.
func (b *bridge) dropAllConns() {
	b.mu.Lock()
	victims := make([]*conn2, 0, len(b.conns))
	for id, s := range b.conns {
		victims = append(victims, s)
		delete(b.conns, id)
	}
	b.mu.Unlock()
	for _, s := range victims {
		s.flowMu.Lock()
		s.flowClosed = true
		s.flowMu.Unlock()
		if s.flowCond != nil {
			s.flowCond.Broadcast()
		}
		s.finish()
		if c := s.getConn(); c != nil {
			c.Close()
		}
	}
}

// --- inbound --------------------------------------------------------------

func (b *bridge) readLoop(sk *wsSock, idx int) {
	for {
		_, raw, err := sk.c.Read(b.ctx)
		if err != nil {
			if b.ctx.Err() != nil {
				return // shutting down
			}
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				log.Printf("ws[%d] closed by server: code=%d reason=%q — reconnecting", idx, ce.Code, ce.Reason)
			} else {
				log.Printf("ws[%d] read: %v — reconnecting", idx, err)
			}
			if !b.reconnect(sk, idx) {
				b.cancel()
				return
			}
			continue
		}
		f, err := decodeFrame(raw)
		if err != nil {
			b.dbg("RX bad frame: %v", err)
			continue
		}
		switch f.msgType {
		case dispatchAuth:
			perm, reason, err := decodeAuth(f.body)
			if err != nil {
				log.Printf("auth decode: %v", err)
			} else if perm == 0 {
				log.Printf("auth REJECTED: reason=%q", reason)
			} else {
				log.Printf("auth OK: perm=%d reason=%q", perm, reason)
			}
		case dispatchSync:
			sm, err := decodeSync(f.body)
			if err != nil {
				b.dbg("RX sync decode: %v", err)
				continue
			}
			if sm.subtype == syncStep1 {
				_ = b.writeFrame(sk, dispatchSync, encodeSyncStep2(emptyUpdate))
			}
			// Sync transport: act only on LIVE updates (subtype 2); the initial
			// catch-up state (subtype 1) holds unrelated note content.
			if b.useSync && sm.subtype == syncUpdate && len(sm.payload) > 0 {
				if items, ok := decodeYjsUpdate(sm.payload); ok {
					b.handleSyncUpdate(items)
				} else if items != nil {
					b.handleSyncUpdate(items) // partial: use what parsed
				}
			}
			if b.probeSync && len(sm.payload) > 0 && (sm.subtype == syncStep2 || sm.subtype == syncUpdate) {
				items, okd := decodeYjsUpdate(sm.payload)
				log.Printf("PROBE RX sync subtype=%d payloadLen=%d decoded=%v items=%d", sm.subtype, len(sm.payload), okd, len(items))
				for _, it := range items {
					log.Printf("  PROBE item client=%d clock=%d ref=%d content=%q", it.client, it.clock, it.ref, truncStr(string(it.data), 64))
				}
			}
		case dispatchAwareness:
			entries, err := decodeAwareness(f.body)
			if err != nil {
				b.dbg("RX awareness decode: %v", err)
				continue
			}
			for _, e := range entries {
				if b.selfSet[e.clientID] || e.stateJSON == "" {
					continue
				}
				lane, msgs, ok := b.decodeState(e.stateJSON)
				if !ok {
					continue
				}
				if lane < 0 || lane >= b.lanes {
					b.laneWarn.Do(func() {
						log.Printf("WARNING: peer is using lane %d but -lanes=%d here — set the SAME -lanes on client and server, or traffic will stall", lane, b.lanes)
					})
					continue
				}
				b.handleState(lane, msgs)
			}
		default:
			b.dbg("RX unknown type=%d len=%d", f.msgType, len(f.body))
		}
	}
}

type inboundAct struct {
	s      *conn2
	kind   byte
	target string
}

func (b *bridge) handleState(lane int, msgs []connMsg) {
	var acts []inboundAct
	now := time.Now()
	touched := map[int]bool{}
	reorderLimit := b.lanes*b.window*4 + 16

	b.mu.Lock()
	for _, m := range msgs {
		s := b.conns[m.id]
		if s == nil {
			// Server creates a connection when it first hears about an unknown
			// id from the peer (the OPEN may arrive on any lane, possibly after
			// some data — we buffer until g=1 OPEN is delivered).
			if b.isServer && len(m.payloads) > 0 {
				s = b.newConn(m.id)
				s.pending = true
				b.conns[m.id] = s
			} else {
				continue
			}
		}

		// Retire the acked prefix of our window on this lane.
		ls := &s.ls[lane]
		if m.a > ls.outBase {
			ls.outBase = m.a
			kept := ls.outWin[:0]
			for _, it := range ls.outWin {
				if it.seq > m.a {
					kept = append(kept, it)
				}
			}
			ls.outWin = kept
			b.promoteLocked(s, lane)
			s.lastProgress = now
			touched[lane] = true
		}

		// Accept this lane's contiguous seq-run into the reorder buffer.
		if len(m.payloads) > 0 {
			sort.Slice(m.payloads, func(i, j int) bool { return m.payloads[i].s < m.payloads[j].s })
			for {
				advanced := false
				for _, p := range m.payloads {
					if p.s != s.lrAck[lane]+1 {
						continue
					}
					if len(s.reorder) >= reorderLimit {
						break // backpressure: stop accepting on this lane
					}
					if _, dup := s.reorder[p.g]; !dup && p.g >= s.rNext {
						s.reorder[p.g] = rItem{kind: p.k, data: p.data}
					}
					s.lrAck[lane]++
					s.lastProgress = now
					touched[lane] = true
					advanced = true
					break
				}
				if !advanced {
					break
				}
			}
		}

		// Flush the globally-contiguous prefix to the socket / actions.
		if b.flushReorderLocked(s, &acts, now) {
			// acks/state may have changed on multiple lanes via progress
		}
	}
	b.mu.Unlock()

	for _, a := range acts {
		switch a.kind {
		case kindOpen:
			go b.dialAndAttach(a.s, a.target)
		case kindClose:
			a.s.signalEOF()
		}
	}

	b.mu.Lock()
	for l := range touched {
		b.markLaneDirtyLocked(l)
	}
	b.mu.Unlock()
}

// flushReorderLocked delivers the contiguous run starting at rNext; caller holds b.mu.
func (b *bridge) flushReorderLocked(s *conn2, acts *[]inboundAct, now time.Time) bool {
	progressed := false
	for {
		it, ok := s.reorder[s.rNext]
		if !ok {
			break
		}
		switch it.kind {
		case kindOpen:
			delete(s.reorder, s.rNext)
			s.rNext++
			s.opened = true
			s.pending = false
			progressed = true
			s.lastProgress = now
			*acts = append(*acts, inboundAct{s: s, kind: kindOpen, target: string(it.data)})
		case kindData:
			select {
			case s.writeCh <- it.data:
				delete(s.reorder, s.rNext)
				s.rNext++
				progressed = true
				s.lastProgress = now
			default:
				return progressed // socket backed up; leave the rest buffered
			}
		case kindClose:
			delete(s.reorder, s.rNext)
			s.rNext++
			s.peerClosed = true
			progressed = true
			s.lastProgress = now
			*acts = append(*acts, inboundAct{s: s, kind: kindClose})
		}
	}
	return progressed
}

// --- attach / pump --------------------------------------------------------

func (b *bridge) dialAndAttach(s *conn2, target string) {
	log.Printf("[open] connID=%d -> %s", s.id, target)
	conn, err := net.Dial("tcp", target)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		b.enqueueClose(s)
		return
	}
	select {
	case <-s.closed:
		conn.Close()
		return
	default:
	}
	b.attach(s, conn)
}

func (b *bridge) attach(s *conn2, conn net.Conn) {
	s.setConn(conn)

	go func() { // writer: buffered inbound bytes -> local socket
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
								conn.Close()
								return
							}
						}
					default:
						conn.Close()
						return
					}
				}
			case data := <-s.writeCh:
				if _, err := conn.Write(data); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	go func() { // reader: local socket -> outbound backlog (striped across lanes)
		defer b.enqueueClose(s)
		buf := make([]byte, b.maxData)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				b.mu.Lock()
				b.enqueueOutLocked(s, kindData, chunk)
				b.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

// probeSender periodically appends a marker to the shared doc over sync so we
// can see (on the other side's logs) whether the relay forwards doc content.
func (b *bridge) probeSender() {
	role := "server"
	if !b.isServer {
		role = "client"
	}
	client := b.laneIDs[0]
	var clock uint64
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for n := 0; ; n++ {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		marker := fmt.Sprintf("PROBE-%s-%d", role, n)
		update := encodeBinaryAppends(client, clock, "tun", [][]byte{[]byte(marker)})
		clock++
		if err := b.writeFrame(b.socks[0], dispatchSync, encodeSyncUpdate(update)); err != nil {
			log.Printf("probe send: %v", err)
			return
		}
		log.Printf("PROBE TX sent %q (client=%d clock=%d)", marker, client, clock-1)
	}
}

func (b *bridge) clientSession(conn net.Conn, target string) {
	if len(target) > b.maxData {
		log.Printf("target too long: %s", target)
		conn.Close()
		return
	}
	id := randID()
	log.Printf("[open] connID=%d -> %s", id, target)
	s := b.newConn(id)
	b.mu.Lock()
	b.conns[id] = s
	b.enqueueOutLocked(s, kindOpen, []byte(target))
	b.mu.Unlock()
	b.attach(s, conn)
}

// --- reaper ---------------------------------------------------------------

func (b *bridge) reapLoop() {
	t := time.NewTicker(5 * time.Second)
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
		for id, s := range b.conns {
			done := s.outboundDone() && s.peerClosed && len(s.reorder) == 0
			idle := now.Sub(s.lastProgress) > idleTimeout
			if done || idle {
				delete(b.conns, id)
				dead = append(dead, s)
			}
		}
		b.mu.Unlock()
		for _, s := range dead {
			s.finish()
			if c := s.getConn(); c != nil {
				c.Close()
			}
		}
	}
}

// --- SOCKS5 (CONNECT only, no auth) --------------------------------------

func (b *bridge) serveSOCKS(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("SOCKS5 listening on %s", addr)
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
			if cmd != socksCmdConnect { // awareness transport is TCP-only
				_ = writeSocksReply(c, 0x07, net.IPv4zero, 0)
				c.Close()
				return
			}
			_ = writeSocksReply(c, 0x00, net.IPv4zero, 0)
			b.clientSession(c, target)
		}()
	}
}

const (
	socksCmdConnect = 0x01
	socksCmdUDP     = 0x03
)

// socks5Handshake negotiates the method, reads the request, and returns the
// command and target. It does NOT write the final reply — the caller does
// (CONNECT replies with 0.0.0.0:0; UDP ASSOCIATE replies with the relay addr).
func socks5Handshake(c net.Conn) (cmd byte, target string, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c, hdr[:]); err != nil {
		return 0, "", err
	}
	if hdr[0] != 0x05 {
		return 0, "", fmt.Errorf("bad version %d", hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err = io.ReadFull(c, methods); err != nil {
		return 0, "", err
	}
	if _, err = c.Write([]byte{0x05, 0x00}); err != nil {
		return 0, "", err
	}

	var req [4]byte
	if _, err = io.ReadFull(c, req[:]); err != nil {
		return 0, "", err
	}
	if req[0] != 0x05 {
		return 0, "", fmt.Errorf("bad version %d", req[0])
	}
	cmd = req[1]
	if cmd != socksCmdConnect && cmd != socksCmdUDP {
		_ = writeSocksReply(c, 0x07, net.IPv4zero, 0)
		return 0, "", fmt.Errorf("unsupported cmd %d", cmd)
	}
	var host string
	switch req[3] {
	case 0x01:
		var ip [4]byte
		if _, err = io.ReadFull(c, ip[:]); err != nil {
			return 0, "", err
		}
		host = net.IP(ip[:]).String()
	case 0x03:
		var l [1]byte
		if _, err = io.ReadFull(c, l[:]); err != nil {
			return 0, "", err
		}
		d := make([]byte, l[0])
		if _, err = io.ReadFull(c, d); err != nil {
			return 0, "", err
		}
		host = string(d)
	case 0x04:
		var ip [16]byte
		if _, err = io.ReadFull(c, ip[:]); err != nil {
			return 0, "", err
		}
		host = net.IP(ip[:]).String()
	default:
		_ = writeSocksReply(c, 0x08, net.IPv4zero, 0)
		return 0, "", fmt.Errorf("bad atyp %d", req[3])
	}
	var portb [2]byte
	if _, err = io.ReadFull(c, portb[:]); err != nil {
		return 0, "", err
	}
	port := binary.BigEndian.Uint16(portb[:])
	return cmd, net.JoinHostPort(host, fmt.Sprintf("%d", port)), nil
}

// writeSocksReply sends a SOCKS5 reply with an IPv4 BND.ADDR/PORT.
func writeSocksReply(c net.Conn, rep byte, ip net.IP, port int) error {
	v4 := ip.To4()
	if v4 == nil {
		v4 = net.IPv4zero.To4()
	}
	buf := []byte{0x05, rep, 0x00, 0x01, v4[0], v4[1], v4[2], v4[3], byte(port >> 8), byte(port)}
	_, err := c.Write(buf)
	return err
}

// --- main -----------------------------------------------------------------

type tunables struct {
	lanes     int
	sockets   int
	window    int
	maxData   int
	writeBuf  int
	stateCap  int
	republish  time.Duration
	flowWindow int
	base64     bool
	probeSync  bool
	useSync    bool
}

func main() {
	url := flag.String("url", "wss://notes.mail.ru/ws/pipe?platform=web", "live websocket URL")
	pipeKey := flag.String("pipe", "", `pipe/session key, e.g. "$650e2874-..."`)
	token := flag.String("token", "", `auth reason token`)
	cookie := flag.String("cookie", "", "Cookie header when opening the websocket")
	server := flag.Bool("server", false, "server mode: dial targets on OPEN")
	client := flag.Bool("client", false, "client mode: listen on -socks")
	socksAddr := flag.String("socks", "127.0.0.1:8888", "SOCKS5 listen addr (client mode)")
	idFlag := flag.Uint64("id", 0, "base awareness client id (default: random; top bit implied by mode)")
	useBase64 := flag.Bool("base64", false, "encode payloads as base64/JSON; if absent, send raw binary")
	probeSyncFlag := flag.Bool("probe-sync", false, "diagnostic: probe whether the sync channel forwards Yjs document content between peers")
	transport := flag.String("transport", "awareness", "data transport: 'awareness' (ARQ over presence) or 'sync' (append-only Yjs doc, faster)")
	debug := flag.Bool("debug", false, "verbose frame logging")

	lanes := flag.Int("lanes", defLanes, "parallel awareness lanes (virtual clientIDs); throughput scales ~lanes x")
	sockets := flag.Int("sockets", 1, "number of WebSocket connections to spread lanes over (1..lanes)")
	window := flag.Int("window", defWindow, "ack window: unacked chunks in flight per connection per lane")
	chunk := flag.Int("chunk", defChunk, "max raw bytes per chunk (lanes*window*base64(chunk) is bounded by lanes*state-cap)")
	writeBuf := flag.Int("writebuf", defWriteBuf, "per-connection inbound buffer depth (chunks)")
	stateCap := flag.Int("state-cap", defStateCap, "max awareness-state size in bytes the relay accepts (per lane)")
	republish := flag.Duration("republish", defRepublish, "state (re)publish/retransmit interval")
	cwnd := flag.Int("cwnd", 1<<20, "sync per-connection flow-control window in bytes (backpressure per stream)")
	flag.Parse()

	if *pipeKey == "" || *token == "" {
		log.Fatal("need -pipe and -token")
	}
	if *server == *client {
		log.Fatal("specify exactly one of -server / -client")
	}
	if *lanes < 1 {
		*lanes = 1
	}
	if *lanes > 250 {
		*lanes = 250 // lane index rides the low byte of the clientID
	}
	if *sockets < 1 {
		*sockets = 1
	}
	if *sockets > *lanes {
		*sockets = *lanes // no point having more sockets than lanes
	}
	if *window < 1 {
		*window = 1
	}
	if *chunk < 1 {
		*chunk = 1
	}
	if *transport == "sync" && *chunk > 131072 {
		log.Printf("chunk %d too large for the relay's message limit; clamping to 131072", *chunk)
		*chunk = 131072 // one chunk must fit inside one WS message (~256KB relay cap)
	}
	if *writeBuf < 1 {
		*writeBuf = 1
	}
	if *cwnd < *chunk {
		*cwnd = *chunk // window must hold at least one chunk
	}

	useSync := *transport == "sync"
	if useSync {
		// sync uses ONE socket (the doc is one shared stream; extra sockets
		// only duplicate the rebroadcast).
		*sockets = 1
		*lanes = 1
	}

	cfg := tunables{
		lanes:     *lanes,
		sockets:   *sockets,
		window:    *window,
		maxData:   *chunk,
		writeBuf:  *writeBuf,
		stateCap:   *stateCap,
		republish:  *republish,
		flowWindow: *cwnd,
		base64:     *useBase64,
		probeSync:  *probeSyncFlag,
		useSync:    useSync,
	}

	ctx := context.Background()
	if err := run(ctx, *url, *pipeKey, *token, *cookie, *socksAddr, *server, *idFlag, cfg, *debug); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run(ctx context.Context, url, pipeKey, token, cookie, socksAddr string, isServer bool, idOverride uint64, cfg tunables, debug bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var header http.Header
	if cookie != "" {
		header = http.Header{"Cookie": []string{cookie}}
	}

	const readLimit int64 = 256 << 20 // 256 MB — large catch-up states won't kill the read

	nSock := cfg.sockets
	if nSock < 1 {
		nSock = 1
	}
	log.Printf("connecting to %s (%d socket(s)) ...", url, nSock)
	socks := make([]*wsSock, 0, nSock)
	defer func() {
		for _, sk := range socks {
			sk.c.Close(websocket.StatusNormalClosure, "")
		}
	}()
	for i := 0; i < nSock; i++ {
		ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			return fmt.Errorf("dial socket %d: %w", i, err)
		}
		ws.SetReadLimit(readLimit)
		socks = append(socks, &wsSock{c: ws})
	}
	log.Printf("%d websocket(s) connected", len(socks))

	// Base clientID: 32-bit, split by mode with the top bit. Lanes ride the low
	// byte so the K virtual ids are distinct and don't cross the server/client
	// boundary.
	var base uint32
	if idOverride != 0 {
		base = uint32(idOverride)
	} else {
		var buf [4]byte
		if _, err := rand.Read(buf[:]); err != nil {
			base = 1
		} else {
			base = binary.BigEndian.Uint32(buf[:])
		}
	}
	base &^= 0x000000ff
	if isServer {
		base &^= 0x80000000
	} else {
		base |= 0x80000000
	}

	laneIDs := make([]uint64, cfg.lanes)
	selfSet := make(map[uint64]bool, cfg.lanes)
	for l := 0; l < cfg.lanes; l++ {
		id := uint64(base | uint32(l))
		if id == 0 {
			id = uint64(0x100 | uint32(l))
		}
		laneIDs[l] = id
		selfSet[id] = true
	}
	log.Printf("base clientID = 0x%08x isServer=%v lanes=%d", base, isServer, cfg.lanes)

	b := &bridge{
		ctx:        ctx,
		cancel:     cancel,
		socks:      socks,
		pipeKey:    pipeKey,
		dialURL:    url,
		dialHeader: header,
		authToken:  token,
		readLimit:  readLimit,
		isServer:   isServer,
		base64:    cfg.base64,
		debug:     debug,
		probeSync: cfg.probeSync,
		useSync:   cfg.useSync,
		lanes:     cfg.lanes,
		laneIDs:   laneIDs,
		selfSet:   selfSet,
		laneClock: make([]uint64, cfg.lanes),
		window:    cfg.window,
		maxData:   cfg.maxData,
		writeBuf:  cfg.writeBuf,
		stateCap:   cfg.stateCap,
		republish:  cfg.republish,
		flowWindow: cfg.flowWindow,
		conns:     make(map[uint64]*conn2),
		laneDirty: make([]bool, cfg.lanes),
		wakeCh:    make(chan struct{}, 1),
	}
	b.connected.Store(true)
	if b.useSync {
		b.syncClientID = laneIDs[0]
		b.syncOutCh = make(chan []byte, 1024)
		b.syncPeers = make(map[uint64]*peerStream)
	}
	log.Printf("encoding=%s lanes=%d sockets=%d window=%d chunk=%d writebuf=%d state-cap=%d republish=%s",
		map[bool]string{true: "base64/JSON", false: "raw binary"}[cfg.base64],
		b.lanes, len(b.socks), b.window, b.maxData, b.writeBuf, b.stateCap, b.republish)

	// Auth + sync handshake on every socket.
	for i, sk := range b.socks {
		if err := b.writeFrame(sk, dispatchAuth, encodeAuth(0, token)); err != nil {
			return fmt.Errorf("auth socket %d: %w", i, err)
		}
		if err := b.writeFrame(sk, dispatchSync, encodeSyncStep1(emptyStateVector)); err != nil {
			return fmt.Errorf("syncstep1 socket %d: %w", i, err)
		}
	}

	for i := range b.socks {
		go b.readLoop(b.socks[i], i)
	}

	if b.probeSync {
		log.Printf("PROBE MODE: sending markers over sync channel; watch BOTH sides' logs for 'PROBE RX ... content=\"PROBE-...\"'")
		go b.probeSender()
		<-ctx.Done()
		return nil
	}

	if b.useSync {
		log.Printf("sync transport active (append-only Yjs doc)")
		go b.syncSendLoop()
		go b.syncPruneLoop()
		go b.syncWatchdog()
		go b.syncPingLoop()
		go b.syncReapLoop()
		go b.meterLoop()
		if isServer {
			log.Printf("server ready (waiting for OPEN)")
			<-ctx.Done()
			return nil
		}
		return b.syncServeSOCKS(socksAddr)
	}

	go b.publishLoop()
	go b.reapLoop()

	if isServer {
		log.Printf("server ready (waiting for OPEN)")
		<-ctx.Done()
		return nil
	}
	return b.serveSOCKS(socksAddr)
}

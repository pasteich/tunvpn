// Package t2smobile is a gomobile wrapper around xjasonlyu/tun2socks that runs
// in-process inside an Android VpnService. It reads packets from the VPN TUN fd
// and forwards them to the local SOCKS5 proxy exposed by the tunnel binary.
//
// The upstream tunnel's SOCKS5 is TCP-only (no UDP ASSOCIATE), so this wrapper
// installs a custom proxy:
//   - TCP flows go through SOCKS CONNECT unchanged.
//   - DNS (UDP :53) is converted to DNS-over-TCP through the tunnel. All DNS is
//     FORCED to the configured upstream server (e.g. 1.1.1.1), regardless of
//     what address the app queried, so the chosen resolver is authoritative and
//     never leaks to the phone's DNS.
//   - AAAA (IPv6) queries can be answered with NODATA so apps fall back to IPv4
//     (fixes sites that hang because IPv6 doesn't survive the tunnel).
//   - Any other UDP is dropped (apps fall back to TCP).
package t2smobile

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xjasonlyu/tun2socks/v2/engine"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
)

// Start brings the engine up.
//
//	fd        - VpnService TUN descriptor
//	socksAddr - "host:port" of the local SOCKS5 (e.g. "127.0.0.1:8888")
//	mtu       - TUN MTU (0 => 1500)
//	loglevel  - debug|info|warn|error|silent
//	dnsServer - upstream DNS to force all queries to (e.g. "1.1.1.1"); empty
//	            keeps whatever address the app queried
//	blockAAAA - answer IPv6 (AAAA) queries with NODATA so apps use IPv4
func Start(fd int, socksAddr string, mtu int, loglevel string, dnsServer string, blockAAAA bool) error {
	if fd < 0 {
		return errors.New("invalid tun fd")
	}
	if socksAddr == "" {
		return errors.New("empty socks address")
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	if loglevel == "" {
		loglevel = "info"
	}
	base, err := socks5.New(socksAddr, "", "")
	if err != nil {
		unix.Close(dup)
		return err
	}

	var upstream netip.AddrPort
	haveUpstream := false
	if s := strings.TrimSpace(dnsServer); s != "" {
		if !strings.Contains(s, ":") || strings.Count(s, ":") > 1 && !strings.Contains(s, "]") {
			// bare IPv4 or IPv6 without port
			if ip, e := netip.ParseAddr(s); e == nil {
				upstream = netip.AddrPortFrom(ip, 53)
				haveUpstream = true
			}
		}
		if !haveUpstream {
			if ap, e := netip.ParseAddrPort(s); e == nil {
				upstream = ap
				haveUpstream = true
			}
		}
	}

	key := &engine.Key{
		Device:     "fd://" + strconv.Itoa(dup),
		Proxy:      "socks5://" + socksAddr,
		MTU:        mtu,
		LogLevel:   loglevel,
		UDPTimeout: 60 * time.Second,
	}
	engine.Insert(key)
	engine.Start()
	tunnel.T().SetProxy(&dnsProxy{
		base:         base,
		upstream:     upstream,
		haveUpstream: haveUpstream,
		blockAAAA:    blockAAAA,
	})
	return nil
}

// Stop tears the engine down.
func Stop() {
	engine.Stop()
}

// dnsProxy wraps a TCP-only SOCKS5 proxy, adding forced DNS-over-TCP for UDP:53.
type dnsProxy struct {
	base         proxy.Proxy
	upstream     netip.AddrPort
	haveUpstream bool
	blockAAAA    bool
}

func (p *dnsProxy) DialContext(ctx context.Context, m *M.Metadata) (net.Conn, error) {
	return p.base.DialContext(ctx, m)
}

func (p *dnsProxy) DialUDP(m *M.Metadata) (net.PacketConn, error) {
	if m.DstPort != 53 {
		return nil, errors.New("udp not supported by upstream (only DNS is tunneled over TCP)")
	}
	return &dnsPacketConn{
		p:      p,
		respCh: make(chan []byte, 16),
		done:   make(chan struct{}),
	}, nil
}

// dnsPacketConn presents a net.PacketConn to tun2socks but resolves each DNS
// query datagram over a fresh TCP connection through the tunnel.
type dnsPacketConn struct {
	p      *dnsProxy
	respCh chan []byte
	done   chan struct{}

	mu       sync.Mutex
	deadline time.Time
	lastDst  net.Addr
	closed   bool
}

func (c *dnsPacketConn) WriteTo(pkt []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	c.lastDst = addr
	c.mu.Unlock()

	query := make([]byte, len(pkt))
	copy(query, pkt)

	// Answer AAAA locally with NODATA so the app uses IPv4.
	if c.p.blockAAAA && isAAAAQuery(query) {
		resp := synthNoData(query)
		if resp != nil {
			select {
			case c.respCh <- resp:
			case <-c.done:
			}
			return len(pkt), nil
		}
	}

	go func() {
		resp, err := c.roundTrip(query, addr)
		if err != nil {
			return
		}
		select {
		case c.respCh <- resp:
		case <-c.done:
		}
	}()
	return len(pkt), nil
}

func (c *dnsPacketConn) roundTrip(query []byte, queried net.Addr) ([]byte, error) {
	var md *M.Metadata
	if c.p.haveUpstream {
		md = &M.Metadata{Network: M.TCP, DstIP: c.p.upstream.Addr(), DstPort: c.p.upstream.Port()}
	} else {
		ua, ok := queried.(*net.UDPAddr)
		if !ok {
			return nil, errors.New("bad dns addr")
		}
		ip, ok := netip.AddrFromSlice(ua.IP)
		if !ok {
			return nil, errors.New("bad dns ip")
		}
		md = &M.Metadata{Network: M.TCP, DstIP: ip.Unmap(), DstPort: uint16(ua.Port)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.p.base.DialContext(ctx, md)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(query)))
	copy(buf[2:], query)
	if _, err := conn.Write(buf); err != nil {
		return nil, err
	}

	var lenHdr [2]byte
	if _, err := io.ReadFull(conn, lenHdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenHdr[:]))
	resp := make([]byte, n)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *dnsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	dl := c.deadline
	dst := c.lastDst
	c.mu.Unlock()

	var timer <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, nil, timeoutErr{}
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}

	select {
	case resp := <-c.respCh:
		n := copy(p, resp)
		return n, dst, nil
	case <-timer:
		return 0, nil, timeoutErr{}
	case <-c.done:
		return 0, nil, io.EOF
	}
}

func (c *dnsPacketConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}

func (c *dnsPacketConn) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4zero, Port: 0} }
func (c *dnsPacketConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}
func (c *dnsPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
func (c *dnsPacketConn) SetWriteDeadline(t time.Time) error { return nil }

// --- minimal DNS message helpers -----------------------------------------

// qtype returns the QTYPE of the first question, or 0 if it can't be parsed.
func firstQType(msg []byte) uint16 {
	if len(msg) < 12 {
		return 0
	}
	qd := binary.BigEndian.Uint16(msg[4:6])
	if qd < 1 {
		return 0
	}
	pos := 12
	// skip QNAME
	for {
		if pos >= len(msg) {
			return 0
		}
		l := int(msg[pos])
		if l == 0 {
			pos++
			break
		}
		if l&0xc0 != 0 { // compression pointer (not expected in a question)
			pos += 2
			break
		}
		pos += 1 + l
	}
	if pos+2 > len(msg) {
		return 0
	}
	return binary.BigEndian.Uint16(msg[pos : pos+2])
}

func isAAAAQuery(msg []byte) bool {
	return firstQType(msg) == 28 // AAAA
}

// synthNoData turns a query into an authoritative-looking empty NOERROR reply
// (QR=1, RD copied, RA=1, ANCOUNT=0), keeping the original question section.
func synthNoData(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, len(query))
	copy(resp, query)
	// flags: set QR, RA; keep RD; RCODE=0
	rd := query[2] & 0x01
	resp[2] = 0x80 | rd // QR=1, Opcode=0, AA=0, TC=0, RD=rd
	resp[3] = 0x80      // RA=1, RCODE=0
	// ANCOUNT/NSCOUNT/ARCOUNT = 0, keep QDCOUNT
	binary.BigEndian.PutUint16(resp[6:8], 0)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	return resp
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

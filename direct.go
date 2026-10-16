package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// Direct mode lets a host's players skip the relay. The host's app opens a port on its router
// and sends a "direct" frame with that port. The relay then:
//
//  1. takes the IP address the host connected from (never one the host types in, so a host can
//     only ever publish its own address),
//  2. connects to ip:port from outside and asks for the Minecraft server-list status, which proves
//     the port really is open to the internet and a Minecraft server answers there,
//  3. if the host has a custom address, points it straight at the host (an A record and the SRV
//     record), so the same address players already use now goes direct,
//  4. keeps checking, and if the host stops answering, points the address back at the relay.
//
// The host's IP address is public in DNS while this is on (the host opted in and was told so), but
// it is never logged and never written to the relay's disk.

const (
	directProbeTimeout   = 6 * time.Second
	directEvery          = 45 * time.Second // how often a live direct host is checked again
	directFailuresToDrop = 2                // misses in a row before players are sent back to the relay
	directAskLimit       = 6                // "direct" requests per host…
	directAskWindow      = 10 * time.Minute // …per this window
	directMinPort        = 1024
	directCleanupDelay   = 2 * time.Minute // after a relay start, addresses still pointing direct are put back
	directClearRetries   = 5
	directClearRetryBase = 5 * time.Second
	maxStatusBytes       = 256 * 1024
)

// background runs f in its own goroutine, counted so tests can wait for it to finish.
func (r *relay) background(f func()) {
	r.directWG.Add(1)
	go func() {
		defer r.directWG.Done()
		f()
	}()
}

// directState is one host's Direct mode, from the moment its port was verified until it stops.
type directState struct {
	ip   string
	port int
	stop chan struct{}

	mu      sync.Mutex
	live    bool // players are being sent straight to the host (its address points at it, if it has one)
	dnsDone bool // the host's custom address, if any, has been pointed at it
	address string
}

func (st *directState) isLive() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.live
}

// ─── Is this a public address a host's router could have opened? ───────────────

var nonPublicV4 = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	} {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

// publicIPv4 reports whether ip is an ordinary public IPv4 address. Private, shared (CGNAT),
// loopback and reserved ranges are not: a host there can't be reached from outside, and an IPv6
// address can't go in an A record.
func publicIPv4(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return false
	}
	for _, n := range nonPublicV4 {
		if n.Contains(p) {
			return false
		}
	}
	return true
}

// ─── Asking a Minecraft server from outside ────────────────────────────────────

func putVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for {
		if u&^0x7f == 0 {
			return append(b, byte(u))
		}
		b = append(b, byte(u&0x7f|0x80))
		u >>= 7
	}
}

func readVarInt(r io.ByteReader) (int32, error) {
	var out uint32
	for shift := uint(0); shift < 35; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		out |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return int32(out), nil
		}
	}
	return 0, errors.New("varint too long")
}

// statusRequest is the handshake (next state: status) followed by the status request.
func statusRequest(host string, port int) []byte {
	var body []byte
	body = putVarInt(body, 0) // handshake packet
	body = putVarInt(body, 767)
	body = putVarInt(body, int32(len(host)))
	body = append(body, host...)
	body = binary.BigEndian.AppendUint16(body, uint16(port))
	body = putVarInt(body, 1)
	out := putVarInt(nil, int32(len(body)))
	out = append(out, body...)
	return append(out, 0x01, 0x00) // length 1, packet 0: status request
}

// probeMinecraft connects to ip:port and checks that a Minecraft server answers the server-list
// status request. Only the host's own address is ever probed (see the file comment).
func probeMinecraft(ctx context.Context, ip string, port int) error {
	d := net.Dialer{Timeout: directProbeTimeout}
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return errors.New("nothing answered")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(directProbeTimeout))
	if _, err := conn.Write(statusRequest("voxelport-direct-check", port)); err != nil {
		return errors.New("the connection was closed")
	}
	br := bufio.NewReader(io.LimitReader(conn, maxStatusBytes+16))
	size, err := readVarInt(br)
	if err != nil || size < 2 || size > maxStatusBytes {
		return errors.New("that is not a Minecraft server")
	}
	packet := make([]byte, size)
	if _, err := io.ReadFull(br, packet); err != nil {
		return errors.New("that is not a Minecraft server")
	}
	pr := bufio.NewReader(bytesReader(packet))
	if id, err := readVarInt(pr); err != nil || id != 0 {
		return errors.New("that is not a Minecraft server")
	}
	n, err := readVarInt(pr)
	if err != nil || n < 2 || int(n) > len(packet) {
		return errors.New("that is not a Minecraft server")
	}
	js := make([]byte, n)
	if _, err := io.ReadFull(pr, js); err != nil {
		return errors.New("that is not a Minecraft server")
	}
	var status map[string]json.RawMessage
	if json.Unmarshal(js, &status) != nil {
		return errors.New("that is not a Minecraft server")
	}
	if _, ok := status["version"]; !ok {
		if _, ok := status["description"]; !ok {
			return errors.New("that is not a Minecraft server")
		}
	}
	return nil
}

type byteSliceReader struct {
	b []byte
	i int
}

func bytesReader(b []byte) io.Reader { return &byteSliceReader{b: b} }

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// ─── The relay side ─────────────────────────────────────────────────────────────

func (r *relay) directIPOK(ip string) bool {
	if r.directAllowIP != nil {
		return r.directAllowIP(ip)
	}
	return publicIPv4(ip)
}

func (r *relay) directProbe(ctx context.Context, ip string, port int) error {
	if r.directProbeFn != nil {
		return r.directProbeFn(ctx, ip, port)
	}
	return probeMinecraft(ctx, ip, port)
}

func (r *relay) directEveryDur() time.Duration {
	if r.directEvery > 0 {
		return r.directEvery
	}
	return directEvery
}

func (r *relay) directRetryBase() time.Duration {
	if r.directRetry > 0 {
		return r.directRetry
	}
	return directClearRetryBase
}

// allowDirectAsk rate-limits how often one host can ask for Direct mode (each ask makes the relay
// connect out and may change DNS).
func (c *client) allowDirectAsk() bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.directAsks[:0]
	for _, t := range c.directAsks {
		if now.Sub(t) < directAskWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= directAskLimit {
		c.directAsks = kept
		return false
	}
	c.directAsks = append(kept, now)
	return true
}

func (c *client) directState() *directState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.direct
}

// handleDirect answers a host's "direct" frame: port 0 turns Direct mode off, anything else asks the
// relay to verify that port and switch to it. It runs in its own goroutine (probing takes seconds).
func (r *relay) handleDirect(c *client, port int) {
	if port == 0 {
		r.stopDirect(c)
		return
	}
	if port < directMinPort || port > 65535 {
		c.send(Msg{Type: "direct_error", Message: "That port can't be used for Direct mode."})
		return
	}
	if !c.allowDirectAsk() {
		c.send(Msg{Type: "direct_error", Message: "Too many tries — wait a few minutes and try again."})
		return
	}
	ip := c.sourceIP
	if !r.directIPOK(ip) {
		c.send(Msg{Type: "direct_error", Message: "Direct mode needs a public IPv4 address. This connection is behind a shared address (CGNAT), a VPN or IPv6 only, so players can't reach this PC directly."})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), directProbeTimeout+2*time.Second)
	err := r.directProbe(ctx, ip, port)
	cancel()
	if err != nil {
		c.send(Msg{Type: "direct_error", Message: fmt.Sprintf("Couldn't reach your server from the internet on port %d (%s). Check that the server is running, that the router opened the port, and that your firewall allows it.", port, err)})
		return
	}

	// Replace any earlier direct state, and publish the new one before DNS is touched, so a
	// clean-up that races with this sees that the host wants to be direct.
	st := &directState{ip: ip, port: port, stop: make(chan struct{}), live: true}
	c.mu.Lock()
	old := c.direct
	c.direct = st
	c.mu.Unlock()
	if old != nil {
		close(old.stop)
	}
	if c.isClosed() { // the tunnel closed meanwhile; its clean-up may already have run
		c.mu.Lock()
		if c.direct == st {
			c.direct = nil
		}
		c.mu.Unlock()
		close(st.stop)
		return
	}
	r.applyDirect(c, st)
	c.send(Msg{Type: "direct", IP: ip, Port: port, Address: st.address})
	log.Printf("[direct] token=%s up port=%d named=%t", c.id[:8], port, st.address != "")
	r.background(func() { r.monitorDirect(c, st) })
}

// refreshDirect tells a live direct host its address changed (it claimed or released a custom
// address while Direct mode was on), and points the new address at it.
func (r *relay) refreshDirect(c *client) {
	st := c.directState()
	if st == nil || !st.isLive() {
		return
	}
	st.mu.Lock()
	st.dnsDone, st.address = false, ""
	st.mu.Unlock()
	r.applyDirect(c, st)
	c.send(Msg{Type: "direct", IP: st.ip, Port: st.port, Address: st.address})
}

// isClosed reports whether the tunnel has been torn down.
func (c *client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// applyDirect points the host's custom address (if it has one) at the host. A DNS failure leaves the
// host verified but not switched; the monitor tries again.
func (r *relay) applyDirect(c *client, st *directState) {
	if r.names == nil {
		return
	}
	ok, err := r.names.setDirect(c.id, st.ip, st.port)
	if err != nil {
		log.Printf("[direct] token=%s dns: %v", c.id[:8], err)
		return
	}
	st.mu.Lock()
	if ok {
		st.dnsDone = true
		st.address = r.names.address(r.names.nameFor(c.id))
	}
	st.mu.Unlock()
}

// monitorDirect keeps checking a live direct host. After directFailuresToDrop misses in a row the
// address goes back to the relay (and the host is told); when the host answers again, direct resumes.
func (r *relay) monitorDirect(c *client, st *directState) {
	t := time.NewTicker(r.directEveryDur())
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-st.stop:
			return
		case <-c.closed:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), directProbeTimeout+2*time.Second)
		err := r.directProbe(ctx, st.ip, st.port)
		cancel()
		if err == nil {
			fails = 0
			st.mu.Lock()
			live, done := st.live, st.dnsDone
			st.mu.Unlock()
			switch {
			case !live:
				st.mu.Lock()
				st.live = true
				st.mu.Unlock()
				r.applyDirect(c, st)
				c.send(Msg{Type: "direct", IP: st.ip, Port: st.port, Address: st.address})
				log.Printf("[direct] token=%s back", c.id[:8])
			case !done:
				r.applyDirect(c, st) // the address couldn't be switched earlier
			}
			continue
		}
		fails++
		st.mu.Lock()
		live := st.live
		st.mu.Unlock()
		if fails < directFailuresToDrop || !live {
			continue
		}
		st.mu.Lock()
		st.live, st.dnsDone, st.address = false, false, ""
		st.mu.Unlock()
		r.clearDirectDNS(c.id)
		c.send(Msg{Type: "direct", Port: 0})
		c.send(Msg{Type: "direct_error", Message: "Direct mode stopped working, so players are using the relay again. It switches back by itself when your server is reachable."})
		log.Printf("[direct] token=%s down: not reachable", c.id[:8])
	}
}

// stopDirect ends Direct mode for a host (it turned it off, or its tunnel closed): players go back
// to the relay and the host's IP leaves DNS.
func (r *relay) stopDirect(c *client) {
	c.mu.Lock()
	st := c.direct
	c.direct = nil
	c.mu.Unlock()
	if st == nil {
		return
	}
	close(st.stop)
	r.clearDirectDNS(c.id)
	if !c.isClosed() {
		c.send(Msg{Type: "direct", Port: 0})
	}
	log.Printf("[direct] token=%s off", c.id[:8])
}

// clearDirectDNS puts the host's address back on the relay, retrying a few times (DNS can fail
// briefly). It does nothing if a live tunnel of that host wants to be direct again by then.
func (r *relay) clearDirectDNS(id string) {
	if r.names == nil {
		return
	}
	r.background(func() {
		wait := r.directRetryBase()
		for i := 0; i < directClearRetries; i++ {
			err := r.names.clearDirectUnless(id, func() bool { return r.hostWantsDirect(id) })
			if err == nil {
				return
			}
			log.Printf("[direct] token=%s clear dns: %v", id[:8], err)
			time.Sleep(wait)
			wait *= 3
		}
	})
}

// hostWantsDirect reports whether id has a live tunnel in live Direct mode.
func (r *relay) hostWantsDirect(id string) bool {
	r.mu.Lock()
	c := r.clients[id]
	r.mu.Unlock()
	if c == nil {
		return false
	}
	st := c.directState()
	return st != nil && st.isLive()
}

// startDirectCleanup, run once at start-up, puts back any address a previous run left pointing at a
// host. Hosts that are still online reconnect and ask for Direct mode again, which wins.
func (r *relay) startDirectCleanup() {
	if r.names == nil {
		return
	}
	wait := directCleanupDelay
	if r.directCleanup > 0 {
		wait = r.directCleanup
	}
	time.AfterFunc(wait, func() {
		for _, id := range r.names.directIDs() {
			r.clearDirectDNS(id)
		}
	})
}

// ─── Name store ─────────────────────────────────────────────────────────────────

// setDirect points clientID's custom address straight at ip:port. It returns false (and does nothing)
// when the host has no custom address. The IP is only sent to DNS, never stored.
func (s *nameStore) setDirect(clientID, ip string, port int) (bool, error) {
	s.dnsMu.Lock()
	defer s.dnsMu.Unlock()
	s.mu.Lock()
	e := s.byClient[clientID]
	var recs []string
	var name string
	if e != nil {
		recs, name = append([]string(nil), e.Records...), e.Name
	}
	s.mu.Unlock()
	if e == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), nameDNSTimeout)
	defer cancel()
	out, err := s.dns.SetDirect(ctx, recs, name, ip, port)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if cur := s.byClient[clientID]; cur == e {
		e.Records, e.Direct = out, true
		s.saveLocked()
	}
	s.mu.Unlock()
	return true, nil
}

// clearDirectUnless puts clientID's address back on the relay, unless stillWanted (checked under the
// DNS lock, so it can't race a new setDirect) says the host is direct again.
func (s *nameStore) clearDirectUnless(clientID string, stillWanted func() bool) error {
	s.dnsMu.Lock()
	defer s.dnsMu.Unlock()
	s.mu.Lock()
	e := s.byClient[clientID]
	var recs []string
	var name string
	var relayPort int
	direct := false
	if e != nil {
		recs, name, relayPort, direct = append([]string(nil), e.Records...), e.Name, e.Port, e.Direct
	}
	s.mu.Unlock()
	if e == nil || !direct || stillWanted() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), nameDNSTimeout)
	defer cancel()
	out, err := s.dns.ClearDirect(ctx, recs, name, relayPort)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if cur := s.byClient[clientID]; cur == e {
		e.Records, e.Direct = out, false
		s.saveLocked()
	}
	s.mu.Unlock()
	return nil
}

// directIDs lists the clients whose address currently points at a host.
func (s *nameStore) directIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, e := range s.byClient {
		if e.Direct {
			ids = append(ids, id)
		}
	}
	return ids
}

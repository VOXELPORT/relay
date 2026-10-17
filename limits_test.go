package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Tests for the per-source limits (items L1–L3 of the capacity change): the address a limit is counted under, and
// separate, CGNAT-friendly limits for IPv4 and for IPv6 /64s.

func TestL1_LimitKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"203.0.113.5", "203.0.113.5"},
		{"  203.0.113.5 ", "203.0.113.5"},
		{"127.0.0.1", "127.0.0.1"},
		{"2401:4900:1c2a:5f00::1", "2401:4900:1c2a:5f00::/64"},
		{"2401:4900:1c2a:5f00:dead:beef:1234:5678", "2401:4900:1c2a:5f00::/64"},
		{"2401:4900:1C2A:5F00::1", "2401:4900:1c2a:5f00::/64"}, // case does not matter
		{"2401:4900:1c2a:5f01::1", "2401:4900:1c2a:5f01::/64"}, // the next /64 is a different key
		{"::1", "::/64"},
		{"::ffff:203.0.113.5", "203.0.113.5"}, // IPv4-mapped IPv6 is the IPv4 address
		{"::ffff:cb00:7105", "203.0.113.5"},   // ...in either spelling
		{"fe80::1%eth0", "fe80::/64"},         // a zone is not part of the address
		{"", ""},                              // garbage comes back unchanged, so it can never merge with a real key
		{"not-an-ip", "not-an-ip"},
		{"203.0.113.256", "203.0.113.256"},
		{"2401:4900::zz", "2401:4900::zz"},
	}
	for _, c := range cases {
		if got := limitKey(c.in); got != c.want {
			t.Errorf("limitKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestL1_TwoAddressesInOneSlash64ShareAKey(t *testing.T) {
	a, b := limitKey("2401:4900:1c2a:5f00::1"), limitKey("2401:4900:1c2a:5f00:ffff:ffff:ffff:fffe")
	if a != b {
		t.Fatalf("same /64 must be one key: %q vs %q", a, b)
	}
	if limitKey("2401:4900:1c2a:5f00::1") == limitKey("2401:4900:1c2a:5f10::1") {
		t.Fatal("different /64s must be different keys")
	}
	if isIPv6Key(limitKey("203.0.113.5")) || !isIPv6Key(limitKey("2401:4900:1c2a:5f00::1")) || isIPv6Key("not-an-ip") {
		t.Fatal("only a /64 key is an IPv6 key")
	}
}

// dialWSFrom opens a host connection that the relay will attribute to ip (through X-Forwarded-For, which a loopback
// peer is trusted to send).
func dialWSFrom(t *testing.T, base, ip string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, http.Header{"X-Forwarded-For": {ip}})
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return ws
}

// registerFrom tries to register token from ip, and returns the answer ("port" or "error").
func registerFrom(t *testing.T, base string, ip, token string) (*websocket.Conn, Msg) {
	t.Helper()
	ws := dialWSFrom(t, base, ip)
	writeMsg(t, ws, Msg{Type: "register", Token: token})
	return ws, readMsg(t, ws)
}

func limitsConfig() relayConfig {
	cfg := testConfig()
	cfg.MinPort, cfg.MaxPort = 27600, 27799
	return cfg
}

func TestL2_TunnelsPerIPv4Address(t *testing.T) {
	cfg := limitsConfig()
	cfg.MaxTunnelsPerIP = 25
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	for i := 0; i < 25; i++ {
		ws, m := registerFrom(t, srv.URL, "203.0.113.50", fmt.Sprintf("vp_v4tunnel_%012d", i))
		defer ws.Close()
		if m.Type != "port" {
			t.Fatalf("tunnel %d from one IPv4 address should be allowed: %+v", i+1, m)
		}
	}
	ws, m := registerFrom(t, srv.URL, "203.0.113.50", "vp_v4tunnel_extra000000")
	defer ws.Close()
	if m.Type != "error" || !strings.Contains(m.Message, "Too many VoxelPort servers are online from your network right now (limit 25)") || !strings.Contains(m.Message, "mobile data, try Wi-Fi") {
		t.Fatalf("the 26th must be refused with a plain message, got %+v", m)
	}
	ws2, m2 := registerFrom(t, srv.URL, "203.0.113.51", "vp_v4tunnel_other00000")
	defer ws2.Close()
	if m2.Type != "port" {
		t.Fatalf("a different IPv4 address is unaffected: %+v", m2)
	}
}

func TestL2_TunnelsPerIPv6Prefix(t *testing.T) {
	srv, _ := newTestServerWithConfig(limitsConfig())
	defer srv.Close()
	for i := 0; i < 5; i++ {
		// five different addresses, all in one /64
		ws, m := registerFrom(t, srv.URL, fmt.Sprintf("2401:4900:1c2a:5f00::%x", i+1), fmt.Sprintf("vp_v6tunnel_%012d", i))
		defer ws.Close()
		if m.Type != "port" {
			t.Fatalf("tunnel %d from one /64 should be allowed: %+v", i+1, m)
		}
	}
	ws, m := registerFrom(t, srv.URL, "2401:4900:1c2a:5f00:aaaa::1", "vp_v6tunnel_extra000000")
	defer ws.Close()
	if m.Type != "error" || !strings.Contains(m.Message, "(limit 5)") {
		t.Fatalf("the 6th from one /64 must be refused, got %+v", m)
	}
	ws2, m2 := registerFrom(t, srv.URL, "2401:4900:1c2a:5f01::1", "vp_v6tunnel_other00000")
	defer ws2.Close()
	if m2.Type != "port" {
		t.Fatalf("another /64 is unaffected: %+v", m2)
	}
}

func TestL2_AnIPv4MappedAddressCountsAsIPv4(t *testing.T) {
	cfg := limitsConfig()
	cfg.MaxTunnelsPerIP = 2
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws1, m1 := registerFrom(t, srv.URL, "203.0.113.60", "vp_mapped_aaaaaaaaaaaa")
	defer ws1.Close()
	ws2, m2 := registerFrom(t, srv.URL, "::ffff:203.0.113.60", "vp_mapped_bbbbbbbbbbbb")
	defer ws2.Close()
	if m1.Type != "port" || m2.Type != "port" {
		t.Fatalf("two tunnels: %+v %+v", m1, m2)
	}
	ws3, m3 := registerFrom(t, srv.URL, "::ffff:cb00:713c", "vp_mapped_cccccccccccc")
	defer ws3.Close()
	if m3.Type != "error" {
		t.Fatalf("the same address in any spelling shares one limit, got %+v", m3)
	}
}

func TestL2_TunnelCountsComeBackWhenTunnelsClose(t *testing.T) {
	cfg := limitsConfig()
	cfg.MaxTunnelsPerIP6 = 1
	srv, r := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws, m := registerFrom(t, srv.URL, "2401:4900:1c2a:5f00::7", "vp_closeing_aaaaaaaaaaaa")
	if m.Type != "port" {
		t.Fatal(m)
	}
	ws.Close()
	waitUntil(t, "the /64's tunnel count to drop", func() bool { return r.tunnelsForIP("2401:4900:1c2a:5f00::9") == 0 })
	ws2, m2 := registerFrom(t, srv.URL, "2401:4900:1c2a:5f00::8", "vp_closeing_bbbbbbbbbbbb")
	defer ws2.Close()
	if m2.Type != "port" {
		t.Fatalf("the slot is free again: %+v", m2)
	}
}

func TestL2_RegistrationLimitPerFamily(t *testing.T) {
	cfg := limitsConfig()
	cfg.RegLimitIP, cfg.RegLimitIP6 = 6, 4
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()

	attempt := func(ip string, i int) Msg {
		ws, m := registerFrom(t, srv.URL, ip, fmt.Sprintf("vp_regfam_%012d", i))
		ws.Close()
		return m
	}
	for i := 0; i < 6; i++ {
		if m := attempt("203.0.113.70", i); m.Type != "port" {
			t.Fatalf("IPv4 attempt %d: %+v", i+1, m)
		}
	}
	if m := attempt("203.0.113.70", 100); m.Type != "error" || !strings.Contains(m.Message, "Too many connection attempts from your network") {
		t.Fatalf("the 7th IPv4 attempt in a minute must be refused with a plain message, got %+v", m)
	}
	if m := attempt("203.0.113.71", 101); m.Type != "port" {
		t.Fatalf("another IPv4 address is unaffected: %+v", m)
	}
	for i := 0; i < 4; i++ {
		if m := attempt(fmt.Sprintf("2401:4900:1c2a:5f00::%x", i+1), 200+i); m.Type != "port" {
			t.Fatalf("IPv6 attempt %d: %+v", i+1, m)
		}
	}
	if m := attempt("2401:4900:1c2a:5f00::ff", 300); m.Type != "error" {
		t.Fatalf("the 5th attempt from one /64 must be refused, got %+v", m)
	}
	if m := attempt("2401:4900:1c2a:5f01::1", 301); m.Type != "port" {
		t.Fatalf("another /64 is unaffected: %+v", m)
	}
}

func TestL2_DefaultsAreThoseInTheBrief(t *testing.T) {
	r := newRelay(testConfigWithoutLimits())
	if r.maxTunnelsPerIP != 25 || r.maxTunnelsPerIP6 != 5 || r.maxPlayersPerIPTunnel != 32 || r.maxPlayersPerIPTunnel6 != 16 {
		t.Fatalf("defaults: tunnels %d/%d players %d/%d", r.maxTunnelsPerIP, r.maxTunnelsPerIP6, r.maxPlayersPerIPTunnel, r.maxPlayersPerIPTunnel6)
	}
	if r.regLimiter.max4 != 30 || r.regLimiter.max6 != 10 {
		t.Fatalf("registration defaults %d/%d", r.regLimiter.max4, r.regLimiter.max6)
	}
}

func testConfigWithoutLimits() relayConfig {
	return relayConfig{MinPort: 27800, MaxPort: 27810}
}

// playersFixture registers a tunnel and returns its address and the host's socket.
func playersFixture(t *testing.T, cfg relayConfig) (host string, port int, ws *websocket.Conn, closeAll func()) {
	t.Helper()
	srv, _ := newTestServerWithConfig(cfg)
	ws = dialWS(t, srv)
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_playerfix_123456789"})
	m := readMsg(t, ws)
	if m.Type != "port" {
		t.Fatalf("register: %+v", m)
	}
	return "", m.Port, ws, func() { ws.Close(); srv.Close() }
}

func TestL2_PlayersPerIPv4AddressPerTunnel(t *testing.T) {
	cfg := limitsConfig()
	cfg.MaxPlayersPerIPTunnel = 32
	_, port, ws, closeAll := playersFixture(t, cfg)
	defer closeAll()
	addr := net.JoinHostPort("127.0.0.1", itoa(port))
	var players []net.Conn
	defer func() {
		for _, p := range players {
			p.Close()
		}
	}()
	for i := 0; i < 32; i++ {
		p, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("player %d: %v", i+1, err)
		}
		players = append(players, p)
		if m := readMsg(t, ws); m.Type != "connect" {
			t.Fatalf("player %d: expected connect, got %+v", i+1, m)
		}
	}
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := extra.Read(make([]byte, 8)); err == nil {
		t.Fatalf("the 33rd player from one IPv4 address must be rejected, got %d bytes", n)
	}
}

func TestL2_PlayersPerIPv6PrefixPerTunnel(t *testing.T) {
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback on this machine")
	}
	probe.Close()
	cfg := limitsConfig()
	_, port, ws, closeAll := playersFixture(t, cfg)
	defer closeAll()
	addr := net.JoinHostPort("::1", itoa(port))
	var players []net.Conn
	defer func() {
		for _, p := range players {
			p.Close()
		}
	}()
	for i := 0; i < 16; i++ {
		p, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("player %d: %v", i+1, err)
		}
		players = append(players, p)
		if m := readMsg(t, ws); m.Type != "connect" {
			t.Fatalf("player %d: expected connect, got %+v", i+1, m)
		}
	}
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := extra.Read(make([]byte, 8)); err == nil {
		t.Fatalf("the 17th player from one /64 must be rejected, got %d bytes", n)
	}
	// IPv4 players have their own, separate allowance on the same tunnel.
	v4, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer v4.Close()
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatalf("an IPv4 player is not held back by the IPv6 count: %+v", m)
	}
}

func TestL2_ThePlayerCountIsReleasedWhenAPlayerLeaves(t *testing.T) {
	cfg := limitsConfig()
	cfg.MaxPlayersPerIPTunnel = 1
	_, port, ws, closeAll := playersFixture(t, cfg)
	defer closeAll()
	addr := net.JoinHostPort("127.0.0.1", itoa(port))
	p1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatal(m)
	}
	p1.Close()
	readUntil(t, ws, "close")
	p2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatalf("the slot is free again: %+v", m)
	}
}

// ─── The per-source player count, without needing many source addresses ──────

type fakeAddrConn struct {
	net.Conn
	addr string
}

func (f fakeAddrConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(f.addr), Port: 40000}
}
func (f fakeAddrConn) Close() error { return nil }

func testClient(max4, max6 int) *client {
	r := &relay{maxPlayersPerIPTunnel: max4, maxPlayersPerIPTunnel6: max6}
	return &client{rel: r, players: map[string]*playerConn{}, ipCounts: map[string]int{}, blocked: map[string]bool{}}
}

func admit(c *client, id, host string) bool {
	return c.admitPlayer(id, &playerConn{conn: fakeAddrConn{addr: host}}, host)
}

func TestL2_ManyAddressesInOneIPv6PrefixShareTheirPlayerAllowance(t *testing.T) {
	c := testClient(32, 16)
	for i := 0; i < 16; i++ {
		// sixteen different addresses in one /64, one player each
		if !admit(c, fmt.Sprintf("p%d", i), fmt.Sprintf("2401:4900:1c2a:5f00::%x", i+1)) {
			t.Fatalf("player %d should fit", i+1)
		}
	}
	if admit(c, "extra", "2401:4900:1c2a:5f00:9999::1") {
		t.Fatal("a 17th player from the same /64 must be refused, whatever address it uses")
	}
	if !admit(c, "other-net", "2401:4900:1c2a:5f01::1") {
		t.Fatal("a different /64 has its own allowance")
	}
	if !admit(c, "v4", "203.0.113.5") {
		t.Fatal("IPv4 players have their own allowance")
	}
}

func TestL2_IPv4PlayersGetTheLargerAllowance(t *testing.T) {
	c := testClient(32, 16)
	for i := 0; i < 32; i++ {
		if !admit(c, fmt.Sprintf("p%d", i), "203.0.113.5") {
			t.Fatalf("IPv4 player %d should fit (carrier-grade NAT shares one address)", i+1)
		}
	}
	if admit(c, "extra", "::ffff:203.0.113.5") {
		t.Fatal("the 33rd player from one IPv4 address (even spelled as IPv4-mapped IPv6) must be refused")
	}
}

func TestL2_PlayerSlotsAreGivenBackPerKey(t *testing.T) {
	c := testClient(32, 2)
	if !admit(c, "a", "2401:4900:1c2a:5f00::1") || !admit(c, "b", "2401:4900:1c2a:5f00::2") {
		t.Fatal("two players fit")
	}
	if admit(c, "c", "2401:4900:1c2a:5f00::3") {
		t.Fatal("a third does not")
	}
	c.mu.Lock()
	delete(c.players, "a")
	c.releasePlayerCountLocked("2401:4900:1c2a:5f00::1")
	c.mu.Unlock()
	if !admit(c, "c", "2401:4900:1c2a:5f00::3") {
		t.Fatal("once one leaves, the slot is free for any address in the /64")
	}
}

func TestL2_ABlockedAddressIsStillRefusedByItsExactAddress(t *testing.T) {
	c := testClient(32, 16)
	c.blocked["203.0.113.9"] = true
	if admit(c, "x", "203.0.113.9") {
		t.Fatal("a blocked address is refused")
	}
	if !admit(c, "y", "203.0.113.10") {
		t.Fatal("others are not")
	}
}

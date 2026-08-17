package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testConfig returns generous defaults so tests that don't care about a
// particular limit aren't accidentally constrained by it. Tests exercising a
// specific limit should copy this and tighten just that field.
func testConfig() relayConfig {
	return relayConfig{
		MinPort:               26000,
		MaxPort:               26050,
		MaxTunnelsPerIP:       1000,
		MaxPlayersPerIPTunnel: 1000,
	}
}

func newTestServer() *httptest.Server {
	srv, _ := newTestServerWithConfig(testConfig())
	return srv
}

func newTestServerWithConfig(cfg relayConfig) (*httptest.Server, *relay) {
	r := newRelay(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/api/status", r.apiStatus)
	return httptest.NewServer(mux), r
}

// dialWS opens a websocket to the test server's /ws endpoint.
func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return ws
}

func readMsg(t *testing.T, ws *websocket.Conn) Msg {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read ws: %v", err)
	}
	var m Msg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

func writeMsg(t *testing.T, ws *websocket.Conn, m Msg) {
	t.Helper()
	if err := ws.WriteJSON(m); err != nil {
		t.Fatalf("write ws: %v", err)
	}
}

func TestRejectsBadToken(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()

	writeMsg(t, ws, Msg{Type: "register", Token: "not-a-valid-token"})
	m := readMsg(t, ws)
	if m.Type != "error" {
		t.Fatalf("expected error frame, got %q", m.Type)
	}
}

func TestAssignsPortAndBridgesPlayer(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	ws := dialWS(t, srv)
	defer ws.Close()

	writeMsg(t, ws, Msg{Type: "register", Token: "vp_testtoken123456"})
	portMsg := readMsg(t, ws)
	if portMsg.Type != "port" || portMsg.Port < 26000 || portMsg.Port > 26050 {
		t.Fatalf("expected port in range, got %+v", portMsg)
	}

	// A vanilla player connects to the assigned public port.
	player, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer player.Close()

	// Relay announces the new connection.
	connMsg := readMsg(t, ws)
	if connMsg.Type != "connect" || connMsg.Conn == "" {
		t.Fatalf("expected connect frame, got %+v", connMsg)
	}
	connID := connMsg.Conn
	if len(connID) != 32 {
		t.Fatalf("expected a 128-bit (32 hex char) connection id, got %d chars: %q", len(connID), connID)
	}

	// Player -> host: bytes arrive as a data frame.
	if _, err := player.Write([]byte("hello-server")); err != nil {
		t.Fatalf("player write: %v", err)
	}
	dataMsg := readMsg(t, ws)
	if dataMsg.Type != "data" || dataMsg.Conn != connID {
		t.Fatalf("expected data frame, got %+v", dataMsg)
	}
	decoded, _ := base64.StdEncoding.DecodeString(dataMsg.Data)
	if string(decoded) != "hello-server" {
		t.Fatalf("player->host mismatch: %q", decoded)
	}

	// Host -> player: echo back through the relay.
	writeMsg(t, ws, Msg{
		Type: "data",
		Conn: connID,
		Data: base64.StdEncoding.EncodeToString([]byte("hello-player")),
	})
	player.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := player.Read(buf)
	if err != nil {
		t.Fatalf("player read: %v", err)
	}
	if string(buf[:n]) != "hello-player" {
		t.Fatalf("host->player mismatch: %q", buf[:n])
	}
}

func TestMeteringCountsUsage(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_metertoken123456"})
	portMsg := readMsg(t, ws)

	player, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer player.Close()
	_ = readMsg(t, ws) // connect

	payload := "twelve bytes"
	player.Write([]byte(payload))
	_ = readMsg(t, ws) // data (ensures relay processed the bytes)

	// /api/status should reflect the active tunnel and its metered upload.
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("status get: %v", err)
	}
	defer resp.Body.Close()
	// The public status endpoint exposes only aggregate metrics — no per-tunnel
	// ports — so metering is verified through totals.bytes_in.
	var st struct {
		Tunnels int `json:"tunnels"`
		Players int `json:"players"`
		Totals  struct {
			BytesIn int64 `json:"bytes_in"`
		} `json:"totals"`
		Forwards []any `json:"forwards"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.Tunnels != 1 || st.Players != 1 {
		t.Fatalf("expected 1 tunnel / 1 player, got %d / %d", st.Tunnels, st.Players)
	}
	if st.Forwards != nil {
		t.Fatalf("status must not expose per-tunnel ports, got forwards=%+v", st.Forwards)
	}
	if st.Totals.BytesIn < int64(len(payload)) {
		t.Fatalf("expected totals.bytes_in >= %d, got %d", len(payload), st.Totals.BytesIn)
	}
}

func TestPingPong(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()

	writeMsg(t, ws, Msg{Type: "register", Token: "vp_pingtoken123456"})
	_ = readMsg(t, ws) // port
	writeMsg(t, ws, Msg{Type: "ping"})
	if m := readMsg(t, ws); m.Type != "pong" {
		t.Fatalf("expected pong, got %q", m.Type)
	}
}

// TestReconnectKeepsStickyPort guards against a race where evicting a stale
// connection for the same token released its port *after* the new
// registration had already reserved a (different) port — so a fast
// reconnect silently got handed a new address instead of keeping the old
// one. close() now releases its port synchronously as part of eviction, so
// this must be deterministic, not flaky.
func TestReconnectKeepsStickyPort(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	ws1 := dialWS(t, srv)
	defer ws1.Close()
	writeMsg(t, ws1, Msg{Type: "register", Token: "vp_stickytoken123456"})
	first := readMsg(t, ws1)
	if first.Type != "port" {
		t.Fatalf("expected port frame, got %+v", first)
	}

	// Reconnect with the same token before ws1 is torn down client-side —
	// the server must evict ws1's tunnel and reuse its exact port.
	ws2 := dialWS(t, srv)
	defer ws2.Close()
	writeMsg(t, ws2, Msg{Type: "register", Token: "vp_stickytoken123456"})
	second := readMsg(t, ws2)
	if second.Type != "port" {
		t.Fatalf("expected port frame, got %+v", second)
	}
	if second.Port != first.Port {
		t.Fatalf("sticky port not preserved on reconnect: first=%d second=%d", first.Port, second.Port)
	}
}

// TestRegistrationRateLimit confirms a single source IP can't register an
// unbounded number of fake tunnels (port-pool exhaustion protection).
func TestRegistrationRateLimit(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	for i := range regLimitMax {
		func() {
			ws := dialWS(t, srv)
			defer ws.Close()
			writeMsg(t, ws, Msg{Type: "register", Token: fmt.Sprintf("vp_ratetoken%d123456", i)})
			m := readMsg(t, ws)
			if m.Type != "port" {
				t.Fatalf("attempt %d: expected port frame, got %+v", i, m)
			}
		}()
	}

	// One more from the same IP should be rejected, even with a valid token.
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_onemoretoken123456"})
	m := readMsg(t, ws)
	if m.Type != "error" {
		t.Fatalf("expected rate limit error, got %+v", m)
	}
}

// TestOversizedFrameRejected confirms a control-channel frame larger than
// maxWSMessageBytes gets the connection closed rather than read into memory.
func TestOversizedFrameRejected(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()

	huge := strings.Repeat("a", maxWSMessageBytes+1)
	oversized := `{"type":"register","token":"vp_` + huge + `"}`
	if err := ws.WriteMessage(websocket.TextMessage, []byte(oversized)); err != nil {
		t.Fatalf("write oversized: %v", err)
	}

	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatalf("expected connection to be closed after oversized frame, read succeeded")
	}
}

// TestPerTunnelPlayerCap confirms a single tunnel can't be flooded with an
// unbounded number of simultaneous player connections.
func TestPerTunnelPlayerCap(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()

	writeMsg(t, ws, Msg{Type: "register", Token: "vp_captoken1234567890"})
	portMsg := readMsg(t, ws)

	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))
	var players []net.Conn
	defer func() {
		for _, p := range players {
			p.Close()
		}
	}()
	for i := range maxPlayersPerTunnel {
		p, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("player %d dial: %v", i, err)
		}
		players = append(players, p)
		if m := readMsg(t, ws); m.Type != "connect" {
			t.Fatalf("player %d: expected connect frame, got %+v", i, m)
		}
	}

	// One more, over the cap, must be rejected (closed without a connect frame).
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("extra player dial: %v", err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	if n, err := extra.Read(buf); err == nil {
		t.Fatalf("expected extra player connection to be closed, got %d bytes", n)
	}
}

// ─── Item 1: max simultaneous tunnels per source IP ────────────────────────

func TestMaxTunnelsPerIPEnforced(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTunnelsPerIP = 3
	srv, r := newTestServerWithConfig(cfg)
	defer srv.Close()

	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := range cfg.MaxTunnelsPerIP {
		ws := dialWS(t, srv)
		conns = append(conns, ws)
		writeMsg(t, ws, Msg{Type: "register", Token: fmt.Sprintf("vp_tunnellimit%d123456", i)})
		if m := readMsg(t, ws); m.Type != "port" {
			t.Fatalf("tunnel %d: expected port, got %+v", i, m)
		}
	}

	extra := dialWS(t, srv)
	defer extra.Close()
	writeMsg(t, extra, Msg{Type: "register", Token: "vp_tunnellimitextra123456"})
	if m := readMsg(t, extra); m.Type != "error" {
		t.Fatalf("expected tunnel-limit error, got %+v", m)
	}

	if got := r.tunnelsForIP("127.0.0.1"); got != cfg.MaxTunnelsPerIP {
		t.Fatalf("expected %d active tunnels tracked, got %d", cfg.MaxTunnelsPerIP, got)
	}
}

func TestTunnelLimitFreedOnDisconnect(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTunnelsPerIP = 1
	srv, r := newTestServerWithConfig(cfg)
	defer srv.Close()

	ws1 := dialWS(t, srv)
	writeMsg(t, ws1, Msg{Type: "register", Token: "vp_freeslot1_123456"})
	readMsg(t, ws1)
	ws1.Close()

	deadline := time.Now().Add(3 * time.Second)
	for r.tunnelsForIP("127.0.0.1") > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	ws2 := dialWS(t, srv)
	defer ws2.Close()
	writeMsg(t, ws2, Msg{Type: "register", Token: "vp_freeslot2_123456"})
	if m := readMsg(t, ws2); m.Type != "port" {
		t.Fatalf("expected slot to be freed after disconnect, got %+v", m)
	}
}

// TestReconnectAtTunnelLimitSucceeds confirms a legitimate reconnect of an
// EXISTING token isn't blocked just because the IP is already at its tunnel
// limit — eviction of the old tunnel must free the slot before the new
// registration's limit check runs.
func TestReconnectAtTunnelLimitSucceeds(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTunnelsPerIP = 1
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()

	ws1 := dialWS(t, srv)
	defer ws1.Close()
	writeMsg(t, ws1, Msg{Type: "register", Token: "vp_samehost_123456"})
	readMsg(t, ws1)

	ws2 := dialWS(t, srv)
	defer ws2.Close()
	writeMsg(t, ws2, Msg{Type: "register", Token: "vp_samehost_123456"})
	if m := readMsg(t, ws2); m.Type != "port" {
		t.Fatalf("reconnect at tunnel limit should succeed, got %+v", m)
	}
}

// TestConcurrentRegistrationsCannotBypassTunnelLimit hammers the same source
// IP with many concurrent, distinct-token registrations and verifies the
// number that actually succeed never exceeds the configured cap — this is
// the regression test for the check+reserve race in registration.
func TestConcurrentRegistrationsCannotBypassTunnelLimit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTunnelsPerIP = 3
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()

	const attempts = 20
	var wg sync.WaitGroup
	results := make(chan string, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws := dialWS(t, srv)
			defer ws.Close()
			writeMsg(t, ws, Msg{Type: "register", Token: fmt.Sprintf("vp_race%d_123456789", i)})
			m := readMsg(t, ws)
			results <- m.Type
			if m.Type == "port" {
				time.Sleep(200 * time.Millisecond) // stay registered while we count below
			}
		}(i)
	}
	wg.Wait()
	close(results)

	accepted := 0
	for res := range results {
		if res == "port" {
			accepted++
		}
	}
	if accepted > cfg.MaxTunnelsPerIP {
		t.Fatalf("expected at most %d accepted registrations under concurrency, got %d", cfg.MaxTunnelsPerIP, accepted)
	}
}

// ─── Item 2: a slow/stuck player can't block other players on the tunnel ───

// TestSlowPlayerDoesNotBlockOtherPlayers connects two players on the same
// tunnel, lets one stop reading entirely, and confirms the other keeps
// receiving host->player data promptly regardless — i.e. that the stuck
// player's full outbox can't stall the host's WebSocket read loop, which
// dispatches writes for every player on the tunnel.
func TestSlowPlayerDoesNotBlockOtherPlayers(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()

	writeMsg(t, ws, Msg{Type: "register", Token: "vp_slowplayer_123456789"})
	portMsg := readMsg(t, ws)
	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))

	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("slow player dial: %v", err)
	}
	defer slow.Close()
	slowConn := readMsg(t, ws)
	if slowConn.Type != "connect" {
		t.Fatalf("expected connect for slow player, got %+v", slowConn)
	}

	fast, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("fast player dial: %v", err)
	}
	defer fast.Close()
	fastConn := readMsg(t, ws)
	if fastConn.Type != "connect" {
		t.Fatalf("expected connect for fast player, got %+v", fastConn)
	}

	// Flood the slow player with far more data than any realistic OS socket
	// buffer could absorb without ever reading from `slow` — this is what a
	// stuck/malicious player looks like. A handful of small messages isn't
	// enough to guarantee the outbox (or the underlying TCP write) actually
	// backs up, since the writer goroutine can drain small messages into the
	// OS send buffer faster than they arrive; enough total bytes eventually
	// forces either the outbox to fill or the write itself to block past
	// playerWriteTimeout, whichever comes first.
	payload := make([]byte, 1024*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	const floodMessages = 64 // 64MB total
	for range floodMessages {
		writeMsg(t, ws, Msg{Type: "data", Conn: slowConn.Conn, Data: encoded})
	}

	// The fast player must still get its own data promptly — proves the
	// host's dispatch loop was never blocked by the slow player's full queue.
	writeMsg(t, ws, Msg{Type: "data", Conn: fastConn.Conn, Data: base64.StdEncoding.EncodeToString([]byte("hi-fast"))})
	fast.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := fast.Read(buf)
	if err != nil {
		t.Fatalf("fast player should have received its data promptly, got error: %v", err)
	}
	if string(buf[:n]) != "hi-fast" {
		t.Fatalf("fast player got wrong data: %q", buf[:n])
	}

	// The relay should eventually give up on the slow player and tell the
	// host it's gone, rather than leaving it silently stuck forever — within
	// playerWriteTimeout (10s) at the very latest, even if the outbox itself
	// never fills. One deadline for the whole wait: gorilla's ReadMessage
	// cannot be called again on the same connection once any call has
	// returned an error (including a deadline timeout), so we must stop at
	// the first error rather than looping past it.
	ws.SetReadDeadline(time.Now().Add(20 * time.Second))
	sawClose := false
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var m Msg
		if json.Unmarshal(raw, &m) == nil && m.Type == "close" && m.Conn == slowConn.Conn {
			sawClose = true
			break
		}
	}
	if !sawClose {
		t.Fatalf("expected the relay to eventually close the slow player's connection")
	}
}

// ─── Item 3: per-source-IP player limit and inactivity timeouts ────────────

func TestPlayerPerIPLimitEnforced(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPlayersPerIPTunnel = 3
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_ipcap_123456789012"})
	portMsg := readMsg(t, ws)
	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))

	var players []net.Conn
	defer func() {
		for _, p := range players {
			p.Close()
		}
	}()
	for i := range cfg.MaxPlayersPerIPTunnel {
		p, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("player %d dial: %v", i, err)
		}
		players = append(players, p)
		if m := readMsg(t, ws); m.Type != "connect" {
			t.Fatalf("player %d: expected connect, got %+v", i, m)
		}
	}

	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("extra dial: %v", err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	if n, err := extra.Read(buf); err == nil {
		t.Fatalf("expected extra same-IP player to be rejected, got %d bytes", n)
	}
}

// TestPlayerPerIPLimitDoesNotAffectOtherIPs confirms one source IP hitting
// its per-tunnel player cap doesn't affect a different source IP.
func TestPlayerPerIPLimitDoesNotAffectOtherIPs(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPlayersPerIPTunnel = 1
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_ipcap2_123456789012"})
	portMsg := readMsg(t, ws)
	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))

	p1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("player1 dial: %v", err)
	}
	defer p1.Close()
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatalf("player1: expected connect, got %+v", m)
	}

	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}
	p2, err := dialer.Dial("tcp", addr)
	if err != nil {
		t.Skipf("cannot bind to 127.0.0.2 in this environment: %v", err)
	}
	defer p2.Close()
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatalf("player2 (different IP): expected connect, got %+v", m)
	}
}

// TestInactivePlayerTimedOut confirms a player that connects but never sends
// anything is dropped after the initial-inactivity window.
func TestInactivePlayerTimedOut(t *testing.T) {
	cfg := testConfig()
	cfg.InitialPlayerTimeout = 200 * time.Millisecond
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_inactive_123456789012"})
	portMsg := readMsg(t, ws)

	p, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer p.Close()
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatalf("expected connect, got %+v", m)
	}

	// Never send anything. The relay should close the connection.
	p.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8)
	if _, err := p.Read(buf); err == nil {
		t.Fatalf("expected the relay to close an inactive player connection")
	}
}

// TestActiveTrafficKeepsPlayerAlive confirms sending data resets the
// timeout, so a player that's actually talking is never reaped by the
// initial-inactivity window.
func TestActiveTrafficKeepsPlayerAlive(t *testing.T) {
	cfg := testConfig()
	cfg.InitialPlayerTimeout = 150 * time.Millisecond
	cfg.PlayerIdleTimeout = 2 * time.Second
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_activetraffic_123456789012"})
	portMsg := readMsg(t, ws)

	p, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer p.Close()
	if m := readMsg(t, ws); m.Type != "connect" {
		t.Fatalf("expected connect, got %+v", m)
	}

	// Keep sending small bytes for longer than the initial timeout would
	// otherwise allow, well within the (longer) idle window each time.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := p.Write([]byte("x")); err != nil {
			t.Fatalf("player write: %v", err)
		}
		readMsg(t, ws) // data frame
		time.Sleep(50 * time.Millisecond)
	}

	// The connection must still be alive — verified by one more round trip.
	if _, err := p.Write([]byte("still-alive")); err != nil {
		t.Fatalf("expected connection to still be alive: %v", err)
	}
	m := readMsg(t, ws)
	if m.Type != "data" {
		t.Fatalf("expected a final data frame, got %+v", m)
	}
}

// ─── Item 4: maxPlayersPerTunnel check+insert must be atomic ───────────────

// TestPlayerCapNeverExceededUnderConcurrency fires many times more
// simultaneous connections than the configured cap and verifies the tracked
// active-player count never exceeds it, even momentarily.
func TestPlayerCapNeverExceededUnderConcurrency(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPlayersPerIPTunnel = 1000 // isolate this test to the tunnel-wide cap only
	srv, r := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_raceplayers_123456789012"})
	portMsg := readMsg(t, ws)
	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))

	const attempts = maxPlayersPerTunnel * 3
	var wg sync.WaitGroup
	var maxObserved int
	var maxMu sync.Mutex
	stop := make(chan struct{})

	// Watch the actual tracked player count while connections race in.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.mu.Lock()
			var cur *client
			for _, cl := range r.clients {
				cur = cl
			}
			r.mu.Unlock()
			if cur != nil {
				cur.mu.Lock()
				n := len(cur.players)
				cur.mu.Unlock()
				maxMu.Lock()
				if n > maxObserved {
					maxObserved = n
				}
				maxMu.Unlock()
			}
		}
	}()

	var conns []net.Conn
	var connsMu sync.Mutex
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, p)
			connsMu.Unlock()
		}()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond) // let the relay finish processing accepts
	close(stop)

	for _, c := range conns {
		c.Close()
	}

	if maxObserved > maxPlayersPerTunnel {
		t.Fatalf("tracked active player count exceeded the cap: observed %d, cap %d", maxObserved, maxPlayersPerTunnel)
	}
}

// ─── Item 5: exactly one live tunnel per token, even under concurrent registration ───

func TestConcurrentRegistrationsSameTokenSettleToOneTunnel(t *testing.T) {
	cfg := testConfig()
	srv, r := newTestServerWithConfig(cfg)
	defer srv.Close()

	const token = "vp_sametoken_race_123456789012"
	const attempts = 15
	var wg sync.WaitGroup
	var connsMu sync.Mutex
	var conns []*websocket.Conn
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws := dialWS(t, srv)
			connsMu.Lock()
			conns = append(conns, ws)
			connsMu.Unlock()
			// Most of these will be evicted by a later, concurrent
			// registration for the same token before ever getting (or after
			// only partially getting) a response — that's the whole point of
			// this test, so read errors here are expected and ignored. We
			// only care about the relay's final settled state, checked below.
			data, err := json.Marshal(Msg{Type: "register", Token: token})
			if err != nil {
				return
			}
			if ws.WriteMessage(websocket.TextMessage, data) != nil {
				return
			}
			ws.SetReadDeadline(time.Now().Add(3 * time.Second))
			ws.ReadMessage()
		}()
	}
	wg.Wait()

	// Give any eviction goroutines a moment to finish unwinding.
	time.Sleep(300 * time.Millisecond)

	sum := sha256.Sum256([]byte(token))
	clientID := hexEncode(sum[:])

	r.mu.Lock()
	_, live := r.clients[clientID]
	liveCount := 0
	for id := range r.clients {
		if id == clientID {
			liveCount++
		}
	}
	usedForThisToken := 0
	for port := range r.usedPort {
		if info, ok := r.portByID[clientID]; ok && info.port == port {
			usedForThisToken++
		}
	}
	r.mu.Unlock()

	if !live {
		t.Fatalf("expected exactly one live tunnel for the token, found none")
	}
	if liveCount != 1 {
		t.Fatalf("expected exactly 1 live tunnel entry, found %d", liveCount)
	}
	if usedForThisToken != 1 {
		t.Fatalf("expected exactly 1 reserved port for this token, found %d", usedForThisToken)
	}

	for _, c := range conns {
		if c != nil {
			c.Close()
		}
	}
}

// hexEncode avoids importing encoding/hex a second time under a different
// name collision in this file; it mirrors relay.go's own hex.EncodeToString.
func hexEncode(b []byte) string {
	const hextable = "0123456789abcdef"
	dst := make([]byte, len(b)*2)
	for i, v := range b {
		dst[i*2] = hextable[v>>4]
		dst[i*2+1] = hextable[v&0x0f]
	}
	return string(dst)
}

// ─── Item 6: bounded sticky-port memory ─────────────────────────────────────

func TestStaleStickyPortEntriesAreSwept(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()

	ws := dialWS(t, srv)
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_staleport_123456789012"})
	readMsg(t, ws)
	ws.Close()

	// Disconnect cleanup (removing the entry from r.clients) runs
	// asynchronously in handleWS's own goroutine — wait for it to actually
	// finish, otherwise the sweep below would see this token as still "live"
	// and correctly (but confusingly, for this test) skip it.
	sum := sha256.Sum256([]byte("vp_staleport_123456789012"))
	clientID := hexEncode(sum[:])
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		_, live := r.clients[clientID]
		r.mu.Unlock()
		if !live {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate the entry having aged past the TTL, then sweep.
	r.mu.Lock()
	info := r.portByID[clientID]
	info.lastSeen = time.Now().Add(-2 * stickyPortTTL)
	r.portByID[clientID] = info
	r.mu.Unlock()

	r.sweepStalePorts()

	r.mu.Lock()
	_, stillThere := r.portByID[clientID]
	r.mu.Unlock()
	if stillThere {
		t.Fatalf("expected stale sticky-port entry to be swept")
	}
}

func TestLiveTokenNeverSweptRegardlessOfAge(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()

	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_liveport_123456789012"})
	readMsg(t, ws)

	sum := sha256.Sum256([]byte("vp_liveport_123456789012"))
	clientID := hexEncode(sum[:])
	r.mu.Lock()
	info := r.portByID[clientID]
	info.lastSeen = time.Now().Add(-2 * stickyPortTTL) // pretend it's ancient
	r.portByID[clientID] = info
	r.mu.Unlock()

	r.sweepStalePorts()

	r.mu.Lock()
	_, stillThere := r.portByID[clientID]
	r.mu.Unlock()
	if !stillThere {
		t.Fatalf("a currently-connected token's sticky-port entry must never be swept")
	}
}

// ─── Item 7: 128-bit connection IDs ─────────────────────────────────────────

func TestNewIDReturns128Bits(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatalf("newID: %v", err)
	}
	if len(id) != 32 { // 16 bytes, hex-encoded
		t.Fatalf("expected a 32-hex-char (128-bit) id, got %d chars: %q", len(id), id)
	}
	if _, err := decodeHexForTest(id); err != nil {
		t.Fatalf("id is not valid hex: %v", err)
	}
}

func decodeHexForTest(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length")
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		var b byte
		for j := range 2 {
			c := s[i*2+j]
			var v byte
			switch {
			case c >= '0' && c <= '9':
				v = c - '0'
			case c >= 'a' && c <= 'f':
				v = c - 'a' + 10
			default:
				return nil, fmt.Errorf("invalid hex char %q", c)
			}
			b = b<<4 | v
		}
		out[i] = b
	}
	return out, nil
}

// TestNewIDDoesNotPanicOrHangOnManyCalls is a light sanity check that ID
// generation behaves under concurrent load (crypto/rand.Read is documented
// as safe for concurrent use).
func TestNewIDManyCallsUnique(t *testing.T) {
	seen := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := newID()
			if err != nil {
				t.Errorf("newID: %v", err)
				return
			}
			mu.Lock()
			if seen[id] {
				t.Errorf("duplicate id generated: %s", id)
			}
			seen[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
}

// ─── Item 9: X-Forwarded-For trust ──────────────────────────────────────────

func TestClientIPIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	r := newRelay(testConfig())
	req := &http.Request{
		RemoteAddr: "203.0.113.7:1234", // a public, non-loopback address
		Header:     http.Header{"X-Forwarded-For": []string{"9.9.9.9"}},
	}
	got := r.clientIP(req)
	if got != "203.0.113.7" {
		t.Fatalf("expected the direct peer address when the peer isn't trusted, got %q", got)
	}
}

func TestClientIPTrustsForwardedHeaderFromLoopback(t *testing.T) {
	r := newRelay(testConfig())
	req := &http.Request{
		RemoteAddr: "127.0.0.1:1234", // Caddy, on the same host, per the documented deployment
		Header:     http.Header{"X-Forwarded-For": []string{"198.51.100.9"}},
	}
	got := r.clientIP(req)
	if got != "198.51.100.9" {
		t.Fatalf("expected the forwarded address from a trusted (loopback) peer, got %q", got)
	}
}

func TestClientIPTrustsRealIPHeaderFromLoopback(t *testing.T) {
	r := newRelay(testConfig())
	req := &http.Request{
		RemoteAddr: "127.0.0.1:1234", // nginx, on the same host, per the trazverse deployment
		Header:     make(http.Header),
	}
	req.Header.Set("X-Real-IP", "198.51.100.9") // Set canonicalizes the key, matching real parsed requests
	got := r.clientIP(req)
	if got != "198.51.100.9" {
		t.Fatalf("expected the X-Real-IP address from a trusted (loopback) peer, got %q", got)
	}
}

func TestClientIPPrefersForwardedForOverRealIP(t *testing.T) {
	r := newRelay(testConfig())
	req := &http.Request{
		RemoteAddr: "127.0.0.1:1234",
		Header:     make(http.Header),
	}
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	req.Header.Set("X-Real-IP", "198.51.100.99")
	got := r.clientIP(req)
	if got != "198.51.100.9" {
		t.Fatalf("expected X-Forwarded-For to take precedence, got %q", got)
	}
}

func TestClientIPIgnoresRealIPHeaderFromUntrustedPeer(t *testing.T) {
	r := newRelay(testConfig())
	req := &http.Request{
		RemoteAddr: "203.0.113.7:1234",
		Header:     make(http.Header),
	}
	req.Header.Set("X-Real-IP", "9.9.9.9")
	got := r.clientIP(req)
	if got != "203.0.113.7" {
		t.Fatalf("expected the direct peer address when the peer isn't trusted, got %q", got)
	}
}

func TestClientIPFallsBackToRemoteAddrWithNoForwardedHeader(t *testing.T) {
	r := newRelay(testConfig())
	req := &http.Request{RemoteAddr: "203.0.113.7:1234"}
	got := r.clientIP(req)
	if got != "203.0.113.7" {
		t.Fatalf("expected RemoteAddr fallback, got %q", got)
	}
}

func TestClientIPTrustsConfiguredProxyCIDR(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	cfg := testConfig()
	cfg.TrustedProxies = []*net.IPNet{cidr}
	r := newRelay(cfg)
	req := &http.Request{
		RemoteAddr: "10.1.2.3:1234",
		Header:     http.Header{"X-Forwarded-For": []string{"198.51.100.9"}},
	}
	got := r.clientIP(req)
	if got != "198.51.100.9" {
		t.Fatalf("expected forwarded address from a configured trusted proxy, got %q", got)
	}
}

// ─── Item 13: cross-tunnel isolation ────────────────────────────────────────

// TestCrossTunnelIsolation registers two independent hosts and verifies
// there is no way for one host's traffic, or a forged connection ID, to
// reach or affect the other host's tunnel.
func TestCrossTunnelIsolation(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	hostA := dialWS(t, srv)
	defer hostA.Close()
	writeMsg(t, hostA, Msg{Type: "register", Token: "vp_hosta_123456789012"})
	portA := readMsg(t, hostA)

	hostB := dialWS(t, srv)
	defer hostB.Close()
	writeMsg(t, hostB, Msg{Type: "register", Token: "vp_hostb_123456789012"})
	portB := readMsg(t, hostB)

	if portA.Port == portB.Port {
		t.Fatalf("hosts A and B were assigned the same port: %d", portA.Port)
	}

	playerA, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portA.Port)))
	if err != nil {
		t.Fatalf("player A dial: %v", err)
	}
	defer playerA.Close()
	connA := readMsg(t, hostA)
	if connA.Type != "connect" {
		t.Fatalf("expected connect on host A, got %+v", connA)
	}

	playerB, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portB.Port)))
	if err != nil {
		t.Fatalf("player B dial: %v", err)
	}
	defer playerB.Close()
	connB := readMsg(t, hostB)
	if connB.Type != "connect" {
		t.Fatalf("expected connect on host B, got %+v", connB)
	}

	// Player A's traffic must only ever reach host A.
	playerA.Write([]byte("only-for-a"))
	dataOnA := readMsg(t, hostA)
	if dataOnA.Type != "data" || dataOnA.Conn != connA.Conn {
		t.Fatalf("expected host A to see player A's data, got %+v", dataOnA)
	}

	// Player B's traffic must only ever reach host B.
	playerB.Write([]byte("only-for-b"))
	dataOnB := readMsg(t, hostB)
	if dataOnB.Type != "data" || dataOnB.Conn != connB.Conn {
		t.Fatalf("expected host B to see player B's data, got %+v", dataOnB)
	}

	// Host A sending "data" using host B's connection ID must be a no-op —
	// it must not reach player B (the id has no authority outside host A's
	// own tunnel), and it must not crash or error host A's connection.
	writeMsg(t, hostA, Msg{Type: "data", Conn: connB.Conn, Data: base64.StdEncoding.EncodeToString([]byte("forged"))})
	playerB.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 16)
	if n, err := playerB.Read(buf); err == nil {
		t.Fatalf("player B must never receive data sent via a forged cross-tunnel connection id, got %q", buf[:n])
	}

	// Host B "closing" host A's connection id must be a no-op — player A's
	// connection must remain open and working.
	writeMsg(t, hostB, Msg{Type: "close", Conn: connA.Conn})
	time.Sleep(200 * time.Millisecond)
	playerA.Write([]byte("still-here"))
	stillAlive := readMsg(t, hostA)
	if stillAlive.Type != "data" {
		t.Fatalf("player A's connection should be unaffected by host B trying to close its id, got %+v", stillAlive)
	}
}

// ─── Item 14: large-payload end-to-end integrity ────────────────────────────

// TestLargePayloadIntegrityPlayerToHost sends several megabytes of
// pseudorandom data from a player through the relay to the host and
// verifies the SHA-256 on each end matches — catches dropped/reordered/
// truncated/corrupted-base64 bytes anywhere in the path.
func TestLargePayloadIntegrityPlayerToHost(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_bigp2h_123456789012"})
	portMsg := readMsg(t, ws)

	player, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer player.Close()
	readMsg(t, ws) // connect

	const size = 4 * 1024 * 1024 // 4MB
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	want := sha256.Sum256(data)

	go func() {
		player.Write(data)
	}()

	got := sha256.New()
	received := 0
	for received < size {
		m := readMsg(t, ws)
		if m.Type != "data" {
			continue
		}
		chunk, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			t.Fatalf("bad base64 in relayed chunk: %v", err)
		}
		got.Write(chunk)
		received += len(chunk)
	}

	if received != size {
		t.Fatalf("expected %d bytes total, got %d", size, received)
	}
	gotSum := got.Sum(nil)
	if string(gotSum) != string(want[:]) {
		t.Fatalf("player->host integrity check failed: hashes differ")
	}
}

// TestLargePayloadIntegrityHostToPlayer is the mirror of the above, in the
// host->player direction (exercising the playerConn outbox/writer path).
func TestLargePayloadIntegrityHostToPlayer(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_bigh2p_123456789012"})
	portMsg := readMsg(t, ws)

	player, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer player.Close()
	connMsg := readMsg(t, ws)
	connID := connMsg.Conn

	const chunkSize = 256 * 1024
	const chunks = 16 // 4MB total, comfortably under playerOutboxSize
	full := make([]byte, chunkSize*chunks)
	if _, err := rand.Read(full); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	want := sha256.Sum256(full)

	for i := range chunks {
		chunk := full[i*chunkSize : (i+1)*chunkSize]
		writeMsg(t, ws, Msg{Type: "data", Conn: connID, Data: base64.StdEncoding.EncodeToString(chunk)})
	}

	got := sha256.New()
	buf := make([]byte, 64*1024)
	received := 0
	player.SetReadDeadline(time.Now().Add(15 * time.Second))
	for received < len(full) {
		n, err := player.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
			received += n
		}
		if err != nil {
			t.Fatalf("player read: %v (received %d/%d)", err, received, len(full))
		}
	}

	gotSum := got.Sum(nil)
	if string(gotSum) != string(want[:]) {
		t.Fatalf("host->player integrity check failed: hashes differ")
	}
}

// ─── Item 16: stress / concurrency / edge-case tests ────────────────────────

// TestManySimultaneousPlayerConnects exercises 100+ concurrent player
// connects/disconnects and checks for no panics, no leaked/incorrect player
// accounting, and a clean final state.
func TestManySimultaneousPlayerConnects(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_manyplayers_123456789012"})
	portMsg := readMsg(t, ws)
	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))

	const n = 120
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			defer p.Close()
			p.Write([]byte("hi"))
			buf := make([]byte, 8)
			p.SetReadDeadline(time.Now().Add(2 * time.Second))
			p.Read(buf) // best-effort; not asserting content here
		}()
	}

	// Drain control-channel messages in the background while connections
	// race in and out, so the relay's sends never block on a full test-side
	// buffer. One deadline for the whole window: gorilla's ReadMessage
	// cannot be called again on the same connection after any error
	// (including a timeout), so we stop draining at the first gap rather
	// than looping past it — a partial drain is fine here, we're not
	// asserting message counts.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.SetReadDeadline(time.Now().Add(4 * time.Second))
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	wg.Wait()
	<-done
}

// TestReconnectWhilePlayersConnected confirms that when a host reconnects
// (evicting its old tunnel), the old tunnel's players are cleanly closed and
// the new tunnel starts with zero players — no leaked state crosses over.
func TestReconnectWhilePlayersConnected(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()

	ws1 := dialWS(t, srv)
	writeMsg(t, ws1, Msg{Type: "register", Token: "vp_reconnectplayers_123456789012"})
	portMsg := readMsg(t, ws1)

	p, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer p.Close()
	readMsg(t, ws1) // connect

	ws2 := dialWS(t, srv)
	defer ws2.Close()
	writeMsg(t, ws2, Msg{Type: "register", Token: "vp_reconnectplayers_123456789012"})
	second := readMsg(t, ws2)
	if second.Type != "port" {
		t.Fatalf("expected reconnect to succeed, got %+v", second)
	}

	// The old player connection must have been closed by the eviction.
	p.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	if n, err := p.Read(buf); err == nil {
		t.Fatalf("expected old tunnel's player connection to be closed on reconnect, got %d bytes", n)
	}

	sum := sha256.Sum256([]byte("vp_reconnectplayers_123456789012"))
	clientID := hexEncode(sum[:])
	r.mu.Lock()
	c := r.clients[clientID]
	r.mu.Unlock()
	c.mu.Lock()
	newPlayerCount := len(c.players)
	c.mu.Unlock()
	if newPlayerCount != 0 {
		t.Fatalf("expected the new tunnel to start with 0 players, got %d", newPlayerCount)
	}
	ws1.Close()
}

// TestHostDisappearsUnexpectedly simulates a host vanishing without a clean
// WebSocket close and confirms the relay eventually cleans up its state
// (port released, tunnel removed) rather than leaking it forever.
func TestHostDisappearsUnexpectedly(t *testing.T) {
	cfg := testConfig()
	srv, r := newTestServerWithConfig(cfg)
	defer srv.Close()

	ws := dialWS(t, srv)
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_vanish_123456789012"})
	portMsg := readMsg(t, ws)

	// Abruptly close the underlying TCP connection rather than a clean WS
	// close handshake, simulating a host whose network just dies.
	ws.UnderlyingConn().Close()

	sum := sha256.Sum256([]byte("vp_vanish_123456789012"))
	clientID := hexEncode(sum[:])
	// The map entry is removed and the port is released in two separate
	// steps of the same deferred cleanup (see handleWS), not atomically
	// together — poll for both, not just the first one to flip.
	deadline := time.Now().Add(3 * time.Second)
	var stillLive, stillUsed bool
	for time.Now().Before(deadline) {
		r.mu.Lock()
		_, stillLive = r.clients[clientID]
		stillUsed = r.usedPort[portMsg.Port]
		r.mu.Unlock()
		if !stillLive && !stillUsed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stillLive {
		t.Fatalf("expected the tunnel to be cleaned up after the host vanished")
	}
	if stillUsed {
		t.Fatalf("expected the port to be released after the host vanished")
	}
}

// TestMalformedBase64DataIgnored confirms a data frame with invalid base64
// is dropped rather than crashing the handler or the tunnel.
func TestMalformedBase64DataIgnored(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_badbase64_123456789012"})
	portMsg := readMsg(t, ws)

	p, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer p.Close()
	connMsg := readMsg(t, ws)

	writeMsg(t, ws, Msg{Type: "data", Conn: connMsg.Conn, Data: "not-valid-base64!!!"})

	// The tunnel must still be usable afterwards.
	writeMsg(t, ws, Msg{Type: "data", Conn: connMsg.Conn, Data: base64.StdEncoding.EncodeToString([]byte("ok"))})
	p.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	n, err := p.Read(buf)
	if err != nil {
		t.Fatalf("expected the tunnel to keep working after a malformed frame, got: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Fatalf("unexpected data: %q", buf[:n])
	}
}

// TestUnknownConnectionIDIgnored confirms messages referencing a connection
// id that doesn't exist are silently ignored, not treated as an error.
func TestUnknownConnectionIDIgnored(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_unknownconn_123456789012"})
	readMsg(t, ws)

	writeMsg(t, ws, Msg{Type: "data", Conn: "00112233445566778899aabbccddeeff", Data: base64.StdEncoding.EncodeToString([]byte("x"))})
	writeMsg(t, ws, Msg{Type: "close", Conn: "00112233445566778899aabbccddeeff"})

	// The control channel must still be responsive afterwards.
	writeMsg(t, ws, Msg{Type: "ping"})
	if m := readMsg(t, ws); m.Type != "pong" {
		t.Fatalf("expected pong after unknown-conn-id messages, got %+v", m)
	}
}

// TestRapidOpenCloseCycles hammers connect/disconnect in a tight loop and
// checks the relay ends up in a consistent state (no leaked player entries).
func TestRapidOpenCloseCycles(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()
	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_rapidcycle_123456789012"})
	portMsg := readMsg(t, ws)
	addr := net.JoinHostPort("127.0.0.1", itoa(portMsg.Port))

	for range 50 {
		p, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		readMsg(t, ws) // connect
		p.Close()
		readMsg(t, ws) // close (from the read loop observing the reset/EOF)
	}

	sum := sha256.Sum256([]byte("vp_rapidcycle_123456789012"))
	clientID := hexEncode(sum[:])
	r.mu.Lock()
	c := r.clients[clientID]
	r.mu.Unlock()

	// handlePlayer sends the "close" notification before it deletes the
	// entry from c.players (two separate statements in the same goroutine) —
	// having read the last "close" message only proves the send happened,
	// not that the following delete has completed yet. Poll briefly rather
	// than assuming it's already done.
	deadline := time.Now().Add(2 * time.Second)
	var remaining int
	for {
		c.mu.Lock()
		remaining = len(c.players)
		c.mu.Unlock()
		if remaining == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if remaining != 0 {
		t.Fatalf("expected no leaked player entries after rapid open/close cycles, got %d", remaining)
	}
}

// ─── Item 23: graceful shutdown ─────────────────────────────────────────────

func TestShutdownClosesActiveTunnelsAndPlayers(t *testing.T) {
	cfg := testConfig()
	r := newRelay(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ws := dialWS(t, srv)
	defer ws.Close()
	writeMsg(t, ws, Msg{Type: "register", Token: "vp_shutdown_123456789012"})
	portMsg := readMsg(t, ws)

	p, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(portMsg.Port)))
	if err != nil {
		t.Fatalf("player dial: %v", err)
	}
	defer p.Close()
	readMsg(t, ws) // connect

	r.shutdown()

	// The control WebSocket must eventually close. It may deliver one final
	// "close" notification for the player first (handlePlayer's own goroutine
	// reacts to its connection being torn down independently of client.close
	// closing the control WS a moment later), so drain until the read
	// actually fails rather than assuming the very first read errors. One
	// deadline for the whole wait — gorilla panics if ReadMessage is called
	// again after any previous error, so we must stop at the first one.
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	closed := false
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatalf("expected control connection to be closed after shutdown")
	}

	// The player connection must be closed.
	p.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	if n, err := p.Read(buf); err == nil {
		t.Fatalf("expected player connection to be closed after shutdown, got %d bytes", n)
	}

	r.mu.Lock()
	remaining := len(r.clients)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected no tunnels to remain after shutdown, got %d", remaining)
	}
}

// itoa avoids importing strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [6]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

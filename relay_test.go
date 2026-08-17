package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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

func newTestServer() *httptest.Server {
	r := newRelay(26000, 26050, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/api/status", r.apiStatus)
	return httptest.NewServer(mux)
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

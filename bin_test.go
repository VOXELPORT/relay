package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Tests for protocol v2 (binary data frames; items B1–B4 of the capacity change), using real WebSocket clients against a
// real relay on a test HTTP server.

func binFrame(kind byte, idHex string, payload []byte) []byte {
	id, _ := hex.DecodeString(idHex)
	return append(append([]byte{kind}, id...), payload...)
}

// registerAs registers a host, asking for protocol v2 when bin is set, and returns the relay's port frame.
func registerAs(t *testing.T, srv interface{ Close() }, url, token string, bin, udp bool) (*websocket.Conn, Msg) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(url, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := Msg{Type: "register", Token: token, UDP: udp}
	if bin {
		reg.Bin = 1
	}
	writeMsg(t, ws, reg)
	m := readMsg(t, ws)
	if m.Type != "port" {
		t.Fatalf("expected a port frame, got %+v", m)
	}
	return ws, m
}

// readRaw returns the next WebSocket message of any kind.
func readRaw(t *testing.T, ws *websocket.Conn) (int, []byte) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read ws: %v", err)
	}
	return mt, data
}

// expectJSON reads the next message, which must be a JSON text frame of type typ.
func expectJSON(t *testing.T, ws *websocket.Conn, typ string) Msg {
	t.Helper()
	mt, data := readRaw(t, ws)
	if mt != websocket.TextMessage {
		t.Fatalf("expected a JSON text frame (%s), got a binary frame of %d bytes", typ, len(data))
	}
	var m Msg
	if err := json.Unmarshal(data, &m); err != nil || m.Type != typ {
		t.Fatalf("expected %s, got %q (%v)", typ, data, err)
	}
	return m
}

// openPlayer connects a player to the tunnel's public port and returns it with the connection id the host was told about
// (the "connect" frame is a JSON text frame in both modes).
func openPlayer(t *testing.T, ws *websocket.Conn, port int) (net.Conn, string) {
	t.Helper()
	p, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	return p, expectJSON(t, ws, "connect").Conn
}

func readPlayer(t *testing.T, p net.Conn, n int) []byte {
	t.Helper()
	p.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, n)
	read := 0
	for read < n {
		m, err := p.Read(got[read:])
		if err != nil {
			t.Fatalf("player read after %d of %d bytes: %v", read, n, err)
		}
		read += m
	}
	return got
}

func playerGetsNothing(t *testing.T, p net.Conn) {
	t.Helper()
	p.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := p.Read(make([]byte, 16)); err == nil {
		t.Fatalf("the player must not receive anything, got %d bytes", n)
	}
}

func TestB1_FramesRoundTripTheirHeader(t *testing.T) {
	id, _ := newID()
	raw, ok := rawID(id)
	if !ok {
		t.Fatal("a relay id must convert")
	}
	if hex.EncodeToString(raw[:]) != id {
		t.Fatal("rawID is the hex-decoded id")
	}
	frame := make([]byte, frameHeaderLen+3)
	putFrameHeader(frame, frameUDP, raw)
	copy(frame[frameHeaderLen:], "abc")
	if frame[0] != 0x02 || !bytes.Equal(frame[1:17], raw[:]) || string(frame[17:]) != "abc" {
		t.Fatalf("layout is kind, 16 id bytes, payload: % x", frame)
	}
	if frameTCP != 0x01 || frameUDP != 0x02 || frameHeaderLen != 17 {
		t.Fatal("the constants are part of the wire format")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 32), strings.Repeat("a", 31), strings.Repeat("a", 33)} {
		if _, ok := rawID(bad); ok {
			t.Errorf("rawID(%q) must be refused", bad)
		}
	}
}

func TestB1_ThePortReplyOnlyConfirmsWhatWasAskedFor(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	wsLegacy, legacy := registerAs(t, srv, srv.URL, "vp_b1legacy_aaaaaaaaaa", false, false)
	defer wsLegacy.Close()
	if legacy.Bin != 0 {
		t.Fatalf("a client that did not ask must not be told bin:1, got %+v", legacy)
	}
	raw, _ := json.Marshal(legacy)
	if strings.Contains(string(raw), "bin") {
		t.Fatalf("the port frame of a legacy client has no bin field at all: %s", raw)
	}
	wsBin, v2 := registerAs(t, srv, srv.URL, "vp_b1binary_aaaaaaaaaa", true, false)
	defer wsBin.Close()
	if v2.Bin != 1 {
		t.Fatalf("a client that asked is told bin:1, got %+v", v2)
	}
	// Only the value 1 asks.
	ws3, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws3.Close()
	writeMsg(t, ws3, Msg{Type: "register", Token: "vp_b1twotwo_aaaaaaaaaa", Bin: 2})
	if m := readMsg(t, ws3); m.Type != "port" || m.Bin != 0 {
		t.Fatalf("bin:2 is not a request, got %+v", m)
	}
}

func TestB4_ALegacyClientGetsJSONEverywhere(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4legacy_aaaaaaaaaa", false, false)
	defer ws.Close()
	p, id := openPlayer(t, ws, m.Port)
	defer p.Close()
	p.Write([]byte("hello"))
	mt, data := readRaw(t, ws)
	if mt != websocket.TextMessage {
		t.Fatalf("a legacy tunnel must never get a binary frame (got %d bytes)", len(data))
	}
	var d Msg
	json.Unmarshal(data, &d)
	got, _ := base64.StdEncoding.DecodeString(d.Data)
	if d.Type != "data" || d.Conn != id || string(got) != "hello" {
		t.Fatalf("expected a JSON data frame, got %q", data)
	}
	writeMsg(t, ws, Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString([]byte("world"))})
	if string(readPlayer(t, p, 5)) != "world" {
		t.Fatal("JSON data frames still reach the player")
	}
	p.Close()
	expectJSON(t, ws, "close")
}

func TestB4_ABinaryClientGetsBinaryPayloadsAndJSONControl(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4binary_aaaaaaaaaa", true, false)
	defer ws.Close()
	p, id := openPlayer(t, ws, m.Port) // "connect" is a JSON text frame in both modes

	payload := make([]byte, 20000)
	rand.Read(payload)
	p.Write(payload)
	var got []byte
	for len(got) < len(payload) {
		mt, data := readRaw(t, ws)
		if mt != websocket.BinaryMessage {
			t.Fatalf("payload to a v2 host must be binary, got a text frame: %q", data)
		}
		if len(data) < frameHeaderLen || data[0] != frameTCP || hex.EncodeToString(data[1:17]) != id {
			t.Fatalf("bad frame header: % x", data[:min(len(data), 20)])
		}
		got = append(got, data[frameHeaderLen:]...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("player -> host bytes changed on the way")
	}

	reply := make([]byte, 20000)
	rand.Read(reply)
	if err := ws.WriteMessage(websocket.BinaryMessage, binFrame(frameTCP, id, reply)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readPlayer(t, p, len(reply)), reply) {
		t.Fatal("host -> player bytes changed on the way")
	}

	// Control frames stay JSON in v2.
	writeMsg(t, ws, Msg{Type: "ping"})
	expectJSON(t, ws, "pong")
	p.Close()
	expectJSON(t, ws, "close")
}

func TestB4_ABinaryClientMayStillSendJSONData(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4mixed_aaaaaaaaaaa", true, false)
	defer ws.Close()
	p, id := openPlayer(t, ws, m.Port)
	defer p.Close()
	writeMsg(t, ws, Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString([]byte("partial client"))})
	if string(readPlayer(t, p, 14)) != "partial client" {
		t.Fatal("a JSON data frame on a v2 tunnel is still accepted")
	}
}

func TestB4_MalformedBinaryFramesAreIgnoredAndTheTunnelStaysUp(t *testing.T) {
	logs := captureLog(t)
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4malform_aaaaaaaaaa", true, false)
	defer ws.Close()
	p, id := openPlayer(t, ws, m.Port)
	defer p.Close()

	unknown := make([]byte, 16)
	rand.Read(unknown)
	udpOversize := binFrame(frameUDP, id, bytes.Repeat([]byte{1}, maxUDPDatagram+1))
	bad := [][]byte{
		{},                                       // nothing
		{frameTCP},                               // just a kind
		append([]byte{frameTCP}, unknown[:5]...), // shorter than its header
		binFrame(0x7f, id, []byte("x")),          // unknown kind
		binFrame(0x00, id, []byte("x")),
		append(append([]byte{frameTCP}, unknown...), "no such connection"...),
		binFrame(frameUDP, id, []byte("this tunnel opened no UDP")),
		udpOversize,
	}
	for _, f := range bad {
		if err := ws.WriteMessage(websocket.BinaryMessage, f); err != nil {
			t.Fatal(err)
		}
	}
	playerGetsNothing(t, p)

	// The tunnel is still up and still carries data both ways.
	writeMsg(t, ws, Msg{Type: "ping"})
	expectJSON(t, ws, "pong")
	ws.WriteMessage(websocket.BinaryMessage, binFrame(frameTCP, id, []byte("still works")))
	if string(readPlayer(t, p, 11)) != "still works" {
		t.Fatal("a valid frame after the bad ones is delivered")
	}
	p.Write([]byte("and back"))
	mt, data := readRaw(t, ws)
	if mt != websocket.BinaryMessage || string(data[frameHeaderLen:]) != "and back" {
		t.Fatalf("player -> host still works: %q", data)
	}

	// Those were all logged, but only once.
	waitForLog(t, logs, "ignored a binary frame")
	if n := strings.Count(logs.String(), "ignored a binary frame"); n != 1 {
		t.Fatalf("a flood of bad frames is logged once, not %d times", n)
	}
}

func TestB4_ABinaryFrameOnALegacyTunnelIsIgnored(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4oldhost_aaaaaaaaaa", false, false)
	defer ws.Close()
	p, id := openPlayer(t, ws, m.Port)
	defer p.Close()
	ws.WriteMessage(websocket.BinaryMessage, binFrame(frameTCP, id, []byte("not for you")))
	playerGetsNothing(t, p)
	writeMsg(t, ws, Msg{Type: "ping"})
	expectJSON(t, ws, "pong")
	writeMsg(t, ws, Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString([]byte("json is fine"))})
	if string(readPlayer(t, p, 12)) != "json is fine" {
		t.Fatal("JSON data still works on a legacy tunnel")
	}
}

func TestB4_ALegacyAndABinaryTunnelRunSideBySide(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	wsOld, mOld := registerAs(t, srv, srv.URL, "vp_b4side_oldoldoldold", false, false)
	defer wsOld.Close()
	wsNew, mNew := registerAs(t, srv, srv.URL, "vp_b4side_newnewnewnew", true, false)
	defer wsNew.Close()
	pOld, idOld := openPlayer(t, wsOld, mOld.Port)
	defer pOld.Close()
	pNew, idNew := openPlayer(t, wsNew, mNew.Port)
	defer pNew.Close()

	pOld.Write([]byte("to the old host"))
	pNew.Write([]byte("to the new host"))
	mt, data := readRaw(t, wsOld)
	if mt != websocket.TextMessage || !strings.Contains(string(data), idOld) {
		t.Fatalf("the old host got %q", data)
	}
	mt, data = readRaw(t, wsNew)
	if mt != websocket.BinaryMessage || hex.EncodeToString(data[1:17]) != idNew || string(data[frameHeaderLen:]) != "to the new host" {
		t.Fatalf("the new host got %q", data)
	}
	writeMsg(t, wsOld, Msg{Type: "data", Conn: idOld, Data: base64.StdEncoding.EncodeToString([]byte("from old"))})
	wsNew.WriteMessage(websocket.BinaryMessage, binFrame(frameTCP, idNew, []byte("from new")))
	if string(readPlayer(t, pOld, 8)) != "from old" || string(readPlayer(t, pNew, 8)) != "from new" {
		t.Fatal("each player gets its own host's data")
	}
	// A frame for the other tunnel's connection is for nobody.
	wsNew.WriteMessage(websocket.BinaryMessage, binFrame(frameTCP, idOld, []byte("crossed")))
	playerGetsNothing(t, pOld)
}

func TestB4_UDPRoundTripInBinaryMode(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4udpbin_aaaaaaaaaaa", true, true)
	defer ws.Close()
	client, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", itoa(m.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.Write([]byte("ping from bedrock"))
	mt, data := readRaw(t, ws)
	if mt != websocket.BinaryMessage || len(data) < frameHeaderLen || data[0] != frameUDP || string(data[frameHeaderLen:]) != "ping from bedrock" {
		t.Fatalf("a v2 host gets the datagram as a binary UDP frame, got %q", data)
	}
	id := hex.EncodeToString(data[1:17])
	if err := ws.WriteMessage(websocket.BinaryMessage, binFrame(frameUDP, id, []byte("pong from geyser"))); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil || string(buf[:n]) != "pong from geyser" {
		t.Fatalf("the player gets the reply: %q, %v", buf[:n], err)
	}
	// A second datagram is a binary frame on the same session id.
	client.Write([]byte("again"))
	mt, data = readRaw(t, ws)
	if mt != websocket.BinaryMessage || hex.EncodeToString(data[1:17]) != id || string(data[frameHeaderLen:]) != "again" {
		t.Fatalf("same session, binary frame: %q", data)
	}
	// An over-size datagram from the host is dropped.
	ws.WriteMessage(websocket.BinaryMessage, binFrame(frameUDP, id, bytes.Repeat([]byte{7}, maxUDPDatagram+1)))
	client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := client.Read(buf); err == nil {
		t.Fatalf("an over-size datagram must not be forwarded, got %d bytes", n)
	}
	// JSON udp frames are still understood from a v2 host.
	writeMsg(t, ws, Msg{Type: "udp", Conn: id, Data: base64.StdEncoding.EncodeToString([]byte("json udp"))})
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err = client.Read(buf)
	if err != nil || string(buf[:n]) != "json udp" {
		t.Fatalf("a JSON udp frame from a v2 host still works: %q %v", buf[:n], err)
	}
}

func TestB4_ByteCountersAreTheSameInBothModes(t *testing.T) {
	counts := func(bin bool, token string) (in, out int64) {
		cfg := testConfig()
		srv, r := newTestServerWithConfig(cfg)
		defer srv.Close()
		ws, m := registerAs(t, srv, srv.URL, token, bin, false)
		defer ws.Close()
		p, id := openPlayer(t, ws, m.Port)
		defer p.Close()
		up := bytes.Repeat([]byte("u"), 1000)
		down := bytes.Repeat([]byte("d"), 700)
		p.Write(up)
		got := 0
		for got < len(up) {
			mt, data := readRaw(t, ws)
			if mt == websocket.BinaryMessage {
				got += len(data) - frameHeaderLen
			} else {
				var d Msg
				json.Unmarshal(data, &d)
				b, _ := base64.StdEncoding.DecodeString(d.Data)
				got += len(b)
			}
		}
		if bin {
			ws.WriteMessage(websocket.BinaryMessage, binFrame(frameTCP, id, down))
		} else {
			writeMsg(t, ws, Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString(down)})
		}
		readPlayer(t, p, len(down))
		r.mu.Lock() // the relay's own goroutines change this map
		c := r.clients[clientIDFor(token)]
		r.mu.Unlock()
		return c.bytesIn.Load(), c.bytesOut.Load()
	}
	inJSON, outJSON := counts(false, "vp_b4count_jsonjsonjson")
	inBin, outBin := counts(true, "vp_b4count_binbinbinbin")
	if inJSON != 1000 || outJSON != 700 {
		t.Fatalf("JSON mode counts payload bytes: in=%d out=%d", inJSON, outJSON)
	}
	if inBin != inJSON || outBin != outJSON {
		t.Fatalf("both modes must count the same payload bytes: json in=%d out=%d, binary in=%d out=%d", inJSON, outJSON, inBin, outBin)
	}
}

func TestB4_PlayersOnABinaryTunnelCanBeManyAndLarge(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	ws, m := registerAs(t, srv, srv.URL, "vp_b4many_aaaaaaaaaaaa", true, false)
	defer ws.Close()
	const players = 8
	conns := make([]net.Conn, players)
	ids := make([]string, players)
	for i := range conns {
		conns[i], ids[i] = openPlayer(t, ws, m.Port)
		defer conns[i].Close()
	}
	// Each player sends its own bytes; each frame must carry its own id.
	for i, c := range conns {
		c.Write(bytes.Repeat([]byte{byte('a' + i)}, 100))
	}
	seen := map[string]int{}
	for total := 0; total < players*100; {
		mt, data := readRaw(t, ws)
		if mt != websocket.BinaryMessage {
			t.Fatalf("expected binary, got %q", data)
		}
		id := hex.EncodeToString(data[1:17])
		for _, b := range data[frameHeaderLen:] {
			for i := range ids {
				if ids[i] == id && b != byte('a'+i) {
					t.Fatalf("a byte of player %d arrived under another player's id", i)
				}
			}
		}
		seen[id] += len(data) - frameHeaderLen
		total += len(data) - frameHeaderLen
	}
	for i, id := range ids {
		if seen[id] != 100 {
			t.Errorf("player %d: %d bytes arrived under its id, want 100", i, seen[id])
		}
	}
}

// ─── The point of it: the same payload through the encode and decode of each protocol ───────

func benchPayload() []byte {
	p := make([]byte, 64*1024)
	rand.Read(p)
	return p
}

// BenchmarkDataPathJSON: a payload as the relay and a host handle it in v1 (base64 inside a JSON text frame, both ways).
func BenchmarkDataPathJSON(b *testing.B) {
	payload := benchPayload()
	id, _ := newID()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		frame, _ := json.Marshal(Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString(payload)})
		var m Msg
		if err := json.Unmarshal(frame, &m); err != nil {
			b.Fatal(err)
		}
		out, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil || len(out) != len(payload) {
			b.Fatal("round trip failed")
		}
	}
}

// BenchmarkDataPathBinary: the same payload as a v2 binary frame (a header in front of the bytes).
func BenchmarkDataPathBinary(b *testing.B) {
	payload := benchPayload()
	id, _ := newID()
	raw, _ := rawID(id)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		frame := make([]byte, frameHeaderLen+len(payload))
		putFrameHeader(frame, frameTCP, raw)
		copy(frame[frameHeaderLen:], payload)
		if frame[0] != frameTCP || len(frame[frameHeaderLen:]) != len(payload) {
			b.Fatal("round trip failed")
		}
		_ = hex.EncodeToString(frame[1:frameHeaderLen]) // the relay looks the connection up by its hex id
	}
}

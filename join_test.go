package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ─── Codes ───────────────────────────────────────────────────────────────────────

func TestNormalizeAndFormatCode(t *testing.T) {
	cases := map[string]string{
		"k7m2q-9xh4t":       "K7M2Q9XH4T",
		" K7M2Q 9XH4T ":     "K7M2Q9XH4T",
		"K7M2Q9XH4T":        "K7M2Q9XH4T",
		"k7m2q_9xh4t":       "K7M2Q9XH4T",
		"O0Il1":             "00111", // look-alikes: O is 0, I and L are 1
		"UUUU":              "",      // U is not in the alphabet
		"":                  "",
	}
	for in, want := range cases {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	if formatCode("K7M2Q9XH4T") != "K7M2Q-9XH4T" {
		t.Fatal("a code is shown as XXXXX-XXXXX")
	}
	if formatCode("short") != "short" {
		t.Fatal("a value that is not a code is left alone")
	}
}

var codePattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{10}$`)

func TestCodesAreStablePerHostDistinctAndRotate(t *testing.T) {
	dir := t.TempDir()
	h, err := newJoinHub(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := &client{id: strings.Repeat("a", 64)}, &client{id: strings.Repeat("b", 64)}
	ca, cb := h.enable(a), h.enable(b)
	if !codePattern.MatchString(ca) || !codePattern.MatchString(cb) {
		t.Fatalf("codes should be 10 characters from the safe alphabet: %q %q", ca, cb)
	}
	if ca == cb {
		t.Fatal("two hosts must not share a code")
	}
	if h.enable(a) != ca {
		t.Fatal("a code is stable while the host stays the same")
	}
	if h.lookup(ca) != a || h.lookup(cb) != b {
		t.Fatal("a code finds its host")
	}

	// A restarted relay (same data directory) gives the same host the same code.
	h2, err := newJoinHub(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := h2.enable(a); got != ca {
		t.Fatalf("the code changed across a restart: %s -> %s", ca, got)
	}

	// Rotating revokes the old code at once, and the new one survives a restart too.
	nc := h2.rotate(a)
	if nc == "" || nc == ca {
		t.Fatalf("rotate should give a new code, got %q", nc)
	}
	if h2.lookup(ca) != nil || h2.lookup(nc) != a {
		t.Fatal("after rotating only the new code works")
	}
	h3, _ := newJoinHub(dir)
	if got := h3.enable(a); got != nc {
		t.Fatalf("the rotated code was not remembered across a restart: %s vs %s", nc, got)
	}
	if got := h3.enable(b); got != cb {
		t.Fatal("rotating one host must not change another's code")
	}
}

func TestCodesNeedTheKeyAndAnInMemoryHubChangesOnRestart(t *testing.T) {
	a := &client{id: strings.Repeat("a", 64)}
	h1, _ := newJoinHub("")
	h2, _ := newJoinHub("")
	if h1.enable(a) == h2.enable(a) {
		t.Fatal("two hubs with different keys must not give the same code")
	}
}

func (h *joinHub) codeForForTest(token string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.codeForLocked(clientIDFor(token))
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRotateAndDisableOnAHostThatNeverTurnedItOn(t *testing.T) {
	h, _ := newJoinHub("")
	a := &client{id: strings.Repeat("a", 64)}
	if h.rotate(a) != "" {
		t.Fatal("nothing to rotate when join codes are off")
	}
	if h.disable(a) {
		t.Fatal("nothing to turn off")
	}
}

// ─── Through the relay ───────────────────────────────────────────────────────────

type joinRig struct {
	srv *httptest.Server
	r   *relay
	hub *joinHub
}

func newJoinRig(t *testing.T, withJoin bool) *joinRig {
	t.Helper()
	cfg := testConfig()
	cfg.MinPort, cfg.MaxPort = 27500, 27560
	cfg.PublicHost = "play.example.test"
	var hub *joinHub
	if withJoin {
		hub, _ = newJoinHub(t.TempDir())
		cfg.Join = hub
	}
	r := newRelay(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/join", r.handleJoin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &joinRig{srv: srv, r: r, hub: hub}
}

func (g *joinRig) host(t *testing.T, token string) (*websocket.Conn, int) {
	t.Helper()
	ws, port := register(t, g.srv, token, false)
	t.Cleanup(func() { ws.Close() })
	return ws, port
}

func (g *joinRig) joiner(t *testing.T) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(g.srv.URL, "http") + "/join"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial /join: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func (g *joinRig) enable(t *testing.T, host *websocket.Conn) string {
	t.Helper()
	on := true
	writeMsg(t, host, Msg{Type: "p2p", P2P: &on})
	m := readUntil(t, host, "join_code")
	if m.Code == "" {
		t.Fatal("expected a code")
	}
	return m.Code
}

func TestAHostThatTurnsJoinCodesOnGetsACode(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000001")
	code := g.enable(t, host)
	if !regexp.MustCompile(`^[0-9A-Z]{5}-[0-9A-Z]{5}$`).MatchString(code) {
		t.Fatalf("a code is shown as XXXXX-XXXXX, got %q", code)
	}
	if g.hub.lookup(normalizeCode(code)) == nil {
		t.Fatal("the code should find the host")
	}
}

func TestAPlayerWithTheCodeIsIntroducedAndSignalsFlowBothWays(t *testing.T) {
	g := newJoinRig(t, true)
	host, port := g.host(t, "vp_joinhost0000002")
	code := g.enable(t, host)

	j := g.joiner(t)
	// Typed any way a person might: lower case, no dash.
	writeMsg(t, j, Msg{Type: "join", Code: strings.ToLower(strings.ReplaceAll(code, "-", "")), Client: "desktop/1.9.0"})

	joined := readMsg(t, j)
	if joined.Type != "joined" || joined.Address != "play.example.test" || joined.Port != port || joined.Peer == "" {
		t.Fatalf("the player should get the host's relay address for a fallback: %+v", joined)
	}
	peer := readUntil(t, host, "peer")
	if peer.Peer != joined.Peer || peer.Client != "desktop/1.9.0" {
		t.Fatalf("the host should be told who is coming: %+v vs %+v", peer, joined)
	}

	writeMsg(t, j, Msg{Type: "signal", Signal: `{"type":"offer","sdp":"v=0"}`})
	got := readUntil(t, host, "peer_signal")
	if got.Peer != joined.Peer || got.Signal != `{"type":"offer","sdp":"v=0"}` {
		t.Fatalf("the offer should reach the host: %+v", got)
	}
	writeMsg(t, host, Msg{Type: "peer_signal", Peer: joined.Peer, Signal: `{"type":"answer","sdp":"v=0"}`})
	back := readMsg(t, j)
	if back.Type != "signal" || back.Signal != `{"type":"answer","sdp":"v=0"}` {
		t.Fatalf("the answer should reach the player: %+v", back)
	}

	writeMsg(t, j, Msg{Type: "leave"})
	if gone := readUntil(t, host, "peer_gone"); gone.Peer != joined.Peer {
		t.Fatalf("the host should be told the player left: %+v", gone)
	}
}

func TestAWrongCodeIsRefusedAndTooManyWrongOnesBlockTheAddress(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000003")
	code := g.enable(t, host)

	for i := 0; i < joinFailLimit; i++ {
		j := g.joiner(t)
		writeMsg(t, j, Msg{Type: "join", Code: "ZZZZZ-ZZZZ" + string(rune('0'+i%10))})
		if m := readMsg(t, j); m.Type != "join_error" || !strings.Contains(m.Message, "isn't active") {
			t.Fatalf("try %d: %+v", i, m)
		}
	}
	// Out of guesses: even the right code is refused for a while, so a code can't be guessed by trying.
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: code})
	if m := readMsg(t, j); m.Type != "join_error" || !strings.Contains(m.Message, "Too many wrong codes") {
		t.Fatalf("expected the block, got %+v", m)
	}
}

func TestACodeOnlyWorksWhileTheHostHasJoinCodesOn(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000004")
	j0 := g.joiner(t)
	writeMsg(t, j0, Msg{Type: "join", Code: formatCode(g.hub.codeForForTest("vp_joinhost0000004"))})
	if m := readMsg(t, j0); m.Type != "join_error" {
		t.Fatalf("the host has not turned it on: %+v", m)
	}

	code := g.enable(t, host)
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: code})
	if m := readMsg(t, j); m.Type != "joined" {
		t.Fatalf("it should work now: %+v", m)
	}
	readUntil(t, host, "peer")

	off := false
	writeMsg(t, host, Msg{Type: "p2p", P2P: &off})
	if m := readMsg(t, j); m.Type != "join_error" || !strings.Contains(m.Message, "turned join codes off") {
		t.Fatalf("a waiting player should be told: %+v", m)
	}
	if m := readUntil(t, host, "join_code"); m.Code != "" {
		t.Fatalf("the host should be told the code is gone: %+v", m)
	}
	j2 := g.joiner(t)
	writeMsg(t, j2, Msg{Type: "join", Code: code})
	if m := readMsg(t, j2); m.Type != "join_error" {
		t.Fatalf("the code must stop working: %+v", m)
	}
}

func TestPlayersAreToldWhenTheHostGoesOffline(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000005")
	code := g.enable(t, host)
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: code})
	readMsg(t, j)
	host.Close()
	if m := readMsg(t, j); m.Type != "join_error" || !strings.Contains(m.Message, "offline") {
		t.Fatalf("expected the offline message, got %+v", m)
	}
	// And the code is dead until the host is back.
	if g.hub.lookup(normalizeCode(code)) != nil {
		t.Fatal("a gone host's code must stop working")
	}
}

func TestRotatingTheCodeRevokesTheOldOneAndDropsWaitingPlayers(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000006")
	oldCode := g.enable(t, host)
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: oldCode})
	readMsg(t, j)

	writeMsg(t, host, Msg{Type: "p2p_rotate"})
	fresh := readUntil(t, host, "join_code").Code
	if fresh == oldCode || fresh == "" {
		t.Fatalf("expected a new code, got %q (old %q)", fresh, oldCode)
	}
	if m := readMsg(t, j); m.Type != "join_error" || !strings.Contains(m.Message, "new code") {
		t.Fatalf("a waiting player should be told: %+v", m)
	}
	j2 := g.joiner(t)
	writeMsg(t, j2, Msg{Type: "join", Code: oldCode})
	if m := readMsg(t, j2); m.Type != "join_error" {
		t.Fatalf("the old code must be dead: %+v", m)
	}
	j3 := g.joiner(t)
	writeMsg(t, j3, Msg{Type: "join", Code: fresh})
	if m := readMsg(t, j3); m.Type != "joined" {
		t.Fatalf("the new code must work: %+v", m)
	}
}

func TestRotatingWithJoinCodesOffIsRefusedPolitely(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000007")
	writeMsg(t, host, Msg{Type: "p2p_rotate"})
	if m := readUntil(t, host, "join_error"); !strings.Contains(m.Message, "on first") {
		t.Fatalf("unexpected %+v", m)
	}
}

func TestSignalsAreOnlyEverDeliveredWithinOnePair(t *testing.T) {
	g := newJoinRig(t, true)
	hostA, _ := g.host(t, "vp_joinhost000000A")
	hostB, _ := g.host(t, "vp_joinhost000000B")
	codeA, codeB := g.enable(t, hostA), g.enable(t, hostB)
	ja, jb := g.joiner(t), g.joiner(t)
	writeMsg(t, ja, Msg{Type: "join", Code: codeA})
	writeMsg(t, jb, Msg{Type: "join", Code: codeB})
	peerA := readMsg(t, ja).Peer
	readMsg(t, jb)
	readUntil(t, hostA, "peer")
	readUntil(t, hostB, "peer")

	// Host B tries to reach host A's player by guessing its id.
	writeMsg(t, hostB, Msg{Type: "peer_signal", Peer: peerA, Signal: "hello from the wrong host"})
	// Host B tries to close it too.
	writeMsg(t, hostB, Msg{Type: "peer_close", Peer: peerA})
	// A signal from A's player goes to A only, and a real signal from A afterwards arrives.
	writeMsg(t, ja, Msg{Type: "signal", Signal: "mine"})
	if m := readUntil(t, hostA, "peer_signal"); m.Signal != "mine" {
		t.Fatalf("host A should get its own player's signal: %+v", m)
	}
	writeMsg(t, hostA, Msg{Type: "peer_signal", Peer: peerA, Signal: "from the right host"})
	if m := readMsg(t, ja); m.Type != "signal" || m.Signal != "from the right host" {
		t.Fatalf("the player must only ever see its own host's signals, got %+v", m)
	}
	hostB.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, raw, err := hostB.ReadMessage(); err == nil && strings.Contains(string(raw), "peer_signal") {
		t.Fatalf("host B must not receive host A's player's signals: %s", raw)
	}
}

func TestOversizedAndFloodedSignalsAreHandled(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000008")
	code := g.enable(t, host)
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: code})
	readMsg(t, j)
	readUntil(t, host, "peer")

	// Too big for a setup message but under the socket's read limit: ignored, the connection stays up.
	writeMsg(t, j, Msg{Type: "signal", Signal: strings.Repeat("x", maxSignalBytes+10)})
	writeMsg(t, j, Msg{Type: "signal", Signal: "after"})
	if m := readUntil(t, host, "peer_signal"); m.Signal != "after" {
		t.Fatalf("only the sane signal should arrive: %q", m.Signal[:10])
	}

	// A flood closes the connection.
	closed := false
	for i := 0; i < joinSignalMax+50; i++ {
		if err := j.WriteJSON(Msg{Type: "signal", Signal: "x"}); err != nil {
			closed = true
			break
		}
	}
	if !closed {
		j.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			if _, _, err := j.ReadMessage(); err != nil {
				closed = true
				break
			}
		}
	}
	if !closed {
		t.Fatal("a flood of setup messages should end the connection")
	}
}

func TestLimitsOnConnectionsPerAddressAndPerHost(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000009")
	code := g.enable(t, host)

	g.hub.maxPerIP = 2
	a, b := g.joiner(t), g.joiner(t)
	for _, j := range []*websocket.Conn{a, b} {
		writeMsg(t, j, Msg{Type: "join", Code: code})
		if m := readMsg(t, j); m.Type != "joined" {
			t.Fatalf("%+v", m)
		}
	}
	c := g.joiner(t)
	if m := readMsg(t, c); m.Type != "join_error" || !strings.Contains(m.Message, "Too many connections") {
		t.Fatalf("the third from one address should be refused: %+v", m)
	}
	// Leaving frees a place.
	writeMsg(t, a, Msg{Type: "leave"})
	time.Sleep(100 * time.Millisecond)
	d := g.joiner(t)
	writeMsg(t, d, Msg{Type: "join", Code: code})
	if m := readMsg(t, d); m.Type != "joined" {
		t.Fatalf("a place should be free again: %+v", m)
	}

	// Per host.
	g.hub.maxPerIP = 1000
	var open []*websocket.Conn
	for i := 0; i < maxPeersPerHost; i++ {
		j := g.joiner(t)
		writeMsg(t, j, Msg{Type: "join", Code: code})
		m := readMsg(t, j)
		if m.Type == "joined" {
			open = append(open, j)
		}
	}
	extra := g.joiner(t)
	writeMsg(t, extra, Msg{Type: "join", Code: code})
	if m := readMsg(t, extra); m.Type != "join_error" || !strings.Contains(m.Message, "too many people") {
		t.Fatalf("a full host should refuse more: %+v", m)
	}
	_ = open
}

func TestAJoinerThatGoesSilentOrStaysTooLongIsDropped(t *testing.T) {
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000010")
	code := g.enable(t, host)

	g.hub.idle = 150 * time.Millisecond
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: code})
	readMsg(t, j)
	j.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := j.ReadMessage(); err == nil {
		t.Fatal("expected the silent connection to be closed")
	}

	g.hub.idle = time.Minute
	g.hub.lifetime = 250 * time.Millisecond
	k := g.joiner(t)
	writeMsg(t, k, Msg{Type: "join", Code: code})
	readMsg(t, k)
	stop := time.After(2 * time.Second)
	for {
		select {
		case <-stop:
			t.Fatal("the connection outlived its lifetime")
		default:
		}
		k.WriteJSON(Msg{Type: "ping"}) // keeps it from being idle
		k.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, _, err := k.ReadMessage(); err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				continue
			}
			return
		}
	}
}

func TestWithoutAJoinHubTheFeatureIsSimplyOff(t *testing.T) {
	g := newJoinRig(t, false)
	host, _ := g.host(t, "vp_joinhost0000011")
	on := true
	writeMsg(t, host, Msg{Type: "p2p", P2P: &on})
	if m := readUntil(t, host, "join_error"); !strings.Contains(m.Message, "aren't available") {
		t.Fatalf("unexpected %+v", m)
	}
	j := g.joiner(t)
	if m := readMsg(t, j); m.Type != "join_error" {
		t.Fatalf("a joiner should be told too: %+v", m)
	}
}

func TestAJoinerMustStartWithAJoinFrame(t *testing.T) {
	g := newJoinRig(t, true)
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "signal", Signal: "x"})
	if m := readMsg(t, j); m.Type != "join_error" {
		t.Fatalf("unexpected %+v", m)
	}
}

func TestCodesAndAddressesNeverReachTheLog(t *testing.T) {
	logs := captureLog(t)
	g := newJoinRig(t, true)
	host, _ := g.host(t, "vp_joinhost0000012")
	code := g.enable(t, host)
	j := g.joiner(t)
	writeMsg(t, j, Msg{Type: "join", Code: code})
	readMsg(t, j)
	readUntil(t, host, "peer")
	wrong := g.joiner(t)
	writeMsg(t, wrong, Msg{Type: "join", Code: "99999-99999"})
	readMsg(t, wrong)
	writeMsg(t, host, Msg{Type: "p2p_rotate"})
	readUntil(t, host, "join_code")
	waitForLog(t, logs, "[join]", "code rotated")

	out := logs.String()
	if strings.Contains(out, code) || strings.Contains(out, strings.ReplaceAll(code, "-", "")) || strings.Contains(out, "99999") {
		t.Fatalf("a code was logged:\n%s", out)
	}
	if strings.Contains(out, "127.0.0.1") {
		t.Fatalf("an IP address was logged:\n%s", out)
	}
}

func TestTheHubSavesOnlyItsKeyAndRotationCounts(t *testing.T) {
	dir := t.TempDir()
	h, _ := newJoinHub(dir)
	a := &client{id: strings.Repeat("a", 64)}
	code := h.enable(a)
	h.rotate(a)
	for _, name := range []string{"join.key", "join-epochs.json"} {
		raw := mustRead(t, dir+"/"+name)
		if strings.Contains(raw, code) {
			t.Fatalf("%s must not contain a code", name)
		}
	}
	if raw := mustRead(t, dir+"/join-epochs.json"); !strings.Contains(raw, `"`+a.id+`":1`) {
		t.Fatalf("the rotation count should be saved: %s", raw)
	}
}

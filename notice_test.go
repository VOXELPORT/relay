package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// syncBuf is a log sink that is safe to write from the relay's goroutines and read from a test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog sends the relay's log into a buffer for the length of the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

// waitForLog waits until the log contains every part, and returns the line that has the first part.
func waitForLog(t *testing.T, buf *syncBuf, parts ...string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(buf.String(), "\n") {
			ok := true
			for _, p := range parts {
				if !strings.Contains(line, p) {
					ok = false
					break
				}
			}
			if ok {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log never had a line with %q. Log was:\n%s", parts, buf.String())
	return ""
}

// registerHost connects a host, reads its port frame, and returns the socket.
func registerHost(t *testing.T, srv *httptest.Server, token, client string) *websocket.Conn {
	t.Helper()
	ws := dialWS(t, srv)
	writeMsg(t, ws, Msg{Type: "register", Token: token, Client: client})
	if m := readMsg(t, ws); m.Type != "port" {
		t.Fatalf("expected a port frame, got %+v", m)
	}
	return ws
}

// readNotice reads frames until a notice arrives (other frames, like a custom name, may come first).
func readNotice(t *testing.T, ws *websocket.Conn) Msg {
	t.Helper()
	for i := 0; i < 5; i++ {
		if m := readMsg(t, ws); m.Type == "notice" {
			return m
		}
	}
	t.Fatalf("no notice arrived")
	return Msg{}
}

// ─── Why a tunnel ended, and who connected ──────────────────────────────────────────────────────

func TestUsageLogSaysWhyTheTunnelEnded(t *testing.T) {
	buf := captureLog(t)
	srv, _ := newTestServerWithConfig(testConfig())
	defer srv.Close()

	// 1. The host's app closes the tunnel properly.
	ws := registerHost(t, srv, "vp_endquit_aaaaaaaaaaaaaaaaaaaa", "desktop/1.7.0")
	ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	ws.Close()
	line := waitForLog(t, buf, "[usage]", "ended=host-quit", "client=desktop/1.7.0")
	if !strings.Contains(line, "route=tunnel") {
		t.Fatalf("a plain (non-TLS) connection is the tunnel route: %s", line)
	}

	// 2. The connection dies with no goodbye (network dropped, laptop closed).
	ws = registerHost(t, srv, "vp_endlost_bbbbbbbbbbbbbbbbbbbb", "android/10")
	ws.UnderlyingConn().Close()
	waitForLog(t, buf, "[usage]", "ended=lost", "client=android/10")
}

func TestReconnectingHostReplacesItsOldConnection(t *testing.T) {
	buf := captureLog(t)
	srv, _ := newTestServerWithConfig(testConfig())
	defer srv.Close()

	first := registerHost(t, srv, "vp_replaced_cccccccccccccccccccc", "desktop/1.7.0")
	defer first.Close()
	// The host's network blipped, so it connects again while the relay still thinks the first is alive.
	second := registerHost(t, srv, "vp_replaced_cccccccccccccccccccc", "desktop/1.7.0")
	defer second.Close()

	waitForLog(t, buf, "[usage]", "ended=replaced")
	waitForLog(t, buf, "tunnel up", "replaced-a-dead-connection")
}

func TestLogSaysHowLongAHostWasGone(t *testing.T) {
	buf := captureLog(t)
	srv, _ := newTestServerWithConfig(testConfig())
	defer srv.Close()

	ws := registerHost(t, srv, "vp_cameback_dddddddddddddddddddd", "cli/0.4")
	ws.Close()
	waitForLog(t, buf, "[usage]", "ended=")
	time.Sleep(1100 * time.Millisecond)
	again := registerHost(t, srv, "vp_cameback_dddddddddddddddddddd", "cli/0.4")
	defer again.Close()
	waitForLog(t, buf, "tunnel up", "back-after=")
}

func TestAClientCannotWriteFreeTextIntoTheLog(t *testing.T) {
	buf := captureLog(t)
	srv, _ := newTestServerWithConfig(testConfig())
	defer srv.Close()

	ws := registerHost(t, srv, "vp_evilname_eeeeeeeeeeeeeeeeeeee", "desktop/1.7.0\n2026/01/01 [relay] fake line: all tokens banned")
	defer ws.Close()
	line := waitForLog(t, buf, "tunnel up", "client=unknown")
	if strings.Contains(buf.String(), "fake line") {
		t.Fatalf("a client wrote into the log: %s", buf.String())
	}
	_ = line

	for _, name := range []string{"", "x y", "A/1.0", "desktop/" + strings.Repeat("9", 40), "desktop/1.7.0/extra"} {
		if got := cleanClientName(name); got != "unknown" {
			t.Errorf("cleanClientName(%q) = %q, want unknown", name, got)
		}
	}
	for _, name := range []string{"desktop/1.7.0", "android/10", "cli/0.4.2", "mod/1.4.8", "app"} {
		if got := cleanClientName(name); got != name {
			t.Errorf("cleanClientName(%q) = %q, want it kept", name, got)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassifyReadError(t *testing.T) {
	var _ net.Error = timeoutError{}
	cases := []struct {
		err  error
		want string
	}{
		{&websocket.CloseError{Code: websocket.CloseNormalClosure}, endHostQuit},
		{&websocket.CloseError{Code: websocket.CloseGoingAway}, endHostQuit},
		{&websocket.CloseError{Code: websocket.CloseAbnormalClosure}, endLost},
		{timeoutError{}, endSilent},
		{errors.New("read tcp: connection reset by peer"), endLost},
	}
	for _, c := range cases {
		if got := classifyReadError(c.err); got != c.want {
			t.Errorf("classifyReadError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestFirstReasonWins(t *testing.T) {
	c := &client{}
	c.endWith(endReplaced)
	c.endWith(endLost) // the read loop failing right after the replacement must not rewrite why
	if got := c.endReason(); got != endReplaced {
		t.Fatalf("reason = %q, want %q", got, endReplaced)
	}
	if got := (&client{}).endReason(); got != endLost {
		t.Fatalf("with no reason set the answer is %q, want %q", got, endLost)
	}
}

// ─── Announcements to hosts ─────────────────────────────────────────────────────────────────────

func TestNoticeReachesConnectedHostsAndLateOnes(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()

	early := registerHost(t, srv, "vp_noticeearly_ffffffffffffff", "desktop/1.7.0")
	defer early.Close()

	sent, err := r.setNotice("The relay restarts in 5 minutes.", 5)
	if err != nil || sent != 1 {
		t.Fatalf("setNotice = %d, %v; want 1, nil", sent, err)
	}
	m := readNotice(t, early)
	if m.Message != "The relay restarts in 5 minutes." || m.Seconds < 295 || m.Seconds > 300 {
		t.Fatalf("early host got %+v", m)
	}

	// A host that connects afterwards is told too, with the time that is left.
	late := registerHost(t, srv, "vp_noticelate_gggggggggggggg", "android/10")
	defer late.Close()
	m = readNotice(t, late)
	if m.Message != "The relay restarts in 5 minutes." || m.Seconds < 290 || m.Seconds > 300 {
		t.Fatalf("late host got %+v", m)
	}
}

func TestClearingANoticeTellsHostsAndStopsTellingNewOnes(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()

	host := registerHost(t, srv, "vp_noticeclear_hhhhhhhhhhhhhh", "desktop/1.7.0")
	defer host.Close()
	if _, err := r.setNotice("Maintenance soon.", 10); err != nil {
		t.Fatal(err)
	}
	readNotice(t, host)

	if told := r.clearNotice(); told != 1 {
		t.Fatalf("clearNotice told %d hosts, want 1", told)
	}
	if m := readNotice(t, host); m.Message != "" {
		t.Fatalf("the clear frame must have an empty message, got %+v", m)
	}
	if _, _, ok := r.activeNotice(); ok {
		t.Fatal("the notice is still active after clearing")
	}
	if told := r.clearNotice(); told != 0 {
		t.Fatalf("clearing nothing told %d hosts", told)
	}
}

func TestUnsafeNoticesAreRefused(t *testing.T) {
	_, r := newTestServerWithConfig(testConfig())
	for name, msg := range map[string]string{
		"empty":        "   ",
		"too long":     strings.Repeat("a", maxNoticeLen+1),
		"line break":   "first line\nsecond line",
		"control char": "bell\x07here",
	} {
		if _, err := r.setNotice(msg, 5); err == nil {
			t.Errorf("%s: the notice was accepted", name)
		}
	}
	for _, minutes := range []int{-1, maxNoticeMinutes + 1} {
		if _, err := r.setNotice("Fine message.", minutes); err == nil {
			t.Errorf("%d minutes was accepted", minutes)
		}
	}
	if _, _, ok := r.activeNotice(); ok {
		t.Fatal("a refused notice became active")
	}
	if _, err := r.setNotice("Café is closed, 再见", 0); err != nil {
		t.Fatalf("ordinary non-English text was refused: %v", err)
	}
}

func TestNoticesExpire(t *testing.T) {
	_, r := newTestServerWithConfig(testConfig())
	if _, err := r.setNotice("Restart at the time.", 5); err != nil {
		t.Fatal(err)
	}
	r.noticeMu.Lock()
	r.notice.at = time.Now().Add(-noticeAfterEvent - time.Minute)
	r.noticeMu.Unlock()
	if _, _, ok := r.activeNotice(); ok {
		t.Fatal("a notice more than 10 minutes past its time is still active")
	}

	if _, err := r.setNotice("Just so you know.", 0); err != nil {
		t.Fatal(err)
	}
	r.noticeMu.Lock()
	r.notice.sentAt = time.Now().Add(-noticeInfoLifetime - time.Minute)
	r.noticeMu.Unlock()
	if _, _, ok := r.activeNotice(); ok {
		t.Fatal("an information notice outlived its lifetime")
	}
}

func TestPublicStatusShowsTheActiveNotice(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()
	get := func() map[string]any {
		resp, err := http.Get(srv.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if _, has := get()["notice"]; has {
		t.Fatal("status shows a notice when there is none")
	}
	if _, err := r.setNotice("Relay restart soon.", 3); err != nil {
		t.Fatal(err)
	}
	n, ok := get()["notice"].(map[string]any)
	if !ok || n["message"] != "Relay restart soon." {
		t.Fatalf("status notice = %v", get()["notice"])
	}
	if left, _ := n["seconds_left"].(float64); left < 170 || left > 180 {
		t.Fatalf("seconds_left = %v, want about 180", n["seconds_left"])
	}
	r.clearNotice()
	if _, has := get()["notice"]; has {
		t.Fatal("status still shows a cleared notice")
	}
}

func TestOperatorActionsSendAndClearNotices(t *testing.T) {
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()
	host := registerHost(t, srv, "vp_noticeadmin_iiiiiiiiiiiiii", "desktop/1.7.0")
	defer host.Close()

	if err := r.adminDo(adminAction{Action: "notice", Message: "Restarting in 2 minutes.", Minutes: 2}); err != nil {
		t.Fatal(err)
	}
	if m := readNotice(t, host); m.Message != "Restarting in 2 minutes." {
		t.Fatalf("host got %+v", m)
	}
	if err := r.adminDo(adminAction{Action: "notice", Message: "", Minutes: 2}); err == nil {
		t.Fatal("an empty notice was accepted")
	}
	if err := r.adminDo(adminAction{Action: "clear_notice"}); err != nil {
		t.Fatal(err)
	}
	if m := readNotice(t, host); m.Message != "" {
		t.Fatalf("expected the clear frame, got %+v", m)
	}
}

func TestStoppingTheRelayTellsHostsFirst(t *testing.T) {
	buf := captureLog(t)
	srv, r := newTestServerWithConfig(testConfig())
	defer srv.Close()
	host := registerHost(t, srv, "vp_stopnotice_jjjjjjjjjjjjjjj", "desktop/1.7.0")
	defer host.Close()

	r.shutdown()
	m := readNotice(t, host)
	if !strings.Contains(m.Message, "restarting") {
		t.Fatalf("host got %+v", m)
	}
	waitForLog(t, buf, "[usage]", "ended=relay-stop")
}

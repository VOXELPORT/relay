package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ─── Is this an address a router could have opened? ─────────────────────────────

func TestPublicIPv4(t *testing.T) {
	for _, ip := range []string{"8.8.8.8", "83.45.12.9", "49.36.1.1", "172.32.0.1", "100.63.255.255", "100.128.0.1", "198.20.0.1"} {
		if !publicIPv4(ip) {
			t.Errorf("%s should be public", ip)
		}
	}
	for _, ip := range []string{
		"", "nope", "10.0.0.5", "192.168.1.20", "172.16.0.1", "172.31.255.255", "127.0.0.1", "169.254.1.1",
		"100.64.0.1", "100.127.255.255", // shared (CGNAT) addresses: nobody can reach a host behind one
		"0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1", "203.0.113.5",
		"::1", "2001:db8::1", "2a01:4f8::1", // IPv6 can't go in an A record
	} {
		if publicIPv4(ip) {
			t.Errorf("%s should not be accepted", ip)
		}
	}
}

// ─── Asking a Minecraft server from outside ─────────────────────────────────────

func packetWith(id byte, payload []byte) []byte {
	body := append([]byte{id}, payload...)
	return append(putVarInt(nil, int32(len(body))), body...)
}

func statusPacket(json string) []byte {
	return packetWith(0, append(putVarInt(nil, int32(len(json))), json...))
}

// fakeMinecraft answers with reply (read after the client's request), and hands what it received to got.
func fakeMinecraft(t *testing.T, reply []byte, got chan<- []byte) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				buf := make([]byte, 512)
				n, _ := io.ReadAtLeast(c, buf, 1)
				if got != nil {
					select {
					case got <- append([]byte(nil), buf[:n]...):
					default:
					}
				}
				c.Write(reply)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func probe(port int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return probeMinecraft(ctx, "127.0.0.1", port)
}

func TestProbeAcceptsAMinecraftServerAndSendsAStatusRequest(t *testing.T) {
	got := make(chan []byte, 1)
	port := fakeMinecraft(t, statusPacket(`{"version":{"name":"1.21.11","protocol":767},"players":{"max":20,"online":1},"description":"hi"}`), got)
	if err := probe(port); err != nil {
		t.Fatalf("a real status reply was refused: %v", err)
	}
	sent := <-got
	// handshake: length, id 0, protocol varint, host, port, next state 1, then the status request 01 00.
	if len(sent) < 8 || sent[1] != 0x00 {
		t.Fatalf("the first packet should be a handshake, got % x", sent)
	}
	if !strings.HasSuffix(string(sent), "\x01\x00") {
		t.Fatalf("the status request is missing: % x", sent)
	}
	if binary.BigEndian.Uint16(sent[len(sent)-5:len(sent)-3]) != uint16(port) {
		t.Fatalf("the handshake should carry the port it connected to: % x", sent)
	}
	if sent[len(sent)-3] != 0x01 {
		t.Fatalf("next state should be status (1): % x", sent)
	}
}

func TestProbeRefusesAnythingThatIsNotAMinecraftServer(t *testing.T) {
	cases := map[string][]byte{
		"an HTTP server":                  []byte("HTTP/1.1 400 Bad Request\r\n\r\n"),
		"silence":                         nil,
		"garbage":                         {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"a status with no version":        statusPacket(`{"hello":"world"}`),
		"a status that is not JSON":       statusPacket(`not json at all`),
		"the wrong packet":                packetWith(1, []byte{0, 0, 0, 0, 0, 0, 0, 0}),
		"a length bigger than allowed":    putVarInt(nil, maxStatusBytes+1),
		"a length with nothing after it":  putVarInt(nil, 50),
		"a string longer than the packet": packetWith(0, append(putVarInt(nil, 9999), 'x')),
	}
	for name, reply := range cases {
		port := fakeMinecraft(t, reply, nil)
		if err := probe(port); err == nil {
			t.Errorf("%s was accepted as a Minecraft server", name)
		}
	}
}

func TestProbeFailsWhenNothingListens(t *testing.T) {
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := probe(port); err == nil {
		t.Fatal("a closed port was accepted")
	}
}

// ─── Direct mode through the relay ──────────────────────────────────────────────

type directRig struct {
	dns *fakeDNS
	r   *relay
	dir string
	ok  atomic.Bool // whether the pretend server answers the relay's checks
	// blip is how many of the next checks fail before the pretend server answers again (set it, never replace the relay's
	// check function while the relay's background checks are running: that would be a data race).
	blip atomic.Int32
}

// newDirectRig is a relay with a fake DNS whose public-address check and outside probe are replaced,
// so tests don't need a real public address.
func newDirectRig(t *testing.T) (*directRig, func(token string) *wsHost) {
	t.Helper()
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27300, 27340)
	srv, r := newFeatureServer(cfg)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { r.directWG.Wait() }) // runs after the hosts disconnect, before the temp folder goes
	rig := &directRig{dns: dns, r: r, dir: dir}
	rig.ok.Store(true)
	r.directAllowIP = func(string) bool { return true }
	r.directProbeFn = func(context.Context, string, int) error {
		if rig.blip.Load() > 0 {
			rig.blip.Add(-1)
			return errors.New("a blip")
		}
		if rig.ok.Load() {
			return nil
		}
		return errors.New("nothing answered")
	}
	r.directEvery = 25 * time.Millisecond
	r.directRetry = 10 * time.Millisecond
	connect := func(token string) *wsHost {
		ws, port := register(t, srv, token, false)
		t.Cleanup(func() { ws.Close() })
		return &wsHost{t: t, ws: ws, port: port, id: clientIDFor(token)}
	}
	return rig, connect
}

type wsHost struct {
	t    *testing.T
	ws   *websocket.Conn
	port int
	id   string
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func srvID(r *relay, clientID string) string {
	e, ok := r.names.forClient(clientID)
	if !ok || len(e.Records) < 2 {
		return ""
	}
	return e.Records[1]
}

func (r *relay) entryDirect(clientID string) bool {
	e, ok := r.names.forClient(clientID)
	return ok && e.Direct
}

func TestDirectModeSendsANamedAddressStraightToTheHost(t *testing.T) {
	logs := captureLog(t)
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0001")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "steve"})
	readUntil(t, ws, "name")

	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	m := readUntil(t, ws, "direct")
	if m.IP != "127.0.0.1" || m.Port != 25565 || m.Address != "steve.example.test" {
		t.Fatalf("unexpected direct frame %+v", m)
	}
	srv := srvID(rig.r, h.id)
	if got, ok := rig.dns.pointsAt(srv); !ok || got.ip != "127.0.0.1" || got.port != 25565 {
		t.Fatalf("the address should point straight at the host, got %+v ok=%v", got, ok)
	}
	if !rig.r.entryDirect(h.id) {
		t.Fatal("the name should be marked as direct")
	}

	// The host's IP address is public in DNS, but must never reach the relay's disk or its log.
	raw, err := os.ReadFile(filepath.Join(rig.dir, "names.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "127.0.0.1") {
		t.Fatalf("the host's IP address was written to disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"direct": true`) {
		t.Fatalf("the saved state should record that the address is direct:\n%s", raw)
	}
	waitForLog(t, logs, "[direct]", "up", "port=25565", "named=true")
	if strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatalf("the host's IP address was logged:\n%s", logs.String())
	}
}

func TestDirectModeWithoutAnAddressStillVerifiesAndTellsTheHost(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0002")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	m := readUntil(t, ws, "direct")
	if m.IP != "127.0.0.1" || m.Port != 25565 || m.Address != "" {
		t.Fatalf("unexpected direct frame %+v", m)
	}
	if rig.dns.hasA() {
		t.Fatal("no DNS should be touched for a host without an address")
	}
	if !rig.r.hostWantsDirect(h.id) {
		t.Fatal("the host should count as direct")
	}
}

func TestDirectModeIsRefusedWhenThePortIsNotReachable(t *testing.T) {
	rig, connect := newDirectRig(t)
	rig.ok.Store(false)
	h := connect("vp_directhost0003")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "alex"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	m := readUntil(t, ws, "direct_error")
	if !strings.Contains(m.Message, "25565") || !strings.Contains(m.Message, "nothing answered") {
		t.Fatalf("the host should be told what failed: %+v", m)
	}
	if rig.dns.hasA() || rig.r.entryDirect(h.id) || rig.r.hostWantsDirect(h.id) {
		t.Fatal("a failed check must change nothing")
	}
}

func TestDirectModeIsRefusedForANonPublicAddress(t *testing.T) {
	rig, connect := newDirectRig(t)
	rig.r.directAllowIP = nil // the real check: the tests connect from 127.0.0.1, and loopback is not public
	h := connect("vp_directhost0004")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	m := readUntil(t, ws, "direct_error")
	if !strings.Contains(m.Message, "public IPv4") {
		t.Fatalf("expected the CGNAT/VPN explanation, got %+v", m)
	}
}

func TestDirectModeRefusesBadPortsAndTooManyTries(t *testing.T) {
	rig, connect := newDirectRig(t)
	rig.ok.Store(false)
	h := connect("vp_directhost0005")
	ws := h.ws
	for _, port := range []int{80, 1023, 70000} {
		writeMsg(t, ws, Msg{Type: "direct", Port: port})
		if m := readUntil(t, ws, "direct_error"); !strings.Contains(m.Message, "can't be used") {
			t.Fatalf("port %d: %+v", port, m)
		}
	}
	for i := 0; i < directAskLimit; i++ {
		writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
		readUntil(t, ws, "direct_error")
	}
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	if m := readUntil(t, ws, "direct_error"); !strings.Contains(m.Message, "Too many") {
		t.Fatalf("the host should be slowed down: %+v", m)
	}
}

func TestTurningDirectModeOffGoesBackToTheRelay(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0006")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "maya"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	srv := srvID(rig.r, h.id)

	writeMsg(t, ws, Msg{Type: "direct", Port: 0})
	if m := readUntil(t, ws, "direct"); m.Port != 0 {
		t.Fatalf("expected the host to be told Direct mode is off: %+v", m)
	}
	eventually(t, "the address to point at the relay again", func() bool {
		_, direct := rig.dns.pointsAt(srv)
		return !direct && !rig.dns.hasA() && !rig.r.entryDirect(h.id)
	})
	rig.dns.mu.Lock()
	back := rig.dns.ports[srv]
	rig.dns.mu.Unlock()
	if back != h.port {
		t.Fatalf("the address should point at the relay port %d, got %d", h.port, back)
	}
}

func TestAHostThatLeavesTakesItsIPOutOfDNS(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0007")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "kira"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	srv := srvID(rig.r, h.id)
	if _, ok := rig.dns.pointsAt(srv); !ok {
		t.Fatal("setup: the address should be direct")
	}
	ws.Close()
	eventually(t, "the host's IP to leave DNS", func() bool {
		_, direct := rig.dns.pointsAt(srv)
		return !direct && !rig.dns.hasA()
	})
}

func TestTheAddressFallsBackWhenTheHostStopsAnsweringAndComesBack(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0008")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "omar"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	srv := srvID(rig.r, h.id)

	rig.ok.Store(false) // the router restarted, the port closed
	if m := readUntil(t, ws, "direct"); m.Port != 0 {
		t.Fatalf("the host should be told players are back on the relay: %+v", m)
	}
	if m := readUntil(t, ws, "direct_error"); !strings.Contains(m.Message, "relay again") {
		t.Fatalf("unexpected message %+v", m)
	}
	eventually(t, "the address to point at the relay", func() bool {
		_, direct := rig.dns.pointsAt(srv)
		return !direct
	})

	rig.ok.Store(true) // fixed
	if m := readUntil(t, ws, "direct"); m.Port != 25565 || m.Address != "omar.example.test" {
		t.Fatalf("direct should resume by itself: %+v", m)
	}
	eventually(t, "the address to point at the host again", func() bool {
		_, direct := rig.dns.pointsAt(srv)
		return direct
	})
}

func TestOneMissedCheckDoesNotSendPlayersBack(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0009")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	rig.blip.Store(1) // the next check fails once, then the server answers again
	time.Sleep(300 * time.Millisecond)
	if !rig.r.hostWantsDirect(h.id) {
		t.Fatal("a single missed check should not end Direct mode")
	}
}

func TestADNSFailureWhileLeavingIsRetried(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0010")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "lena"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	srv := srvID(rig.r, h.id)

	rig.dns.mu.Lock()
	rig.dns.failClear = 2
	rig.dns.mu.Unlock()
	writeMsg(t, ws, Msg{Type: "direct", Port: 0})
	eventually(t, "the clean-up to succeed after two failures", func() bool {
		_, direct := rig.dns.pointsAt(srv)
		return !direct && !rig.dns.hasA()
	})
}

func TestADNSFailureWhileSwitchingIsRetriedByTheCheck(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0011")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "ravi"})
	readUntil(t, ws, "name")
	rig.dns.mu.Lock()
	rig.dns.failSet = 1
	rig.dns.mu.Unlock()
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct") // verified, even though DNS wasn't switched yet
	srv := srvID(rig.r, h.id)
	eventually(t, "the address to be switched on a later check", func() bool {
		_, direct := rig.dns.pointsAt(srv)
		return direct
	})
}

func TestClaimingANameWhileDirectPointsItAtTheHost(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0012")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")

	writeMsg(t, ws, Msg{Type: "claim_name", Name: "tara"})
	readUntil(t, ws, "name")
	if m := readUntil(t, ws, "direct"); m.Address != "tara.example.test" {
		t.Fatalf("the host should be told its new address is direct: %+v", m)
	}
	srv := srvID(rig.r, h.id)
	if got, ok := rig.dns.pointsAt(srv); !ok || got.port != 25565 {
		t.Fatalf("the new address should point at the host, got %+v ok=%v", got, ok)
	}
}

func TestReleasingTheNameWhileDirectRemovesItsRecords(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0013")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "ines"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")

	writeMsg(t, ws, Msg{Type: "release_name"})
	readUntil(t, ws, "name")
	if m := readUntil(t, ws, "direct"); m.Address != "" || m.Port != 25565 {
		t.Fatalf("the host stays direct, now without an address: %+v", m)
	}
	if rig.dns.hasA() {
		t.Fatal("releasing the address must delete the A record too")
	}
}

func TestAPortChangeDoesNotPullADirectAddressBackToTheRelay(t *testing.T) {
	rig, connect := newDirectRig(t)
	h := connect("vp_directhost0014")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "nico"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	srv := srvID(rig.r, h.id)

	rig.r.names.repoint(h.id, h.port+7)
	if _, ok := rig.dns.pointsAt(srv); !ok {
		t.Fatal("repointing the relay port must not undo Direct mode")
	}
	if e, _ := rig.r.names.forClient(h.id); e.Port != h.port+7 {
		t.Fatalf("the remembered relay port should still update, got %d", e.Port)
	}
}

func TestAQuickReconnectIsNotUndoneByTheOldSessionsCleanUp(t *testing.T) {
	rig, connect := newDirectRig(t)
	// The old session's clean-up fails once and retries later, after the new session is already direct: the
	// retry must see that and leave the new session alone.
	rig.dns.failClear = 1
	rig.r.directRetry = 150 * time.Millisecond
	h := connect("vp_directhost0015")
	ws := h.ws
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "yuki"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	srv := srvID(rig.r, h.id)

	// The same host connects again (a network blip): its old tunnel is replaced, which cleans up in the background.
	h2 := connect("vp_directhost0015")
	ws2 := h2.ws
	readUntil(t, ws2, "name")
	writeMsg(t, ws2, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws2, "direct")

	time.Sleep(700 * time.Millisecond)
	if _, ok := rig.dns.pointsAt(srv); !ok || !rig.r.entryDirect(h.id) {
		t.Fatal("the new session's Direct mode was undone by the old session's clean-up")
	}
}

func TestAddressesLeftDirectByARestartArePutBack(t *testing.T) {
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27400, 27410)
	srv, r := newFeatureServer(cfg)
	r.directAllowIP = func(string) bool { return true }
	r.directProbeFn = func(context.Context, string, int) error { return nil }
	t.Cleanup(func() { r.directWG.Wait() }) // runs after the host below disconnects, before the temp folder goes
	ws, _ := register(t, srv, "vp_directhost0016", false)
	t.Cleanup(func() { ws.Close() })
	writeMsg(t, ws, Msg{Type: "claim_name", Name: "juno"})
	readUntil(t, ws, "name")
	writeMsg(t, ws, Msg{Type: "direct", Port: 25565})
	readUntil(t, ws, "direct")
	id := clientIDFor("vp_directhost0016")
	rec := srvID(r, id)
	// Pretend the relay was killed here: the record still points at the host, and names.json says so.
	srv.Close()

	store, err := loadNameStore(filepath.Join(dir, "names.json"), "example.test", dns)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.directIDs(); len(got) != 1 || got[0] != id {
		t.Fatalf("the restarted relay should remember which addresses were direct, got %v", got)
	}
	cfg2 := testConfig()
	cfg2.Names, cfg2.PublicHost = store, "play.example.test"
	cfg2.Hidden = newHiddenList(filepath.Join(dir, "hidden2.txt"))
	r2 := newRelay(cfg2)
	r2.directCleanup = 20 * time.Millisecond
	r2.directRetry = 10 * time.Millisecond
	r2.startDirectCleanup()
	t.Cleanup(func() { r2.directWG.Wait() })
	eventually(t, "the leftover direct address to be put back on the relay", func() bool {
		_, direct := dns.pointsAt(rec)
		return !direct && !dns.hasA() && len(store.directIDs()) == 0
	})
}

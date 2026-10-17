package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeDNS records what the name store asked for, without touching Cloudflare.
type fakeDNS struct {
	mu       sync.Mutex
	existing map[string]bool // names that already have records "outside" VoxelPort
	ports    map[string]int  // srv record id -> port
	deleted  []string
	repoints []int // ports records were moved to
	next     int
}

func newFakeDNS() *fakeDNS {
	return &fakeDNS{existing: map[string]bool{}, ports: map[string]int{}}
}

func (f *fakeDNS) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existing[name], nil
}

func (f *fakeDNS) Create(_ context.Context, name string, port int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	cname, srv := fmt.Sprintf("cname-%d", f.next), fmt.Sprintf("srv-%d", f.next)
	f.ports[srv] = port
	return []string{cname, srv}, nil
}

func (f *fakeDNS) Repoint(_ context.Context, records []string, _ string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ports[records[1]] = port
	f.repoints = append(f.repoints, port)
	return nil
}

func (f *fakeDNS) Delete(_ context.Context, records []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, records...)
	for _, id := range records {
		delete(f.ports, id)
	}
	return nil
}

func featureConfig(t *testing.T, dns *fakeDNS, minPort, maxPort int) (relayConfig, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := loadNameStore(filepath.Join(dir, "names.json"), "example.test", dns)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.MinPort, cfg.MaxPort = minPort, maxPort
	cfg.Names = store
	cfg.PublicHost = "play.example.test"
	cfg.Hidden = newHiddenList(filepath.Join(dir, "hidden.txt"))
	return cfg, dir
}

func newFeatureServer(cfg relayConfig) (*httptest.Server, *relay) {
	r := newRelay(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/api/servers", r.apiServers)
	return httptest.NewServer(mux), r
}

func register(t *testing.T, srv *httptest.Server, token string, udp bool) (*websocket.Conn, int) {
	t.Helper()
	ws := dialWS(t, srv)
	writeMsg(t, ws, Msg{Type: "register", Token: token, UDP: udp})
	m := readMsg(t, ws)
	if m.Type != "port" {
		t.Fatalf("expected port, got %+v", m)
	}
	return ws, m.Port
}

// readUntil skips frames until one of type typ arrives.
func readUntil(t *testing.T, ws *websocket.Conn, typ string) Msg {
	t.Helper()
	for i := 0; i < 20; i++ {
		m := readMsg(t, ws)
		if m.Type == typ {
			return m
		}
	}
	t.Fatalf("no %s frame", typ)
	return Msg{}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"steve", "abc", "my-smp", "a1b2c3", "yogita-smp-2026"} {
		if err := validateName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "-steve", "steve-", "Steve", "my--smp", "a_b", "play", "www", "relay",
		"verylongname-over-twenty", "fuckland", "sh1tcraft", "my smp", "stëve"} {
		if err := validateName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, innocent := range []string{"essex", "peacock", "grapes", "dickens"} {
		if err := validateName(innocent); err != nil {
			t.Errorf("%q is innocent but was rejected: %v", innocent, err)
		}
	}
}

func TestClaimNameReservesPortAcrossReconnect(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27100, 27102)
	srv, _ := newFeatureServer(cfg)
	defer srv.Close()

	a, portA := register(t, srv, "vp_hostAAAAAAAAAA", false)
	writeMsg(t, a, Msg{Type: "claim_name", Name: "steve"})
	m := readUntil(t, a, "name")
	if m.Name != "steve" || m.Address != "steve.example.test" {
		t.Fatalf("unexpected name frame %+v", m)
	}
	a.Close()
	time.Sleep(200 * time.Millisecond) // let the relay notice the disconnect

	// Someone else must never be handed the named port, even while its owner is offline.
	b, portB := register(t, srv, "vp_hostBBBBBBBBBB", false)
	defer b.Close()
	if portB == portA {
		t.Fatalf("named port %d was given to another host", portA)
	}

	// The owner gets its port and name back on reconnect.
	a2, portA2 := register(t, srv, "vp_hostAAAAAAAAAA", false)
	defer a2.Close()
	if portA2 != portA {
		t.Fatalf("owner reconnected on %d, want its named port %d", portA2, portA)
	}
	if m := readUntil(t, a2, "name"); m.Name != "steve" {
		t.Fatalf("owner wasn't told its name on reconnect: %+v", m)
	}
}

func TestClaimNameRejectsTakenAndForeignNames(t *testing.T) {
	dns := newFakeDNS()
	dns.existing["yogitasmp"] = true // a record VoxelPort didn't create
	cfg, _ := featureConfig(t, dns, 27110, 27115)
	srv, _ := newFeatureServer(cfg)
	defer srv.Close()

	a, _ := register(t, srv, "vp_hostAAAAAAAAAA", false)
	defer a.Close()
	writeMsg(t, a, Msg{Type: "claim_name", Name: "yogitasmp"})
	if m := readUntil(t, a, "name_error"); !strings.Contains(m.Message, "taken") {
		t.Fatalf("expected taken error, got %+v", m)
	}
	writeMsg(t, a, Msg{Type: "claim_name", Name: "relay"})
	if m := readUntil(t, a, "name_error"); !strings.Contains(m.Message, "reserved") {
		t.Fatalf("expected reserved error, got %+v", m)
	}
	writeMsg(t, a, Msg{Type: "claim_name", Name: "alpha"})
	readUntil(t, a, "name")

	b, _ := register(t, srv, "vp_hostBBBBBBBBBB", false)
	defer b.Close()
	writeMsg(t, b, Msg{Type: "claim_name", Name: "alpha"})
	if m := readUntil(t, b, "name_error"); !strings.Contains(m.Message, "taken") {
		t.Fatalf("second host got someone else's name: %+v", m)
	}
}

func TestRenameAndReleaseCleanUpDNS(t *testing.T) {
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27120, 27125)
	srv, _ := newFeatureServer(cfg)
	defer srv.Close()

	a, port := register(t, srv, "vp_hostAAAAAAAAAA", false)
	defer a.Close()
	writeMsg(t, a, Msg{Type: "claim_name", Name: "first"})
	readUntil(t, a, "name")
	writeMsg(t, a, Msg{Type: "claim_name", Name: "second"})
	if m := readUntil(t, a, "name"); m.Name != "second" {
		t.Fatalf("rename failed: %+v", m)
	}
	dns.mu.Lock()
	deletedOld := len(dns.deleted) == 2
	dns.mu.Unlock()
	if !deletedOld {
		t.Fatalf("old name's records weren't deleted: %v", dns.deleted)
	}

	// The store survives a restart.
	reloaded, err := loadNameStore(filepath.Join(dir, "names.json"), "example.test", dns)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := reloaded.forClient(clientIDFor("vp_hostAAAAAAAAAA")); !ok || e.Name != "second" || e.Port != port {
		t.Fatalf("reloaded entry = %+v, %v", e, ok)
	}

	writeMsg(t, a, Msg{Type: "release_name"})
	if m := readUntil(t, a, "name"); m.Name != "" {
		t.Fatalf("release should send an empty name, got %+v", m)
	}
	if cfg.Names.portOwner(port) != "" {
		t.Fatal("port still reserved after release")
	}
}

func TestNameClaimRateLimited(t *testing.T) {
	cfg, _ := featureConfig(t, newFakeDNS(), 27130, 27131)
	id := clientIDFor("vp_hostAAAAAAAAAA")
	for i := 0; i < nameClaimLimit; i++ {
		cfg.Names.claim(id, fmt.Sprintf("name%d", i), 27130)
	}
	if _, err := cfg.Names.claim(id, "onemore", 27130); err == nil || !strings.Contains(err.Error(), "Too many") {
		t.Fatalf("expected rate limit, got %v", err)
	}
}

func TestExpiredNamesAreReleasedButLiveOnesKept(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27140, 27141)
	s := cfg.Names
	if _, err := s.claim("aaaaaaaaaaaa", "oldone", 27140); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim("bbbbbbbbbbbb", "liveone", 27141); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	for _, e := range s.byName {
		e.LastSeen = time.Now().Add(-100 * 24 * time.Hour)
	}
	s.mu.Unlock()
	s.sweepExpired(nameExpiry, func(id string) bool { return id == "bbbbbbbbbbbb" })
	if _, ok := s.forClient("aaaaaaaaaaaa"); ok {
		t.Fatal("stale name not expired")
	}
	if _, ok := s.forClient("bbbbbbbbbbbb"); !ok {
		t.Fatal("live host's name was expired")
	}
}

func TestUDPBridgeRoundTrip(t *testing.T) {
	cfg, _ := featureConfig(t, newFakeDNS(), 27150, 27155)
	srv, _ := newFeatureServer(cfg)
	defer srv.Close()

	host, port := register(t, srv, "vp_hostAAAAAAAAAA", true)
	defer host.Close()

	player, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()

	ping := []byte("unconnected-ping")
	player.Write(ping)
	m := readUntil(t, host, "udp")
	got, _ := base64.StdEncoding.DecodeString(m.Data)
	if string(got) != string(ping) || m.IP != "127.0.0.1" || m.Conn == "" {
		t.Fatalf("host got %+v (%q)", m, got)
	}

	// A reply far bigger than the pre-trust allowance is dropped…
	huge := make([]byte, 1800)
	writeMsg(t, host, Msg{Type: "udp", Conn: m.Conn, Data: base64.StdEncoding.EncodeToString(huge)})
	// …but a normal-sized pong goes through.
	pong := []byte("pong-with-motd")
	writeMsg(t, host, Msg{Type: "udp", Conn: m.Conn, Data: base64.StdEncoding.EncodeToString(pong)})

	player.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, err := player.Read(buf)
	if err != nil {
		t.Fatalf("player got no reply: %v", err)
	}
	if string(buf[:n]) != string(pong) {
		t.Fatalf("player got %d bytes, want the pong (oversized reply should have been dropped)", n)
	}

	// Once the client has proven itself, larger replies flow.
	for i := 0; i < udpTrustPackets; i++ {
		player.Write([]byte("hello"))
		readUntil(t, host, "udp")
	}
	writeMsg(t, host, Msg{Type: "udp", Conn: m.Conn, Data: base64.StdEncoding.EncodeToString(huge)})
	player.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err = player.Read(buf)
	if err != nil || n != len(huge) {
		t.Fatalf("trusted session reply: n=%d err=%v", n, err)
	}
}

func TestUDPNotOpenedUnlessRequested(t *testing.T) {
	cfg, _ := featureConfig(t, newFakeDNS(), 27160, 27161)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	host, _ := register(t, srv, "vp_hostAAAAAAAAAA", false)
	defer host.Close()
	r.mu.Lock()
	c := r.clients[clientIDFor("vp_hostAAAAAAAAAA")]
	r.mu.Unlock()
	if c == nil || c.udp != nil {
		t.Fatal("UDP listener opened for a host that didn't ask for it")
	}
}

func fetchServers(t *testing.T, srv *httptest.Server) []serverEntry {
	t.Helper()
	res, err := http.Get(srv.URL + "/api/servers")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Servers []serverEntry `json:"servers"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Servers
}

func TestListingLifecycle(t *testing.T) {
	cfg, dir := featureConfig(t, newFakeDNS(), 27170, 27175)
	srv, _ := newFeatureServer(cfg)
	defer srv.Close()

	host, port := register(t, srv, "vp_hostAAAAAAAAAA", true)
	if got := fetchServers(t, srv); len(got) != 0 {
		t.Fatalf("server listed without opting in: %+v", got)
	}

	writeMsg(t, host, Msg{Type: "listing", Listing: &Listing{
		Title: "§aSurvival\x07 SMP", Description: "Friendly   survival", Version: "26.2",
		Mode: "survival", Players: 50, MaxPlayers: 20, Bedrock: true,
	}})
	time.Sleep(150 * time.Millisecond)
	got := fetchServers(t, srv)
	if len(got) != 1 {
		t.Fatalf("expected 1 listed server, got %+v", got)
	}
	e := got[0]
	if e.Title != "Survival SMP" || e.Description != "Friendly survival" || e.Players != 20 ||
		e.Address != fmt.Sprintf("play.example.test:%d", port) || !e.Bedrock || e.BedrockPort != port {
		t.Fatalf("unexpected entry %+v", e)
	}

	// A claimed name replaces host:port.
	writeMsg(t, host, Msg{Type: "claim_name", Name: "mysmp"})
	readUntil(t, host, "name")
	if got := fetchServers(t, srv); got[0].Address != "mysmp.example.test" || got[0].BedrockHost != "mysmp.example.test" {
		t.Fatalf("address should use the custom name: %+v", got[0])
	}

	// Operator takedown by name.
	os.WriteFile(filepath.Join(dir, "hidden.txt"), []byte("# takedowns\nmysmp\n"), 0o600)
	cfg.Hidden.mu.Lock()
	cfg.Hidden.loaded = time.Time{}
	cfg.Hidden.mu.Unlock()
	if got := fetchServers(t, srv); len(got) != 0 {
		t.Fatalf("hidden server still listed: %+v", got)
	}
	os.WriteFile(filepath.Join(dir, "hidden.txt"), nil, 0o600)
	cfg.Hidden.mu.Lock()
	cfg.Hidden.loaded = time.Time{}
	cfg.Hidden.mu.Unlock()

	// Bad words are refused; unlisting works; disconnecting removes it.
	writeMsg(t, host, Msg{Type: "listing", Listing: &Listing{Title: "Fuck SMP", MaxPlayers: 10}})
	readUntil(t, host, "listing_error")
	writeMsg(t, host, Msg{Type: "listing"})
	time.Sleep(150 * time.Millisecond)
	if got := fetchServers(t, srv); len(got) != 0 {
		t.Fatalf("unlisted server still shown: %+v", got)
	}
	writeMsg(t, host, Msg{Type: "listing", Listing: &Listing{Title: "Back again", MaxPlayers: 10}})
	time.Sleep(150 * time.Millisecond)
	host.Close()
	time.Sleep(300 * time.Millisecond)
	if got := fetchServers(t, srv); len(got) != 0 {
		t.Fatalf("offline server still listed: %+v", got)
	}
}

func TestSanitizeListingRequiresTitle(t *testing.T) {
	if _, err := sanitizeListing(Listing{Title: " §a "}); err == nil {
		t.Fatal("empty title accepted")
	}
	l, err := sanitizeListing(Listing{Title: strings.Repeat("x", 100), Mode: "weird", MaxPlayers: 99999})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(l.Title)) != 40 || l.Mode != "" || l.MaxPlayers != 1000 {
		t.Fatalf("not clamped: %+v", l)
	}
}

// clientIDFor mirrors how handleWS derives a client's ID from its token.
func clientIDFor(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hexEncode(sum[:])
}

func TestValidateShortName(t *testing.T) {
	for _, ok := range []string{"a", "z9", "7", "vp"} {
		if err := validateShortName(ok); err != nil {
			t.Errorf("%q should be a valid short name: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "abc", "A", "a-", "-a", "a b", "n", "ns", "ë"} {
		if err := validateShortName(bad); err == nil {
			t.Errorf("%q should be rejected as a short name", bad)
		}
	}
	// Ordinary claims still need three characters: only the operator hands out short ones.
	if err := validateName("vp"); err == nil {
		t.Error("a host must not be able to claim a 2-character name")
	}
}

func TestSupportersAreBadgedAndPinnedFirst(t *testing.T) {
	cfg, dir := featureConfig(t, newFakeDNS(), 27200, 27205)
	cfg.Supporters = newHiddenList(filepath.Join(dir, "supporters.txt"))
	srv, r := newFeatureServer(cfg)
	defer srv.Close()

	busy, _ := register(t, srv, "vp_busyAAAAAAAAAAAA", false)
	quiet, _ := register(t, srv, "vp_quietAAAAAAAAAAA", false)
	// Keep both connections referenced until the end: an unreferenced conn can be garbage-collected (closed) mid-test.
	defer busy.Close()
	defer quiet.Close()
	writeMsg(t, busy, Msg{Type: "listing", Listing: &Listing{Title: "Busy SMP", Players: 15, MaxPlayers: 20}})
	writeMsg(t, quiet, Msg{Type: "listing", Listing: &Listing{Title: "Quiet SMP", Players: 1, MaxPlayers: 20}})
	writeMsg(t, quiet, Msg{Type: "claim_name", Name: "quiet-smp"})
	readUntil(t, quiet, "name")

	got := waitListed(t, srv, 2)
	if len(got) != 2 || got[0].Title != "Busy SMP" || got[0].Supporter || got[1].Supporter {
		t.Fatalf("by default the busiest server is first and nobody is badged: %+v", got)
	}

	// The operator thanks the quiet server (by its address): it gets a badge and moves to the top.
	if err := r.adminDo(adminAction{Action: "supporter", Name: "quiet-smp"}); err != nil {
		t.Fatal(err)
	}
	got = fetchServers(t, srv)
	if got[0].Title != "Quiet SMP" || !got[0].Supporter || got[1].Title != "Busy SMP" || got[1].Supporter {
		t.Fatalf("supporter should be badged and pinned first: %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "supporters.txt")); !strings.Contains(string(b), "quiet-smp") {
		t.Fatalf("supporters.txt should record it, got %q", b)
	}

	// Supporters don't get the free hosts' slots: the list is otherwise ordered as before.
	if err := r.adminDo(adminAction{Action: "unsupporter", Name: "quiet-smp"}); err != nil {
		t.Fatal(err)
	}
	got = fetchServers(t, srv)
	if got[0].Title != "Busy SMP" || got[0].Supporter || got[1].Supporter {
		t.Fatalf("badge should be gone: %+v", got)
	}

	// A supporter can also be named by client ID, and the dashboard state shows it.
	var id string
	r.mu.Lock()
	for k, c := range r.clients {
		if c.listing != nil && c.listing.Title == "Busy SMP" {
			id = k
		}
	}
	r.mu.Unlock()
	if err := r.adminDo(adminAction{Action: "supporter", ID: id[:12]}); err != nil {
		t.Fatal(err)
	}
	if got = fetchServers(t, srv); !got[0].Supporter || got[0].Title != "Busy SMP" {
		t.Fatalf("supporter by id: %+v", got)
	}
	st := r.adminState()
	if list, _ := st["supporters"].([]string); len(list) != 1 || list[0] != id[:12] {
		t.Fatalf("state should list the supporter, got %+v", st["supporters"])
	}
	if err := r.adminDo(adminAction{Action: "supporter"}); err == nil {
		t.Fatal("a supporter entry needs an id or a name")
	}
}

func TestSupportersKeepTheirAddressWhenUnused(t *testing.T) {
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27210, 27211)
	s := cfg.Names
	sup := newHiddenList(filepath.Join(dir, "supporters.txt"))
	if _, err := s.claim("aaaaaaaaaaaa", "plainone", 27210); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim("bbbbbbbbbbbb", "thankyou", 27211); err != nil {
		t.Fatal(err)
	}
	if err := sup.set("thankyou", true); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	for _, e := range s.byName {
		e.LastSeen = time.Now().Add(-100 * 24 * time.Hour)
	}
	s.mu.Unlock()
	s.sweepExpiredKeeping(nameExpiry, func(string) bool { return false }, sup.has)
	if _, ok := s.forClient("aaaaaaaaaaaa"); ok {
		t.Fatal("an unused free address should still expire")
	}
	if _, ok := s.forClient("bbbbbbbbbbbb"); !ok {
		t.Fatal("a supporter's address was expired")
	}
	// A nil list (supporters disabled) keeps nobody and must not panic.
	var none *supporterList
	if none.has("bbbbbbbbbbbb", "thankyou") {
		t.Fatal("a nil supporter list matched")
	}
}

func TestOperatorCanGrantAShortAddress(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27215, 27217)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()

	host, port := register(t, srv, "vp_shortAAAAAAAAAAA", false)
	defer host.Close()
	writeMsg(t, host, Msg{Type: "claim_name", Name: "longer-name"})
	readUntil(t, host, "name")
	var id string
	r.mu.Lock()
	for k := range r.clients {
		id = k
	}
	r.mu.Unlock()

	// Hosts can't claim a short name themselves…
	writeMsg(t, host, Msg{Type: "claim_name", Name: "vp"})
	if m := readUntil(t, host, "name_error"); m.Message == "" {
		t.Fatal("a 2-character claim from a host must be refused")
	}
	// …the operator can give one, replacing the old address, and the app is told.
	if err := r.adminDo(adminAction{Action: "grant_name", ID: id[:12], Name: "vp"}); err != nil {
		t.Fatal(err)
	}
	m := readUntil(t, host, "name")
	if m.Name != "vp" || m.Address != "vp.example.test" {
		t.Fatalf("host should be told its new address, got %+v", m)
	}
	if e, ok := cfg.Names.forClient(id); !ok || e.Name != "vp" || e.Port != port {
		t.Fatalf("name store should now hold vp on port %d: %+v %v", port, e, ok)
	}
	if _, ok := cfg.Names.byName["longer-name"]; ok {
		t.Fatal("the old address should have been replaced")
	}

	// Refused cases.
	for _, a := range []adminAction{
		{Action: "grant_name", ID: id[:12], Name: "abc"}, // not a short name
		{Action: "grant_name", ID: id[:12], Name: "n"},   // reserved
		{Action: "grant_name", Name: "q9"},               // no host given
		{Action: "grant_name", ID: "ffffffffffff", Name: "q9"},
	} {
		if err := r.adminDo(a); err == nil {
			t.Errorf("%+v should have been refused", a)
		}
	}
	// A name someone else holds can't be taken.
	other, _ := register(t, srv, "vp_otherAAAAAAAAAAA", false)
	defer other.Close()
	var otherID string
	r.mu.Lock()
	for k := range r.clients {
		if k != id {
			otherID = k
		}
	}
	r.mu.Unlock()
	if err := r.adminDo(adminAction{Action: "grant_name", ID: otherID[:12], Name: "vp"}); err == nil {
		t.Fatal("a short name already in use must not be handed out twice")
	}
}

// waitListed polls the public list until n servers show (the relay handles each host's frames
// on its own goroutine, so a fixed sleep is flaky under load).
func waitListed(t *testing.T, srv *httptest.Server, n int) []serverEntry {
	t.Helper()
	var got []serverEntry
	for i := 0; i < 100; i++ {
		if got = fetchServers(t, srv); len(got) == n {
			return got
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("expected %d listed servers, last saw %+v", n, got)
	return nil
}

func TestGrantedAddressSurvivesAnAppThatRemembersAnOlderOne(t *testing.T) {
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27220, 27223)
	s := cfg.Names
	const id = "aaaaaaaaaaaa"
	if _, err := s.claim(id, "phone", 27220); err != nil {
		t.Fatal(err)
	}
	// The operator gives the host a short address in place of "phone".
	if e, err := s.grantShort(id, "gg", 27220); err != nil || !e.Granted {
		t.Fatalf("grant failed or wasn't marked granted: %+v %v", e, err)
	}
	if _, ok := s.byName["phone"]; ok {
		t.Fatal("the old address should be gone")
	}

	// The app still remembers "phone" and asks for it again on every start. That must NOT undo the grant.
	if _, err := s.claim(id, "phone", 27220); err == nil || !strings.Contains(err.Error(), "set for you") {
		t.Fatalf("a host must not be able to replace a granted address, got %v", err)
	}
	if got := s.nameFor(id); got != "gg" {
		t.Fatalf("address changed to %q", got)
	}

	// Asking for the address it already holds is fine, even though an ordinary claim can't be 2 characters.
	if e, err := s.claim(id, "gg", 27220); err != nil || e.Name != "gg" {
		t.Fatalf("re-claiming its own short address should work: %+v %v", e, err)
	}
	// ...and a different host still can't take it, or make a short one.
	if _, err := s.claim("bbbbbbbbbbbb", "gg", 27221); err == nil {
		t.Fatal("another host claimed a short address")
	}

	// The operator can still change it, and the host can give it up on purpose.
	if e, err := s.grantShort(id, "hh", 27220); err != nil || e.Name != "hh" {
		t.Fatalf("operator re-grant: %+v %v", e, err)
	}
	// It survives a restart of the relay.
	reloaded, err := loadNameStore(filepath.Join(dir, "names.json"), "example.test", dns)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := reloaded.forClient(id); !ok || e.Name != "hh" || !e.Granted {
		t.Fatalf("granted flag not persisted: %+v %v", e, ok)
	}
	if err := s.release(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(id, "phone", 27220); err != nil {
		t.Fatalf("after giving up the granted address the host can claim a normal one: %v", err)
	}
	if e, _ := s.forClient(id); e.Granted {
		t.Fatal("an ordinary claim must not be marked granted")
	}
}

func TestShortAddressesFromAnOlderSaveAreKeptOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "names.json")
	// A save written before the "granted" flag existed: one short address (operator-made), one ordinary, one junk.
	os.WriteFile(path, []byte(`[
	  {"name":"gg","client_id":"aaaaaaaaaaaa","port":27230,"records":["c1","s1"]},
	  {"name":"mysmp","client_id":"bbbbbbbbbbbb","port":27231,"records":["c2","s2"]},
	  {"name":"-bad-","client_id":"cccccccccccc","port":27232,"records":[]},
	  {"name":"abc","client_id":"","port":27233,"records":[]}
	]`), 0o600)
	s, err := loadNameStore(path, "example.test", newFakeDNS())
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := s.forClient("aaaaaaaaaaaa"); !ok || e.Name != "gg" || !e.Granted {
		t.Fatalf("short address lost or not marked granted: %+v %v", e, ok)
	}
	if e, ok := s.forClient("bbbbbbbbbbbb"); !ok || e.Name != "mysmp" || e.Granted {
		t.Fatalf("ordinary address wrong: %+v %v", e, ok)
	}
	if _, ok := s.forClient("cccccccccccc"); ok {
		t.Fatal("an invalid name was kept")
	}
	if len(s.byName) != 2 {
		t.Fatalf("expected 2 addresses, got %d", len(s.byName))
	}
}

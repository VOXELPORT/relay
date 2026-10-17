package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Tests for how many hosts and custom addresses the relay can hold: the port pool, how long a name keeps its port,
// and the per-address limits. (Items P1–P4 and L1–L3 of the capacity change.)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// claimOverWS asks for a custom address on a live tunnel and returns the relay's answer ("name" or "name_error").
func claimOverWS(t *testing.T, ws *websocket.Conn, name string) Msg {
	t.Helper()
	writeMsg(t, ws, Msg{Type: "claim_name", Name: name})
	for i := 0; i < 20; i++ {
		m := readMsg(t, ws)
		if m.Type == "name" || m.Type == "name_error" {
			return m
		}
	}
	t.Fatal("no answer to claim_name")
	return Msg{}
}

func setLastSeen(s *nameStore, clientID string, when time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byClient[clientID].LastSeen = when
}

// disconnect closes a host's tunnel and waits until the relay has finished with it, including the moment the
// grace period starts counting (touchClosed), so a test can then set LastSeen without the relay overwriting it.
func disconnect(t *testing.T, ws *websocket.Conn, r *relay, token string) {
	t.Helper()
	id := clientIDFor(token)
	before := time.Now()
	ws.Close()
	waitUntil(t, "the tunnel to be gone", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.clients[id] == nil
	})
	if r.names != nil {
		if _, ok := r.names.forClient(id); ok {
			waitUntil(t, "the last-seen time to be updated", func() bool {
				e, _ := r.names.forClient(id)
				return !e.LastSeen.Before(before)
			})
		}
	}
}

const (
	tokA = "vp_capacity_aaaaaaaaaaaa"
	tokB = "vp_capacity_bbbbbbbbbbbb"
	tokC = "vp_capacity_cccccccccccc"
)

// ─── P1: the port pool ──────────────────────────────────────────────────────

func TestP1_PortListParsing(t *testing.T) {
	got, err := parsePortList("25565, 26499 ,,")
	if err != nil || len(got) != 2 || got[0] != 25565 || got[1] != 26499 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := parsePortList(""); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v, %v", got, err)
	}
	for _, bad := range []string{"abc", "0", "70000", "25565;26499", "-5"} {
		if _, err := parsePortList(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestP1_CapacityConfigIsValidated(t *testing.T) {
	ok := func(min, max int, reserved []int, listen string) error {
		return validateCapacity(min, max, reserved, listen, 7*24*time.Hour, 0.5, 10, 3)
	}
	if err := ok(25500, 29999, []int{25565}, "127.0.0.1:2526"); err != nil {
		t.Fatalf("the defaults are valid: %v", err)
	}
	if err := ok(25500, 29999, nil, "127.0.0.1:27000"); err == nil || !strings.Contains(err.Error(), "-listen port 27000 overlaps") {
		t.Fatalf("a control port inside the range must be refused: %v", err)
	}
	if err := ok(25500, 29999, []int{27000}, "127.0.0.1:27000"); err != nil {
		t.Fatalf("...unless it is kept out of the pool on purpose: %v", err)
	}
	if err := ok(25500, 25502, []int{25500, 25501, 25502}, ":2526"); err == nil {
		t.Fatal("a range with nothing left in it must be refused")
	}
	for name, err := range map[string]error{
		"grace":    validateCapacity(25500, 29999, nil, ":2526", 0, 0.5, 10, 3),
		"fraction": validateCapacity(25500, 29999, nil, ":2526", time.Hour, 1.5, 10, 3),
		"zero":     validateCapacity(25500, 29999, nil, ":2526", time.Hour, 0, 10, 3),
		"claims4":  validateCapacity(25500, 29999, nil, ":2526", time.Hour, 0.5, 0, 3),
		"claims6":  validateCapacity(25500, 29999, nil, ":2526", time.Hour, 0.5, 10, 0),
	} {
		if err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	// The direct listener may sit inside the range only when it is reserved.
	if err := validateDirectListener(":26499", "c", "k", 26000, 29999, nil); err == nil {
		t.Fatal("a direct listener inside the range must be refused")
	}
	if err := validateDirectListener(":26499", "c", "k", 26000, 29999, []int{26499}); err != nil {
		t.Fatalf("...unless it is reserved: %v", err)
	}
}

func TestP1_ReservedPortsAreNeverHandedOut(t *testing.T) {
	cfg := testConfig()
	cfg.MinPort, cfg.MaxPort = 27500, 27502
	cfg.ReservedPorts = []int{27500, 27501}
	srv, _ := newTestServerWithConfig(cfg)
	defer srv.Close()
	ws, port := register(t, srv, tokA, false)
	defer ws.Close()
	if port != 27502 {
		t.Fatalf("expected the only unreserved port 27502, got %d", port)
	}
	ws2 := dialWS(t, srv)
	defer ws2.Close()
	writeMsg(t, ws2, Msg{Type: "register", Token: tokB})
	if m := readMsg(t, ws2); m.Type != "error" || !strings.Contains(m.Message, "no free public port") {
		t.Fatalf("with every free port taken the second host is refused, got %+v", m)
	}
}

// ─── P2: a name's port is held for a grace period, not for the name's whole life ─────────

func TestP2_NamePortGraceExpires(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27300, 27301)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()

	wsA, portA := register(t, srv, tokA, false)
	if portA != 27300 {
		t.Fatalf("host A should get the first port, got %d", portA)
	}
	if m := claimOverWS(t, wsA, "alpha"); m.Type != "name" {
		t.Fatalf("claim failed: %+v", m)
	}
	disconnect(t, wsA, r, tokA)

	// Just left: its port is still held for it, so a stranger gets the other one.
	wsB, portB := register(t, srv, tokB, false)
	if portB != 27301 {
		t.Fatalf("within the grace period the stranger must not get A's port 27300, got %d", portB)
	}
	disconnect(t, wsB, r, tokB)

	// Away for 8 days: the grace period (7 days) is over, so the port is free for anyone.
	setLastSeen(cfg.Names, clientIDFor(tokA), time.Now().Add(-8*24*time.Hour))
	wsC, portC := register(t, srv, tokC, false) // (B would be offered its own last port first, so another host asks)
	defer wsC.Close()
	if portC != 27300 {
		t.Fatalf("after the grace period a stranger may take 27300, got %d", portC)
	}
}

func TestP2_AnAddressJustBelowTheGraceStaysProtected(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27310, 27311)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	wsA, _ := register(t, srv, tokA, false)
	claimOverWS(t, wsA, "alpha")
	disconnect(t, wsA, r, tokA)
	setLastSeen(cfg.Names, clientIDFor(tokA), time.Now().Add(-6*24*time.Hour-23*time.Hour))
	wsB, portB := register(t, srv, tokB, false)
	defer wsB.Close()
	if portB != 27311 {
		t.Fatalf("6 days 23 hours is inside the grace period; got port %d", portB)
	}
}

func TestP2_SupporterKeepsThePortForever(t *testing.T) {
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27320, 27321)
	path := filepath.Join(dir, "supporters.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Supporters = newHiddenList(path)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	wsA, portA := register(t, srv, tokA, false)
	claimOverWS(t, wsA, "alpha")
	disconnect(t, wsA, r, tokA)
	setLastSeen(cfg.Names, clientIDFor(tokA), time.Now().Add(-100*24*time.Hour))
	wsB, portB := register(t, srv, tokB, false)
	defer wsB.Close()
	if portB == portA {
		t.Fatalf("a supporter's port is kept even after 100 days, but B was given %d", portB)
	}
}

func TestP2_AGrantedAddressKeepsThePortForever(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27330, 27331)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	wsA, portA := register(t, srv, tokA, false)
	if _, err := cfg.Names.grantShort(clientIDFor(tokA), "ab", portA); err != nil {
		t.Fatal(err)
	}
	disconnect(t, wsA, r, tokA)
	setLastSeen(cfg.Names, clientIDFor(tokA), time.Now().Add(-300*24*time.Hour))
	wsB, portB := register(t, srv, tokB, false)
	defer wsB.Close()
	if portB == portA {
		t.Fatalf("an operator-granted address keeps its port, but B was given %d", portB)
	}
}

func TestP2_OwnerComingBackToATakenPortGetsANewOneAndTheAddressFollows(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27340, 27341)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()

	wsA, portA := register(t, srv, tokA, false)
	if m := claimOverWS(t, wsA, "alpha"); m.Type != "name" {
		t.Fatalf("claim failed: %+v", m)
	}
	disconnect(t, wsA, r, tokA)
	setLastSeen(cfg.Names, clientIDFor(tokA), time.Now().Add(-8*24*time.Hour))

	wsB, portB := register(t, srv, tokB, false) // takes A's old port
	defer wsB.Close()
	if portB != portA {
		t.Fatalf("B should have taken A's expired port %d, got %d", portA, portB)
	}

	wsA2, portA2 := register(t, srv, tokA, false)
	defer wsA2.Close()
	if portA2 == portA {
		t.Fatalf("A's old port is in use: it must be given another, got %d", portA2)
	}
	name := readUntil(t, wsA2, "name")
	if name.Name != "alpha" || name.Address != "alpha.example.test" {
		t.Fatalf("the address players use must not change, got %+v", name)
	}
	waitUntil(t, "the DNS record to follow the host", func() bool {
		dns.mu.Lock()
		defer dns.mu.Unlock()
		for _, p := range dns.repoints {
			if p == portA2 {
				return true
			}
		}
		return false
	})
	waitUntil(t, "the stored address to move", func() bool {
		e, _ := cfg.Names.forClient(clientIDFor(tokA))
		return e.Port == portA2
	})
}

// ─── P3: names may reserve only part of the pool ─────────────────────────────

func capFixture(t *testing.T, supporters string) (*nameStore, *relay, func(i int) (*websocket.Conn, int), func()) {
	t.Helper()
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27400, 27409) // ten ports: names may hold five
	if supporters != "" {
		path := filepath.Join(dir, "supporters.txt")
		if err := os.WriteFile(path, []byte(supporters+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Supporters = newHiddenList(path)
	}
	srv, r := newFeatureServer(cfg)
	connect := func(i int) (*websocket.Conn, int) {
		return register(t, srv, "vp_capcap_"+strings.Repeat(string(rune('a'+i)), 12), false)
	}
	return cfg.Names, r, connect, srv.Close
}

func TestP3_ClaimsStopAtTheReservationCap(t *testing.T) {
	_, _, connect, closeAll := capFixture(t, "")
	defer closeAll()
	for i := 0; i < 5; i++ {
		ws, _ := connect(i)
		defer ws.Close()
		if m := claimOverWS(t, ws, "name-"+strings.Repeat(string(rune('a'+i)), 3)); m.Type != "name" {
			t.Fatalf("claim %d should succeed below the cap: %+v", i+1, m)
		}
	}
	ws, _ := connect(5)
	defer ws.Close()
	m := claimOverWS(t, ws, "name-zzz")
	if m.Type != "name_error" || m.Message != "Custom addresses are full right now — try again later." {
		t.Fatalf("the sixth claim must hit the cap, got %+v", m)
	}
}

func TestP3_SupportersAndOperatorGrantsGoPastTheCap(t *testing.T) {
	supporterToken := "vp_capcap_" + strings.Repeat("f", 12)
	store, _, connect, closeAll := capFixture(t, clientIDFor(supporterToken)[:12])
	defer closeAll()
	for i := 0; i < 5; i++ {
		ws, _ := connect(i)
		defer ws.Close()
		claimOverWS(t, ws, "name-"+strings.Repeat(string(rune('a'+i)), 3))
	}
	wsS, _ := connect(5) // index 5 is the supporter's token
	defer wsS.Close()
	if m := claimOverWS(t, wsS, "name-sss"); m.Type != "name" {
		t.Fatalf("a supporter is not held to the cap: %+v", m)
	}
	wsOther, portOther := connect(6)
	defer wsOther.Close()
	if m := claimOverWS(t, wsOther, "name-ooo"); m.Type != "name_error" {
		t.Fatalf("an ordinary host still is: %+v", m)
	}
	if _, err := store.grantShort(clientIDFor("vp_capcap_"+strings.Repeat("g", 12)), "zz", portOther+1); err != nil {
		t.Fatalf("an operator grant is not held to the cap: %v", err)
	}
}

func TestP3_NamesWhoseHostIsLongGoneDoNotCountTowardsTheCap(t *testing.T) {
	store, r, connect, closeAll := capFixture(t, "")
	defer closeAll()
	var tokens []string
	for i := 0; i < 5; i++ {
		ws, _ := connect(i)
		claimOverWS(t, ws, "name-"+strings.Repeat(string(rune('a'+i)), 3))
		tok := "vp_capcap_" + strings.Repeat(string(rune('a'+i)), 12)
		tokens = append(tokens, tok)
		disconnect(t, ws, r, tok)
	}
	if got := store.reservedCount(time.Now()); got != 5 {
		t.Fatalf("five names hold their ports, got %d", got)
	}
	for _, tok := range tokens[:2] {
		setLastSeen(store, clientIDFor(tok), time.Now().Add(-8*24*time.Hour))
	}
	if got := store.reservedCount(time.Now()); got != 3 {
		t.Fatalf("two names are past their grace and no longer count, got %d", got)
	}
	ws, _ := connect(7)
	defer ws.Close()
	if m := claimOverWS(t, ws, "name-new"); m.Type != "name" {
		t.Fatalf("there is room again: %+v", m)
	}
}

func TestP3_ChangingYourOwnAddressWorksAtTheCap(t *testing.T) {
	_, _, connect, closeAll := capFixture(t, "")
	defer closeAll()
	var first *websocket.Conn
	for i := 0; i < 5; i++ {
		ws, _ := connect(i)
		defer ws.Close()
		if i == 0 {
			first = ws
		}
		claimOverWS(t, ws, "name-"+strings.Repeat(string(rune('a'+i)), 3))
	}
	if m := claimOverWS(t, first, "name-renamed"); m.Type != "name" {
		t.Fatalf("a host that already has an address may swap it when the pool is full: %+v", m)
	}
}

// ─── P4: a daily limit on new claims per source address ──────────────────────

func claimsStore(t *testing.T) *nameStore {
	t.Helper()
	store, err := loadNameStore(filepath.Join(t.TempDir(), "names.json"), "example.test", newFakeDNS())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func uniqueClient(i int) string {
	return clientIDFor("vp_claimer_" + strings.Repeat(string(rune('a'+i%26)), 8) + string(rune('a'+i/26)))
}

func claimN(store *nameStore, ip string, n int, base int) error {
	for i := 0; i < n; i++ {
		if _, err := store.claimFrom(uniqueClient(base+i), ip, "claim-"+strings.Repeat(string(rune('a'+(base+i)%26)), 3)+string(rune('a'+(base+i)/26)), 26000+base+i); err != nil {
			return err
		}
	}
	return nil
}

func TestP4_TenClaimsADayPerIPv4Address(t *testing.T) {
	store := claimsStore(t)
	if err := claimN(store, "198.51.100.7", 10, 0); err != nil {
		t.Fatalf("ten claims from one IPv4 address in a day are fine: %v", err)
	}
	err := claimN(store, "198.51.100.7", 1, 10)
	if err == nil || !strings.Contains(err.Error(), "Too many custom addresses were claimed from your network today") {
		t.Fatalf("the 11th must be refused with a plain message, got %v", err)
	}
	if err := claimN(store, "198.51.100.8", 1, 11); err != nil {
		t.Fatalf("another address is unaffected: %v", err)
	}
	if err := claimN(store, "::ffff:198.51.100.7", 1, 12); err == nil {
		t.Fatal("the IPv4-mapped form of the same address counts as the same address")
	}
}

func TestP4_ThreeClaimsADayPerIPv6Prefix(t *testing.T) {
	store := claimsStore(t)
	// Different addresses in one /64 are one household.
	for i, ip := range []string{"2401:4900:1c2a:5f00::1", "2401:4900:1c2a:5f00:aaaa::2", "2401:4900:1c2a:5f00:ffff:ffff:ffff:ffff"} {
		if err := claimN(store, ip, 1, i); err != nil {
			t.Fatalf("claim %d from the /64 should work: %v", i+1, err)
		}
	}
	if err := claimN(store, "2401:4900:1c2a:5f00::99", 1, 3); err == nil {
		t.Fatal("the 4th claim from one /64 must be refused")
	}
	// A different /64 is a different network.
	if err := claimN(store, "2401:4900:1c2a:5f01::1", 1, 4); err != nil {
		t.Fatalf("another /64 is unaffected: %v", err)
	}
}

func TestP4_OnlySuccessfulNewClaimsCount(t *testing.T) {
	store := claimsStore(t)
	ip := "203.0.113.9"
	// A refused claim (bad name, taken name) uses none of the allowance.
	for i := 0; i < 20; i++ {
		if _, err := store.claimFrom(uniqueClient(i), ip, "x", 26000); err == nil {
			t.Fatal("a one-letter name is invalid")
		}
	}
	// Claiming the address you already have again (apps do on every start) is free too.
	if err := claimN(store, ip, 1, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // (a host's own attempts are limited separately: five per ten minutes)
		if err := claimN(store, ip, 1, 0); err != nil {
			t.Fatalf("re-claiming your own address must not use the allowance: %v", err)
		}
	}
	if err := claimN(store, ip, 9, 1); err != nil {
		t.Fatalf("nine more are still allowed (ten in all): %v", err)
	}
	if err := claimN(store, ip, 1, 10); err == nil {
		t.Fatal("the 11th new address is over the limit")
	}
}

func TestP4_TheAllowanceComesBackAfterADay(t *testing.T) {
	store := claimsStore(t)
	ip := "203.0.113.20"
	if err := claimN(store, ip, 10, 0); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	key := limitKey(ip)
	for i := range store.ipClaims[key] {
		store.ipClaims[key][i] = time.Now().Add(-25 * time.Hour)
	}
	store.mu.Unlock()
	if err := claimN(store, ip, 1, 10); err != nil {
		t.Fatalf("a day later the allowance is back: %v", err)
	}
	store.sweepIPClaims()
	store.mu.Lock()
	n := len(store.ipClaims)
	store.mu.Unlock()
	if n != 1 {
		t.Fatalf("the sweep keeps only addresses with recent claims, got %d", n)
	}
}

func TestP4_TheOperatorAndTheHostsOwnAttemptLimitAreUnchanged(t *testing.T) {
	store := claimsStore(t)
	// The per-token attempt limit (five tries per ten minutes) still applies, with or without a source address.
	id := uniqueClient(40)
	for i := 0; i < nameClaimLimit; i++ {
		if _, err := store.claimFrom(id, "203.0.113.30", "try-"+strings.Repeat(string(rune('a'+i)), 3), 26000+i); err != nil {
			t.Fatalf("attempt %d should be allowed: %v", i+1, err)
		}
	}
	if _, err := store.claimFrom(id, "203.0.113.30", "fine-name", 26009); err == nil || !strings.Contains(err.Error(), "Too many tries") {
		t.Fatalf("the per-token attempt limit still applies, got %v", err)
	}
	// Operator grants carry no source address and are never counted.
	for i := 0; i < 15; i++ {
		if _, err := store.grantShort(uniqueClient(50+i), "a"+string(rune('a'+i)), 26100+i); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
}

// ─── N: custom addresses as a supporter perk (-names-supporters-only) ────────

func supportersOnlyFixture(t *testing.T, supporters string) (*nameStore, relayConfig, func() (*websocket.Conn, int), func()) {
	t.Helper()
	dns := newFakeDNS()
	cfg, dir := featureConfig(t, dns, 27500, 27519)
	cfg.NamesSupportersOnly = true
	if supporters != "" {
		path := filepath.Join(dir, "supporters.txt")
		if err := os.WriteFile(path, []byte(supporters+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Supporters = newHiddenList(path)
	}
	srv, _ := newFeatureServer(cfg)
	connect := func() (*websocket.Conn, int) { return register(t, srv, tokA, false) }
	return cfg.Names, cfg, connect, srv.Close
}

func TestN1_AnOrdinaryHostGetsNoNewAddressWhenItIsASupporterPerk(t *testing.T) {
	_, _, connect, closeAll := supportersOnlyFixture(t, "")
	defer closeAll()
	ws, _ := connect()
	defer ws.Close()
	m := claimOverWS(t, ws, "freeloader")
	if m.Type != "name_error" || !strings.Contains(m.Message, "New custom addresses are for VoxelPort supporters now") || !strings.Contains(m.Message, "keeps working") {
		t.Fatalf("an ordinary host must be told, in plain words, got %+v", m)
	}
	if !strings.Contains(m.Message, "Naye custom address") {
		t.Fatalf("and in Hinglish too: %q", m.Message)
	}
}

func TestN1_ASupporterAndAnOperatorGrantStillGetAnAddress(t *testing.T) {
	store, _, connect, closeAll := supportersOnlyFixture(t, clientIDFor(tokA)[:12])
	defer closeAll()
	ws, port := connect()
	defer ws.Close()
	if m := claimOverWS(t, ws, "supporter-smp"); m.Type != "name" {
		t.Fatalf("a supporter (listed by client id) gets one: %+v", m)
	}
	if _, err := store.grantShort(clientIDFor(tokB), "zz", port+1); err != nil {
		t.Fatalf("the operator can still grant one: %v", err)
	}
}

func TestN1_AnAddressAHostAlreadyHasKeepsWorking(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27530, 27539)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	ws, port := register(t, srv, tokA, false)
	if m := claimOverWS(t, ws, "oldfriend"); m.Type != "name" {
		t.Fatalf("claim before the change: %+v", m)
	}
	disconnect(t, ws, r, tokA)

	// The perk becomes supporters-only.
	cfg.Names.configure(nameLimits{SupportersOnly: true, PoolSize: 10})

	ws2, _ := register(t, srv, tokA, false) // reconnecting: the name is announced as before
	defer ws2.Close()
	if m := readUntil(t, ws2, "name"); m.Name != "oldfriend" {
		t.Fatalf("an existing address is still announced: %+v", m)
	}
	if m := claimOverWS(t, ws2, "oldfriend"); m.Type != "name" {
		t.Fatalf("apps re-send their address on every start; that must keep working: %+v", m)
	}
	if m := claimOverWS(t, ws2, "somethingnew"); m.Type != "name_error" {
		t.Fatalf("but swapping it for a new one is a new address: %+v", m)
	}
	// Releasing is always allowed, and frees the DNS records.
	writeMsg(t, ws2, Msg{Type: "release_name"})
	if m := readUntil(t, ws2, "name"); m.Name != "" {
		t.Fatalf("release: %+v", m)
	}
	_ = port
	if _, ok := cfg.Names.forClient(clientIDFor(tokA)); ok {
		t.Fatal("the address is gone after a release")
	}
}

func TestN1_TheDefaultIsUnchanged(t *testing.T) {
	dns := newFakeDNS()
	cfg, _ := featureConfig(t, dns, 27540, 27549)
	srv, _ := newFeatureServer(cfg)
	defer srv.Close()
	ws, _ := register(t, srv, tokA, false)
	defer ws.Close()
	if m := claimOverWS(t, ws, "everyone"); m.Type != "name" {
		t.Fatalf("without the flag anyone can claim: %+v", m)
	}
}

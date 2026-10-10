package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAdminToken = "test-admin-token-0123456789abcdef"

func adminCall(t *testing.T, h http.Handler, method, path, body, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAdminRequiresToken(t *testing.T) {
	cfg, _ := featureConfig(t, newFakeDNS(), 27180, 27181)
	_, r := newFeatureServer(cfg)
	h := r.adminHandler(testAdminToken)
	if code, _ := adminCall(t, h, "GET", "/state", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", code)
	}
	if code, _ := adminCall(t, h, "GET", "/state", "", "wrong-token-wrong-token-wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d", code)
	}
	if code, _ := adminCall(t, h, "GET", "/state", "", testAdminToken); code != http.StatusOK {
		t.Fatalf("right token: got %d", code)
	}
}

func TestAdminStateKickAndBan(t *testing.T) {
	cfg, dir := featureConfig(t, newFakeDNS(), 27185, 27188)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	r.banned = loadBanList(filepath.Join(dir, "banned.txt"))
	h := r.adminHandler(testAdminToken)

	const token = "vp_adminAAAAAAAAAA"
	host, port := register(t, srv, token, false)
	defer host.Close()
	id := clientIDFor(token)[:12]

	_, state := adminCall(t, h, "GET", "/state", "", testAdminToken)
	tunnels := state["tunnels"].([]any)
	if len(tunnels) != 1 || tunnels[0].(map[string]any)["id"] != id || int(tunnels[0].(map[string]any)["port"].(float64)) != port {
		t.Fatalf("unexpected tunnels %+v", tunnels)
	}

	// Kick closes the tunnel.
	if code, out := adminCall(t, h, "POST", "/action", `{"action":"kick","id":"`+id+`"}`, testAdminToken); code != 200 {
		t.Fatalf("kick: %d %v", code, out)
	}
	host.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := host.ReadMessage(); err == nil {
		t.Fatal("tunnel still open after kick")
	}

	// A banned token can't register again; unban lets it back in.
	if code, out := adminCall(t, h, "POST", "/action", `{"action":"ban","id":"`+id+`"}`, testAdminToken); code != 200 {
		t.Fatalf("ban: %d %v", code, out)
	}
	ws := dialWS(t, srv)
	writeMsg(t, ws, Msg{Type: "register", Token: token})
	if m := readMsg(t, ws); m.Type != "error" || !strings.Contains(m.Message, "blocked") {
		t.Fatalf("banned token got %+v", m)
	}
	ws.Close()
	if !loadBanList(filepath.Join(dir, "banned.txt")).banned(clientIDFor(token)) {
		t.Fatal("ban not persisted")
	}
	adminCall(t, h, "POST", "/action", `{"action":"unban","id":"`+id+`"}`, testAdminToken)
	again, _ := register(t, srv, token, false)
	again.Close()
}

func TestAdminHideTogglesListing(t *testing.T) {
	cfg, _ := featureConfig(t, newFakeDNS(), 27190, 27192)
	srv, r := newFeatureServer(cfg)
	defer srv.Close()
	h := r.adminHandler(testAdminToken)

	const token = "vp_hideAAAAAAAAAAA"
	host, _ := register(t, srv, token, false)
	defer host.Close()
	writeMsg(t, host, Msg{Type: "listing", Listing: &Listing{Title: "Hide me", MaxPlayers: 10}})
	time.Sleep(150 * time.Millisecond)
	if len(fetchServers(t, srv)) != 1 {
		t.Fatal("listing not shown")
	}
	id := clientIDFor(token)[:12]
	adminCall(t, h, "POST", "/action", `{"action":"hide","id":"`+id+`"}`, testAdminToken)
	if got := fetchServers(t, srv); len(got) != 0 {
		t.Fatalf("hidden listing still shown: %+v", got)
	}
	adminCall(t, h, "POST", "/action", `{"action":"unhide","id":"`+id+`"}`, testAdminToken)
	if len(fetchServers(t, srv)) != 1 {
		t.Fatal("unhide didn't restore the listing")
	}
}

func TestAdminRejectsBadIDs(t *testing.T) {
	cfg, _ := featureConfig(t, newFakeDNS(), 27195, 27196)
	_, r := newFeatureServer(cfg)
	h := r.adminHandler(testAdminToken)
	for _, body := range []string{`{"action":"kick","id":"abc"}`, `{"action":"kick","id":"zzzzzzzzzz"}`, `{"action":"nope"}`, `not json`} {
		if code, _ := adminCall(t, h, "POST", "/action", body, testAdminToken); code != http.StatusBadRequest {
			t.Fatalf("%s: got %d", body, code)
		}
	}
}

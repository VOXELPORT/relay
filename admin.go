package main

// Operator API: a second HTTP listener, meant for loopback only (-admin-addr
// 127.0.0.1:2528), that the voxelport-admin dashboard talks to. Unlike
// /api/status it lists every tunnel, so it is never mounted on the public
// mux and every request must carry $VOXELPORT_ADMIN_TOKEN as a bearer token.

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// banList is a file of client-ID prefixes (≥8 hex chars, as shown in the
// dashboard) that may not open tunnels. Written only by the admin API.
type banList struct {
	path    string
	mu      sync.Mutex
	entries []string
}

func loadBanList(path string) *banList {
	b := &banList{path: path}
	b.entries = readLines(path)
	return b
}

func (b *banList) banned(clientID string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.entries {
		if strings.HasPrefix(clientID, e) {
			return true
		}
	}
	return false
}

func (b *banList) list() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.entries...)
}

func (b *banList) set(entry string, on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = toggleEntry(b.entries, entry, on)
	return writeLines(b.path, b.entries)
}

// readLines returns the non-empty, non-comment lines of path, lowercased.
func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func writeLines(path string, lines []string) error {
	tmp := path + ".tmp"
	data := strings.Join(lines, "\n")
	if data != "" {
		data += "\n"
	}
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func toggleEntry(entries []string, entry string, on bool) []string {
	out := entries[:0:0]
	for _, e := range entries {
		if e != entry {
			out = append(out, e)
		}
	}
	if on {
		out = append(out, entry)
	}
	return out
}

// setHidden adds or removes a takedown entry and makes it apply immediately.
func (h *hiddenList) set(entry string, on bool) error {
	if h == nil || h.path == "" {
		return errors.New("takedowns need -data-dir")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = toggleEntry(readLines(h.path), entry, on)
	h.loaded = time.Now()
	return writeLines(h.path, h.entries)
}

// list returns a snapshot of every custom address.
func (s *nameStore) list() []nameEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]nameEntry, 0, len(s.byName))
	for _, e := range s.byName {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type adminTunnel struct {
	ID          string    `json:"id"`
	Port        int       `json:"port"`
	Name        string    `json:"name,omitempty"`
	SourceIP    string    `json:"source_ip"`
	Since       time.Time `json:"since"`
	Players     []string  `json:"players"`
	UDP         bool      `json:"udp"`
	UDPSessions int       `json:"udp_sessions"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
	Conns       int64     `json:"conns"`
	Peak        int       `json:"peak_players"`
	Listing     *Listing  `json:"listing,omitempty"`
	Hidden      bool      `json:"hidden"`
	Supporter   bool      `json:"supporter"`
}

type adminName struct {
	Name      string    `json:"name"`
	ID        string    `json:"id"`
	Port      int       `json:"port"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen"`
	Online    bool      `json:"online"`
	Supporter bool      `json:"supporter"`
}

func (r *relay) adminState() map[string]any {
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	usedPorts := len(r.usedPort)
	r.mu.Unlock()

	tunnels := make([]adminTunnel, 0, len(clients))
	var players int
	var liveIn, liveOut, liveConns int64
	online := map[string]bool{}
	for _, c := range clients {
		online[c.id] = true
		t := adminTunnel{
			ID: c.id[:12], Port: c.publicPort, SourceIP: c.sourceIP, Since: c.startedAt,
			BytesIn: c.bytesIn.Load(), BytesOut: c.bytesOut.Load(), Conns: c.totalConns.Load(),
			UDP: c.udp != nil, Players: []string{},
		}
		c.mu.Lock()
		for _, pc := range c.players {
			t.Players = append(t.Players, pc.remoteHost())
		}
		t.Peak = c.peakPlayers
		if c.listing != nil {
			l := *c.listing
			t.Listing = &l
		}
		c.mu.Unlock()
		sort.Strings(t.Players)
		if c.udp != nil {
			c.udp.mu.Lock()
			t.UDPSessions = len(c.udp.byID)
			c.udp.mu.Unlock()
		}
		if r.names != nil {
			t.Name = r.names.nameFor(c.id)
		}
		t.Hidden = r.hidden.hides(c.id, t.Name)
		t.Supporter = r.supporters.has(c.id, t.Name)
		players += len(t.Players) + t.UDPSessions
		liveIn += t.BytesIn
		liveOut += t.BytesOut
		liveConns += t.Conns
		tunnels = append(tunnels, t)
	}
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].Port < tunnels[j].Port })

	names := []adminName{}
	if r.names != nil {
		for _, e := range r.names.list() {
			names = append(names, adminName{Name: e.Name, ID: e.ClientID[:12], Port: e.Port, Created: e.Created, LastSeen: e.LastSeen, Online: online[e.ClientID], Supporter: r.supporters.has(e.ClientID, e.Name)})
		}
	}
	var hidden, banned, supporters []string
	if r.hidden != nil {
		hidden = append(hidden, r.hidden.current()...)
	}
	if r.supporters != nil {
		supporters = append(supporters, r.supporters.current()...)
	}
	if r.banned != nil {
		banned = r.banned.list()
	}

	var notice any
	if message, seconds, ok := r.activeNotice(); ok {
		notice = map[string]any{"message": message, "seconds_left": seconds}
	}

	return map[string]any{
		"notice":        notice, // the announcement hosts are being shown, or null
		"started":       r.startedAt,
		"public_host":   r.publicHost,
		"ports":         map[string]int{"min": r.minPort, "max": r.maxPort, "used": usedPorts},
		"names_enabled": r.names != nil,
		"totals": map[string]any{
			"tunnels":        len(tunnels),
			"players":        players,
			"live_bytes_in":  liveIn,
			"live_bytes_out": liveOut,
			// Everything since the relay process started, including closed tunnels.
			"bytes_in":       liveIn + r.closedIn.Load(),
			"bytes_out":      liveOut + r.closedOut.Load(),
			"conns":          liveConns + r.closedConns.Load(),
			"tunnels_served": r.tunnelsServed.Load(),
		},
		"tunnels": tunnels,
		"names":   names,
		"hidden":  nonNil(hidden),
		"banned":  nonNil(banned),
		// Hosts thanked for supporting VoxelPort (see supporterList).
		"supporters": nonNil(supporters),
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// findClient resolves a dashboard ID (a client-ID prefix of ≥8 hex chars).
func (r *relay) findClient(prefix string) *client {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.clients {
		if strings.HasPrefix(id, prefix) {
			return c
		}
	}
	return nil
}

func (r *relay) fullClientID(prefix string) string {
	if c := r.findClient(prefix); c != nil {
		return c.id
	}
	if r.names != nil {
		for _, e := range r.names.list() {
			if strings.HasPrefix(e.ClientID, prefix) {
				return e.ClientID
			}
		}
	}
	return ""
}

type adminAction struct {
	Action string `json:"action"` // kick | ban | unban | hide | unhide | supporter | unsupporter | grant_name | release_name | notice | clear_notice
	ID     string `json:"id"`     // client-ID prefix (kick, ban, unban, hide, unhide, supporter, unsupporter, grant_name, release_name)
	Name   string `json:"name"`   // or a custom-address name (hide, unhide, supporter, unsupporter); the short name to give (grant_name)

	Message string `json:"message"` // notice: what to tell every host ("The relay restarts in 10 minutes.")
	Minutes int    `json:"minutes"` // notice: how many minutes until it happens (0 = just information)
}

func (r *relay) adminDo(a adminAction) error {
	id := strings.ToLower(strings.TrimSpace(a.ID))
	name := strings.ToLower(strings.TrimSpace(a.Name))
	if id != "" && (len(id) < 8 || strings.Trim(id, "0123456789abcdef") != "") {
		return errors.New("id must be at least 8 hex characters")
	}
	switch a.Action {
	case "notice":
		if _, err := r.setNotice(a.Message, a.Minutes); err != nil {
			return err
		}
		return nil
	case "clear_notice":
		r.clearNotice()
		return nil
	case "kick":
		if id == "" {
			return errors.New("missing id") // an empty prefix would match any tunnel
		}
		c := r.findClient(id)
		if c == nil {
			return errors.New("that tunnel isn't online")
		}
		c.close(r)
		log.Printf("[admin] kicked %s… (port %d)", c.id[:8], c.publicPort)
	case "ban", "unban":
		if r.banned == nil {
			return errors.New("bans need -data-dir")
		}
		if id == "" {
			return errors.New("missing id")
		}
		if err := r.banned.set(id, a.Action == "ban"); err != nil {
			return err
		}
		if a.Action == "ban" {
			if c := r.findClient(id); c != nil {
				c.close(r)
			}
		}
		log.Printf("[admin] %s %s", a.Action, id)
	case "hide", "unhide":
		entry := name
		if entry == "" {
			entry = id
		}
		if entry == "" {
			return errors.New("missing id or name")
		}
		if err := r.hidden.set(entry, a.Action == "hide"); err != nil {
			return err
		}
		log.Printf("[admin] %s %s", a.Action, entry)
	case "supporter", "unsupporter":
		if r.supporters == nil {
			return errors.New("supporters need -data-dir")
		}
		entry := name
		if entry == "" {
			entry = id
		}
		if entry == "" {
			return errors.New("missing id or name")
		}
		if err := r.supporters.set(entry, a.Action == "supporter"); err != nil {
			return err
		}
		log.Printf("[admin] %s %s", a.Action, entry)
	case "grant_name":
		// Gives a supporter a 1–2 character address (replacing any address they have now).
		if r.names == nil {
			return errors.New("custom addresses are disabled")
		}
		if id == "" {
			return errors.New("missing id")
		}
		full := r.fullClientID(id)
		if full == "" {
			return errors.New("no host with that id")
		}
		// The address points at the host's public port: its live one, or the one it already reserves.
		port := 0
		if c := r.findClient(id); c != nil {
			port = c.publicPort
		} else if e, ok := r.names.forClient(full); ok {
			port = e.Port
		}
		if port == 0 {
			return errors.New("that host isn't online and has no address yet")
		}
		e, err := r.names.grantShort(full, name, port)
		if err != nil {
			return err
		}
		if c := r.findClient(id); c != nil {
			c.send(Msg{Type: "name", Name: e.Name, Address: r.names.address(e.Name)})
		}
		log.Printf("[admin] granted short address %s to %s…", e.Name, full[:8])
	case "release_name":
		if r.names == nil {
			return errors.New("custom addresses are disabled")
		}
		if id == "" {
			return errors.New("missing id")
		}
		full := r.fullClientID(id)
		if full == "" {
			return errors.New("no address for that id")
		}
		if err := r.names.release(full); err != nil {
			return err
		}
		if c := r.findClient(id); c != nil {
			c.send(Msg{Type: "name"})
		}
	default:
		return errors.New("unknown action")
	}
	return nil
}

// adminHandler serves GET /state and POST /action behind a bearer token.
func (r *relay) adminHandler(token string) http.Handler {
	want := []byte("Bearer " + token)
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/state":
			writeJSON(w, http.StatusOK, r.adminState())
		case req.Method == http.MethodGet && req.URL.Path == "/system":
			writeJSON(w, http.StatusOK, machineInfo())
		case req.Method == http.MethodPost && req.URL.Path == "/action":
			var a adminAction
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&a); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
				return
			}
			if err := r.adminDo(a); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	})
}

// startAdmin runs the operator API on addr. It refuses anything but a
// loopback address, and refuses to start without a token.
func (r *relay) startAdmin(addr, token, dataDir string) {
	if addr == "" {
		return
	}
	if token == "" || len(token) < 24 {
		log.Fatalf("-admin-addr needs $VOXELPORT_ADMIN_TOKEN (at least 24 characters)")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "[::1]:") && !strings.HasPrefix(addr, "localhost:") {
		log.Fatalf("-admin-addr must be a loopback address")
	}
	if dataDir != "" {
		r.banned = loadBanList(filepath.Join(dataDir, "banned.txt"))
	}
	srv := &http.Server{Addr: addr, Handler: r.adminHandler(token), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("operator API on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("operator API stopped: %v", err)
		}
	}()
}

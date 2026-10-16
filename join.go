package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Join codes let a player connect to a host peer-to-peer. A host that turns it on gets a code
// (XXXXX-XXXXX) to share. A player enters it in VoxelPort; the app connects to /join here, and this
// relay only carries the connection-setup messages (WebRTC offer, answer and network candidates)
// between the two, so they can open a direct connection to each other. It also tells the player the
// host's ordinary relay address, which the player's app falls back to if no direct route can be made.
// None of the game's traffic passes through this code.
//
// The code is a capability: anyone who has it can ask to join. It is stable for the host (derived with
// HMAC from a key kept in the data directory and the host's id), so friends can save it, and the host can
// "rotate" it. It is only valid while the host is online with join codes switched on. Wrong codes are rate-limited
// per address. Codes and IP addresses are never logged and never written to disk (only the key and the
// number of times each host rotated its code are saved).

const (
	joinCodeLen       = 10 // 50 bits
	maxPeersPerHost   = 16
	maxSignalBytes    = 8 * 1024
	maxJoinersPerIP   = 4
	joinFailLimit     = 10
	joinFailWindow    = 10 * time.Minute
	joinFirstFrame    = 10 * time.Second
	joinerIdleTimeout = 90 * time.Second
	joinerLifetime    = 10 * time.Minute
	joinSignalMax     = 200 // signalling frames per joiner per minute
)

// Crockford's base32: no I, L, O or U, so a code read aloud or typed from a screenshot is hard to get wrong.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// normalizeCode accepts what a person types: any case, with or without the dash or spaces, and the
// usual look-alikes (O for 0, I and L for 1).
func normalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if strings.ContainsRune(crockford, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatCode is how a code is shown: XXXXX-XXXXX.
func formatCode(code string) string {
	if len(code) != joinCodeLen {
		return code
	}
	return code[:5] + "-" + code[5:]
}

type joinHub struct {
	key        []byte
	epochsPath string

	// Overridable for tests.
	idle     time.Duration
	lifetime time.Duration
	maxPerIP int // open join connections per address (0 = the default)

	mu      sync.Mutex
	epochs  map[string]int     // host id -> how many times it rotated its code (saved)
	byCode  map[string]*client // code -> live host that has join codes on
	codeOf  map[string]string  // host id -> its code while on
	fails   map[string][]time.Time
	joiners map[string]int // source address -> open join connections
}

// newJoinHub keeps its key and rotation counts in dir (empty dir: in memory only, so codes change on restart).
func newJoinHub(dir string) (*joinHub, error) {
	h := &joinHub{
		idle: joinerIdleTimeout, lifetime: joinerLifetime,
		epochs: map[string]int{}, byCode: map[string]*client{}, codeOf: map[string]string{},
		fails: map[string][]time.Time{}, joiners: map[string]int{},
	}
	if dir == "" {
		h.key = make([]byte, 32)
		_, err := rand.Read(h.key)
		return h, err
	}
	keyPath := filepath.Join(dir, "join.key")
	h.epochsPath = filepath.Join(dir, "join-epochs.json")
	if raw, err := os.ReadFile(keyPath); err == nil {
		if k, err := hex.DecodeString(strings.TrimSpace(string(raw))); err == nil && len(k) == 32 {
			h.key = k
		}
	}
	if h.key == nil {
		h.key = make([]byte, 32)
		if _, err := rand.Read(h.key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(h.key)+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	if raw, err := os.ReadFile(h.epochsPath); err == nil {
		_ = json.Unmarshal(raw, &h.epochs)
	}
	return h, nil
}

func (h *joinHub) codeForLocked(clientID string) string {
	mac := hmac.New(sha256.New, h.key)
	fmt.Fprintf(mac, "%s|%d", clientID, h.epochs[clientID])
	sum := mac.Sum(nil)
	var bits uint64
	for _, b := range sum[:7] {
		bits = bits<<8 | uint64(b)
	}
	out := make([]byte, joinCodeLen)
	for i := joinCodeLen - 1; i >= 0; i-- {
		out[i] = crockford[bits&31]
		bits >>= 5
	}
	return string(out)
}

func (h *joinHub) saveEpochsLocked() {
	if h.epochsPath == "" {
		return
	}
	data, _ := json.Marshal(h.epochs)
	tmp := h.epochsPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, h.epochsPath)
	}
}

// enable turns join codes on for c and returns its code.
func (h *joinHub) enable(c *client) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	code := h.codeForLocked(c.id)
	if old, ok := h.codeOf[c.id]; ok && old != code {
		delete(h.byCode, old)
	}
	h.byCode[code], h.codeOf[c.id] = c, code
	return code
}

// disable turns join codes off for c. It reports whether they were on.
func (h *joinHub) disable(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	code, ok := h.codeOf[c.id]
	if !ok {
		return false
	}
	if h.byCode[code] == c {
		delete(h.byCode, code)
	}
	delete(h.codeOf, c.id)
	return true
}

// rotate gives c a new code (the old one stops working at once). It returns "" if join codes are off for c.
func (h *joinHub) rotate(c *client) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, ok := h.codeOf[c.id]
	if !ok {
		return ""
	}
	delete(h.byCode, old)
	h.epochs[c.id]++
	h.saveEpochsLocked()
	code := h.codeForLocked(c.id)
	h.byCode[code], h.codeOf[c.id] = c, code
	return code
}

func (h *joinHub) lookup(code string) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byCode[code]
}

// tooManyWrong reports whether ip has used up its wrong guesses.
func (h *joinHub) tooManyWrong(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	kept := h.fails[ip][:0]
	for _, t := range h.fails[ip] {
		if now.Sub(t) < joinFailWindow {
			kept = append(kept, t)
		}
	}
	h.fails[ip] = kept
	if len(kept) == 0 {
		delete(h.fails, ip)
	}
	return len(kept) >= joinFailLimit
}

func (h *joinHub) recordWrong(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fails[ip] = append(h.fails[ip], time.Now())
}

func (h *joinHub) openJoiner(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	limit := h.maxPerIP
	if limit <= 0 {
		limit = maxJoinersPerIP
	}
	if h.joiners[ip] >= limit {
		return false
	}
	h.joiners[ip]++
	return true
}

func (h *joinHub) closeJoiner(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.joiners[ip] <= 1 {
		delete(h.joiners, ip)
	} else {
		h.joiners[ip]--
	}
}

// ─── A player waiting to connect ────────────────────────────────────────────────

type joiner struct {
	id        string
	ws        *websocket.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func (j *joiner) send(m Msg) {
	j.writeMu.Lock()
	data, _ := json.Marshal(m)
	j.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	err := j.ws.WriteMessage(websocket.TextMessage, data)
	j.writeMu.Unlock()
	if err != nil {
		j.close()
	}
}

func (j *joiner) close() { j.closeOnce.Do(func() { j.ws.Close() }) }

func sendJoinErr(ws *websocket.Conn, message string) {
	data, _ := json.Marshal(Msg{Type: "join_error", Message: message})
	ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	ws.WriteMessage(websocket.TextMessage, data)
}

const (
	msgBadCode = "That code isn't active. Check it, and ask the host to make sure their server is public and join codes are on."
	msgTooMany = "Too many wrong codes. Try again in a few minutes."
)

// handleJoin serves /join: a player's app presents a code, then exchanges connection-setup messages with
// the host through here until the two have connected (or it gives up).
func (r *relay) handleJoin(w http.ResponseWriter, req *http.Request) {
	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxSignalBytes + 1024)
	defer ws.Close()
	if r.join == nil {
		sendJoinErr(ws, "Join codes aren't available on this relay.")
		return
	}
	ip := r.clientIP(req)
	if !r.join.openJoiner(ip) {
		sendJoinErr(ws, "Too many connections from this address.")
		return
	}
	defer r.join.closeJoiner(ip)

	ws.SetReadDeadline(time.Now().Add(joinFirstFrame))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		return
	}
	var first Msg
	if json.Unmarshal(raw, &first) != nil || first.Type != "join" {
		sendJoinErr(ws, "expected a join frame")
		return
	}
	if r.join.tooManyWrong(ip) {
		sendJoinErr(ws, msgTooMany)
		return
	}
	host := r.join.lookup(normalizeCode(first.Code))
	if host == nil {
		r.join.recordWrong(ip)
		sendJoinErr(ws, msgBadCode)
		return
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	j := &joiner{id: hex.EncodeToString(idBytes), ws: ws}
	host.mu.Lock()
	if host.peers == nil || len(host.peers) >= maxPeersPerHost || host.isClosed() {
		host.mu.Unlock()
		sendJoinErr(ws, "This server has too many people connecting right now. Try again in a moment.")
		return
	}
	host.peers[j.id] = j
	host.mu.Unlock()
	defer func() {
		host.mu.Lock()
		delete(host.peers, j.id)
		host.mu.Unlock()
		if !host.isClosed() {
			host.send(Msg{Type: "peer_gone", Peer: j.id})
		}
	}()

	// The host's ordinary relay address, for when no direct route can be made.
	j.send(Msg{Type: "joined", Peer: j.id, Address: r.publicHost, Port: host.publicPort})
	host.send(Msg{Type: "peer", Peer: j.id, Client: cleanClientName(first.Client)})
	log.Printf("[join] token=%s player connecting", host.id[:8])

	deadline := time.Now().Add(r.join.lifetime)
	var sigTimes []time.Time
	for {
		wait := r.join.idle
		if left := time.Until(deadline); left < wait {
			wait = left
		}
		if wait <= 0 {
			return
		}
		ws.SetReadDeadline(time.Now().Add(wait))
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m Msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "signal":
			if len(m.Signal) == 0 || len(m.Signal) > maxSignalBytes {
				continue
			}
			now := time.Now()
			kept := sigTimes[:0]
			for _, t := range sigTimes {
				if now.Sub(t) < time.Minute {
					kept = append(kept, t)
				}
			}
			sigTimes = append(kept, now)
			if len(sigTimes) > joinSignalMax {
				return
			}
			host.send(Msg{Type: "peer_signal", Peer: j.id, Signal: m.Signal})
		case "ping":
			j.send(Msg{Type: "pong"})
		case "leave":
			return
		}
	}
}

// ─── The host side ──────────────────────────────────────────────────────────────

// handleHostP2P handles the join-code frames a host sends on its tunnel.
func (r *relay) handleHostP2P(c *client, m Msg) {
	if r.join == nil {
		c.send(Msg{Type: "join_error", Message: "Join codes aren't available on this relay."})
		return
	}
	switch m.Type {
	case "p2p":
		if m.P2P != nil && *m.P2P {
			code := r.join.enable(c)
			c.send(Msg{Type: "join_code", Code: formatCode(code)})
			log.Printf("[join] token=%s join codes on", c.id[:8])
		} else if r.join.disable(c) {
			r.dropPeers(c, "The host turned join codes off.")
			c.send(Msg{Type: "join_code"})
			log.Printf("[join] token=%s join codes off", c.id[:8])
		}
	case "p2p_rotate":
		code := r.join.rotate(c)
		if code == "" {
			c.send(Msg{Type: "join_error", Message: "Turn join codes on first."})
			return
		}
		r.dropPeers(c, "The host made a new code.")
		c.send(Msg{Type: "join_code", Code: formatCode(code)})
		log.Printf("[join] token=%s code rotated", c.id[:8])
	case "peer_signal":
		if len(m.Signal) == 0 || len(m.Signal) > maxSignalBytes {
			return
		}
		c.mu.Lock()
		j := c.peers[m.Peer]
		c.mu.Unlock()
		if j != nil {
			j.send(Msg{Type: "signal", Signal: m.Signal})
		}
	case "peer_close":
		c.mu.Lock()
		j := c.peers[m.Peer]
		c.mu.Unlock()
		if j != nil {
			sendJoinErr(j.ws, "The host closed this connection.")
			j.close()
		}
	}
}

// dropPeers closes every player still waiting to connect to c, telling them why.
func (r *relay) dropPeers(c *client, why string) {
	c.mu.Lock()
	peers := make([]*joiner, 0, len(c.peers))
	for _, j := range c.peers {
		peers = append(peers, j)
	}
	c.mu.Unlock()
	for _, j := range peers {
		sendJoinErr(j.ws, why)
		j.close()
	}
}

// hostGone is called when a host's tunnel closes: its code stops working and waiting players are told.
func (r *relay) hostGone(c *client) {
	if r.join == nil {
		return
	}
	r.join.disable(c)
	r.dropPeers(c, "The host went offline.")
}

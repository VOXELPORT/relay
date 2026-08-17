package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Msg is the single wire envelope exchanged with a host client (mod or app).
//
// Client -> relay:  register{token, blocked_ips[]}, data{conn, data}, close{conn}, ping
// Relay  -> client: port{port} | error{message}, connect{conn, ip}, data{conn, data}, close{conn}, pong
type Msg struct {
	Type       string   `json:"type"`
	Token      string   `json:"token,omitempty"`
	BlockedIPs []string `json:"blocked_ips,omitempty"`
	Port       int      `json:"port,omitempty"`
	Conn       string   `json:"conn,omitempty"`
	IP         string   `json:"ip,omitempty"`
	Data       string   `json:"data,omitempty"`
	Message    string   `json:"message,omitempty"`
}

// tokenPattern accepts the auto-generated device tokens produced by the mod/app.
var tokenPattern = regexp.MustCompile(`^vp_[A-Za-z0-9_-]{10,}$`)

const (
	// maxWSMessageBytes bounds a single control-channel WebSocket frame. The
	// mod caps its own outgoing base64 payload at ~2MB; this leaves headroom
	// for that plus JSON overhead while still bounding a malicious/compromised
	// host client from forcing unbounded memory allocation (gorilla/websocket
	// has no read limit by default).
	maxWSMessageBytes = 4 * 1024 * 1024

	// registerReadTimeout bounds how long we wait for the first (register)
	// frame after a WS upgrade, so a connection that never registers can't
	// hold a goroutine open indefinitely.
	registerReadTimeout = 10 * time.Second

	// idleReadTimeout bounds how long a registered control connection may go
	// without sending anything before we consider it dead. Both the mod
	// (25s) and the app (15s) ping well inside this window; it's re-armed on
	// every successful read.
	idleReadTimeout = 90 * time.Second

	// maxPlayersPerTunnel backstops a single tunnel against a connection
	// flood — well above the mod's own default max_connections (200).
	maxPlayersPerTunnel = 256

	// regLimitMax/regLimitWindow bound how many registration attempts a
	// single source IP may make, to blunt scripted abuse of the
	// intentionally accountless /ws endpoint (fake-tunnel port exhaustion).
	regLimitMax    = 10
	regLimitWindow = time.Minute
)

// client is one connected host (a single tunnel) with its own public port.
type client struct {
	id         string // sha256(token) hex — stable identity for a token
	ws         *websocket.Conn
	writeMu    sync.Mutex // serialise WS writes to this client
	publicPort int
	ln         net.Listener

	mu        sync.Mutex
	players   map[string]net.Conn
	blocked   map[string]bool
	closeOnce sync.Once

	// Usage metering — the foundation for per-token limits / billing.
	startedAt   time.Time    // when this tunnel registered (read-only)
	bytesIn     atomic.Int64 // player -> host (uploaded through the relay)
	bytesOut    atomic.Int64 // host -> player (downloaded through the relay)
	totalConns  atomic.Int64 // lifetime player connections accepted
	peakPlayers int          // max concurrent players (guarded by mu)
}

func (c *client) send(m Msg) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, _ := json.Marshal(m)
	_ = c.ws.WriteMessage(websocket.TextMessage, data)
}

// relay is the multi-tenant hub. Every host token maps to at most one live tunnel.
type relay struct {
	mu       sync.Mutex
	clients  map[string]*client // clientID -> live client
	portByID map[string]int     // clientID -> last assigned port (sticky reconnects)
	usedPort map[int]bool       // reserved/in-use public ports

	minPort int
	maxPort int

	// tokenSecret gates registration. When non-empty, a host's token must be
	// "vp_<secret>…" or it is rejected — this keeps a public/home relay from
	// being used as an open proxy by strangers. Empty = accept any well-formed
	// token (original behaviour).
	tokenSecret string

	regLimiter *regLimiter
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
}

func newRelay(minPort, maxPort int, tokenSecret string) *relay {
	return &relay{
		clients:     make(map[string]*client),
		portByID:    make(map[string]int),
		usedPort:    make(map[int]bool),
		minPort:     minPort,
		maxPort:     maxPort,
		tokenSecret: tokenSecret,
		regLimiter:  newRegLimiter(),
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// reservePort picks a free public port, preferring the client's previous sticky
// port, and binds a TCP listener on it. The port is reserved atomically.
func (r *relay) reservePort(clientID string) (net.Listener, int, error) {
	tryBind := func(port int) (net.Listener, bool) {
		r.mu.Lock()
		if r.usedPort[port] {
			r.mu.Unlock()
			return nil, false
		}
		r.usedPort[port] = true
		r.mu.Unlock()

		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			r.mu.Lock()
			delete(r.usedPort, port)
			r.mu.Unlock()
			return nil, false
		}
		return ln, true
	}

	// Prefer the sticky port so a reconnecting host keeps its address.
	r.mu.Lock()
	sticky := r.portByID[clientID]
	r.mu.Unlock()
	if sticky != 0 {
		if ln, ok := tryBind(sticky); ok {
			return ln, sticky, nil
		}
	}

	for port := r.minPort; port <= r.maxPort; port++ {
		if ln, ok := tryBind(port); ok {
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no free public port in range %d-%d", r.minPort, r.maxPort)
}

func (r *relay) releasePort(port int) {
	r.mu.Lock()
	delete(r.usedPort, port)
	r.mu.Unlock()
}

// regLimiter tracks registration attempts per source IP so a script can't
// exhaust the (small, fixed) port pool by opening many fake tunnels.
type regLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newRegLimiter() *regLimiter {
	l := &regLimiter{attempts: make(map[string][]time.Time)}
	go func() {
		for range time.Tick(regLimitWindow) {
			l.sweep()
		}
	}()
	return l
}

// allow reports whether ip may attempt another registration now, recording
// the attempt if so.
func (l *regLimiter) allow(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-regLimitWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.attempts[ip][:0]
	for _, t := range l.attempts[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= regLimitMax {
		l.attempts[ip] = kept
		return false
	}
	l.attempts[ip] = append(kept, now)
	return true
}

// sweep drops IPs with no recent attempts so the map can't grow unbounded
// under IP-rotating abuse.
func (l *regLimiter) sweep() {
	cutoff := time.Now().Add(-regLimitWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, times := range l.attempts {
		stale := true
		for _, t := range times {
			if t.After(cutoff) {
				stale = false
				break
			}
		}
		if stale {
			delete(l.attempts, ip)
		}
	}
}

// clientIP extracts the real client address for rate limiting, preferring
// the X-Forwarded-For header Caddy sets when reverse-proxying /ws in
// production, and falling back to the direct connection otherwise.
func clientIP(req *http.Request) string {
	if fwd := req.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// handleWS accepts a host client, authenticates its register frame, assigns a
// public port, and pumps the tunnel until the socket closes.
func (r *relay) handleWS(w http.ResponseWriter, req *http.Request) {
	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("[ws] upgrade: %v", err)
		return
	}
	ws.SetReadLimit(maxWSMessageBytes)

	// The first frame must be a valid register. Read it before any rejection
	// path below closes the socket — closing while the client's frame is
	// still unread in the OS receive buffer triggers a hard RST instead of a
	// clean close, which can race with (and clobber) the error frame we just
	// wrote.
	ws.SetReadDeadline(time.Now().Add(registerReadTimeout))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		ws.Close()
		return
	}
	var reg Msg
	if err := json.Unmarshal(raw, &reg); err != nil || reg.Type != "register" {
		sendErr(ws, "expected register frame")
		ws.Close()
		return
	}
	if !tokenPattern.MatchString(reg.Token) {
		sendErr(ws, "invalid or missing token")
		ws.Close()
		return
	}
	ip := clientIP(req)
	if !r.regLimiter.allow(ip) {
		sendErr(ws, "too many registration attempts, try again later")
		ws.Close()
		return
	}
	// Shared-secret gate: on a public/home relay this stops strangers who find
	// the URL from opening tunnels through the operator's connection.
	if r.tokenSecret != "" && !strings.HasPrefix(reg.Token, "vp_"+r.tokenSecret) {
		sendErr(ws, "unauthorized token")
		ws.Close()
		return
	}

	sum := sha256.Sum256([]byte(reg.Token))
	clientID := hex.EncodeToString(sum[:])

	// One live tunnel per token — kick any previous connection for this token.
	// close() releases the evicted client's own port synchronously, before we
	// reserve a port for the new connection below, so the two can never race
	// (previously, releasing happened later in the *old* goroutine's own
	// deferred cleanup, which could lose the sticky port to this new
	// registration's reservePort call).
	r.mu.Lock()
	if old := r.clients[clientID]; old != nil {
		delete(r.clients, clientID)
		r.mu.Unlock()
		old.close(r)
		r.mu.Lock()
	}
	r.mu.Unlock()

	ln, port, err := r.reservePort(clientID)
	if err != nil {
		sendErr(ws, err.Error())
		ws.Close()
		return
	}

	c := &client{
		id:         clientID,
		ws:         ws,
		publicPort: port,
		ln:         ln,
		players:    make(map[string]net.Conn),
		blocked:    make(map[string]bool),
		startedAt:  time.Now(),
	}
	for _, ip := range reg.BlockedIPs {
		c.blocked[ip] = true
	}

	r.mu.Lock()
	r.clients[clientID] = c
	r.portByID[clientID] = port
	r.mu.Unlock()

	c.send(Msg{Type: "port", Port: port})
	log.Printf("[relay] tunnel up: token %s… -> public port %d", clientID[:8], port)

	go r.acceptPlayers(c)

	defer func() {
		r.mu.Lock()
		if r.clients[clientID] == c {
			delete(r.clients, clientID)
		}
		r.mu.Unlock()
		c.close(r)
		c.mu.Lock()
		peak := c.peakPlayers
		c.mu.Unlock()
		// Durable per-session usage record (retained in journald/syslog — parse for billing).
		log.Printf("[usage] token=%s port=%d uptime=%s conns=%d peak_players=%d in=%s out=%s",
			clientID[:8], port, time.Since(c.startedAt).Round(time.Second),
			c.totalConns.Load(), peak, formatBytes(c.bytesIn.Load()), formatBytes(c.bytesOut.Load()))
	}()

	for {
		ws.SetReadDeadline(time.Now().Add(idleReadTimeout))
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m Msg
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		r.handleClientMsg(c, m)
	}
}

func (r *relay) handleClientMsg(c *client, m Msg) {
	switch m.Type {
	case "data":
		c.mu.Lock()
		pc := c.players[m.Conn]
		c.mu.Unlock()
		if pc == nil {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			return
		}
		c.bytesOut.Add(int64(len(decoded)))
		pc.Write(decoded)

	case "close":
		c.mu.Lock()
		pc := c.players[m.Conn]
		delete(c.players, m.Conn)
		c.mu.Unlock()
		if pc != nil {
			pc.Close()
		}

	case "ping":
		c.send(Msg{Type: "pong"})
	}
}

// acceptPlayers accepts vanilla players on the tunnel's public port and bridges
// each connection to the host over the WebSocket.
func (r *relay) acceptPlayers(c *client) {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go r.handlePlayer(c, conn)
	}
}

func (r *relay) handlePlayer(c *client, conn net.Conn) {
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	c.mu.Lock()
	blocked := c.blocked[host]
	tooMany := len(c.players) >= maxPlayersPerTunnel
	c.mu.Unlock()
	if blocked || tooMany {
		conn.Close()
		return
	}

	id := newID()
	c.mu.Lock()
	c.players[id] = conn
	if len(c.players) > c.peakPlayers {
		c.peakPlayers = len(c.players)
	}
	c.mu.Unlock()
	c.totalConns.Add(1)

	c.send(Msg{Type: "connect", Conn: id, IP: host})

	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			c.bytesIn.Add(int64(n))
			c.send(Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString(buf[:n])})
		}
		if err != nil {
			break
		}
	}

	c.send(Msg{Type: "close", Conn: id})
	c.mu.Lock()
	delete(c.players, id)
	c.mu.Unlock()
	conn.Close()
}

// close tears down a client's listener and all player connections, and
// releases its public port back to the pool. Idempotent — safe to call from
// both the eviction path (a newer registration for the same token) and the
// connection's own disconnect cleanup, whichever happens first.
func (c *client) close(r *relay) {
	c.closeOnce.Do(func() {
		if c.ln != nil {
			c.ln.Close()
		}
		c.mu.Lock()
		players := c.players
		c.players = make(map[string]net.Conn)
		c.mu.Unlock()
		for _, pc := range players {
			pc.Close()
		}
		c.ws.Close()
		r.releasePort(c.publicPort)
	})
}

func sendErr(ws *websocket.Conn, message string) {
	data, _ := json.Marshal(Msg{Type: "error", Message: message})
	_ = ws.WriteMessage(websocket.TextMessage, data)
}

// apiStatus exposes only aggregate, non-identifying relay metrics. It deliberately
// does NOT return a per-tunnel list: publishing each tunnel's public port turned the
// panel into a public directory of every live server, letting anyone connect to
// other people's games. A host sees its own address in-game via the mod/app.
func (r *relay) apiStatus(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	var totalPlayers int
	var totalIn, totalOut, totalConns int64
	for _, c := range r.clients {
		c.mu.Lock()
		players := len(c.players)
		c.mu.Unlock()
		totalPlayers += players
		totalIn += c.bytesIn.Load()
		totalOut += c.bytesOut.Load()
		totalConns += c.totalConns.Load()
	}
	count := len(r.clients)
	r.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]any{
		"online":  true,
		"tunnels": count,
		"players": totalPlayers,
		"totals": map[string]any{
			"bytes_in":    totalIn,
			"bytes_out":   totalOut,
			"total_conns": totalConns,
		},
	})
}

// formatBytes renders a byte count as a human-readable string for logs.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

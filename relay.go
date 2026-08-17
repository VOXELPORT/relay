package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
//
// None of the fixes in this file change this envelope — every field, type,
// and frame type is unchanged, so existing mod/app clients keep working
// without any update. See README.md's "Compatibility" note.
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

	// wsWriteTimeout bounds a single write to a host's control WebSocket, so a
	// host whose TCP connection has gone stale (but not yet errored) can't
	// hang a relay goroutine forever on a blocked write.
	wsWriteTimeout = 10 * time.Second

	// maxPlayersPerTunnel backstops a single tunnel against a connection
	// flood — well above the mod's own default max_connections (200).
	maxPlayersPerTunnel = 256

	// regLimitMax/regLimitWindow bound how many registration attempts a
	// single source IP may make, to blunt scripted abuse of the
	// intentionally accountless /ws endpoint (fake-tunnel port exhaustion).
	regLimitMax    = 10
	regLimitWindow = time.Minute

	// playerOutboxSize bounds how many not-yet-written host->player messages
	// may queue up per player before that player is considered unable to
	// keep up and is disconnected (see playerConn).
	playerOutboxSize = 128

	// playerWriteTimeout bounds a single write to a player's TCP socket, so a
	// player that stops reading can't block that write forever even once the
	// OS send buffer itself fills up.
	playerWriteTimeout = 10 * time.Second

	// initialPlayerInactivityTimeout: a newly-accepted player TCP connection
	// that sends no bytes at all within this window is dropped — this stops
	// someone from opening many raw connections that never speak Minecraft
	// just to occupy player slots.
	initialPlayerInactivityTimeout = 30 * time.Second

	// playerIdleTimeout: once a player has sent at least one byte, the read
	// deadline relaxes to this much wider window, re-armed on every read.
	// Real Minecraft traffic (client keep-alive responses) flows at least
	// every ~15-20s during normal play, so this only reaps connections that
	// have gone genuinely silent at the TCP level (e.g. a client that
	// vanished without closing cleanly), not merely idle players.
	playerIdleTimeout = 10 * time.Minute

	// stickyPortTTL: how long a disconnected token's previous public port is
	// remembered for reconnect stickiness before the memory (not any actual
	// port reservation, which is already freed at disconnect) is dropped.
	// A currently-connected token's entry is never swept regardless of age.
	stickyPortTTL       = time.Hour
	stickyPortSweepTick = 10 * time.Minute
)

// playerConn wraps a player's TCP connection with a bounded outbound queue
// and a dedicated writer goroutine, so a slow/stuck player can never block
// the host's WebSocket read loop (which is what dispatches host->player
// writes for every player of that tunnel). See enqueue.
type playerConn struct {
	conn   net.Conn
	outbox chan []byte
	// done is closed exactly once (via closeOnce) to signal the writer loop
	// to stop. It is never closed together with outbox — see enqueue for why
	// outbox itself is never closed (avoids a send-on-closed-channel panic
	// if a send races with shutdown).
	done      chan struct{}
	closeOnce sync.Once
}

func newPlayerConn(conn net.Conn) *playerConn {
	pc := &playerConn{
		conn:   conn,
		outbox: make(chan []byte, playerOutboxSize),
		done:   make(chan struct{}),
	}
	go pc.writeLoop()
	return pc
}

func (pc *playerConn) writeLoop() {
	for {
		select {
		case data := <-pc.outbox:
			pc.conn.SetWriteDeadline(time.Now().Add(playerWriteTimeout))
			if _, err := pc.conn.Write(data); err != nil {
				pc.close()
				return
			}
		case <-pc.done:
			return
		}
	}
}

// enqueue hands data to the player's writer without ever blocking the
// caller (the host's single-threaded control-message loop). Byte ordering
// is preserved since outbox is a FIFO channel drained by one writer. If the
// player's outbox is already full — meaning the player isn't consuming data
// fast enough to keep up — the player is disconnected outright rather than
// either blocking every other player on this tunnel or silently dropping
// some of its bytes while pretending the connection is healthy.
func (pc *playerConn) enqueue(data []byte) {
	select {
	case pc.outbox <- data:
	case <-pc.done:
		// Already shutting down; nothing to do.
	default:
		pc.close()
	}
}

func (pc *playerConn) close() {
	pc.closeOnce.Do(func() {
		close(pc.done)
		pc.conn.Close()
	})
}

// client is one connected host (a single tunnel) with its own public port.
type client struct {
	id         string // sha256(token) hex — stable identity for a token
	sourceIP   string // IP the registration was attributed to, for tunnel-per-IP accounting
	ws         *websocket.Conn
	writeMu    sync.Mutex // serialise WS writes to this client
	publicPort int
	ln         net.Listener
	rel        *relay // back-reference so send() can self-terminate on a broken control channel

	mu        sync.Mutex
	players   map[string]*playerConn
	ipCounts  map[string]int // source IP -> active player connections on this tunnel
	blocked   map[string]bool
	closeOnce sync.Once

	// Usage metering — the foundation for per-token limits / billing.
	startedAt   time.Time    // when this tunnel registered (read-only)
	bytesIn     atomic.Int64 // player -> host (uploaded through the relay)
	bytesOut    atomic.Int64 // host -> player (downloaded through the relay)
	totalConns  atomic.Int64 // lifetime player connections accepted
	peakPlayers int          // max concurrent players (guarded by mu)
}

// send writes a control-channel frame to the host. Writes are serialised and
// deadline-bounded; if a write fails (or times out), the control channel is
// considered unusable and the whole tunnel is torn down — there is no way to
// recover a broken control channel, so leaving it half-alive would just leak
// resources and confuse the host.
func (c *client) send(m Msg) {
	c.writeMu.Lock()
	data, _ := json.Marshal(m)
	c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	err := c.ws.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		c.close(c.rel)
	}
}

// relay is the multi-tenant hub. Every host token maps to at most one live tunnel.
type relay struct {
	mu        sync.Mutex
	clients   map[string]*client        // clientID -> live client
	portByID  map[string]stickyPortInfo // clientID -> last assigned port + when it was last used
	usedPort  map[int]bool              // reserved/in-use public ports
	ipTunnels map[string]int            // source IP -> count of currently-active tunnels

	// regSerialize makes the whole "evict any previous tunnel for this
	// token -> check per-IP tunnel limit -> reserve a port -> publish the
	// new tunnel" sequence atomic with respect to OTHER concurrent
	// registrations (same token or not). Registration is comparatively rare
	// next to data forwarding, so a single coarse lock here is simpler and
	// just as correct as fine-grained per-token locking, and it's the only
	// thing that actually guarantees "exactly one live tunnel per token"
	// and "the per-IP tunnel limit can't be bypassed by racing requests".
	regSerialize sync.Mutex

	minPort int
	maxPort int

	maxTunnelsPerIP       int
	maxPlayersPerIPTunnel int

	// Overridable for tests (real 30s/10m windows would make tests slow);
	// production always gets the package-level defaults via newRelay.
	initialPlayerTimeout time.Duration
	idlePlayerTimeout    time.Duration

	// tokenSecret gates registration with a shared prefix. See "Improve
	// token-gate comparison" in README.md for exactly what this does and
	// does not provide — it is NOT a substitute for real per-user auth.
	tokenSecret string

	trustedProxies []*net.IPNet

	regLimiter *regLimiter
}

// stickyPortInfo remembers which port a token last used, and when, so a
// reconnect can be handed the same port back and so stale entries can
// eventually be forgotten (see stickyPortTTL).
type stickyPortInfo struct {
	port     int
	lastSeen time.Time
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
}

// relayConfig groups the tunable limits so newRelay's signature doesn't grow
// a new positional parameter every time a new limit is added.
type relayConfig struct {
	MinPort               int
	MaxPort               int
	TokenSecret           string
	MaxTunnelsPerIP       int
	MaxPlayersPerIPTunnel int
	TrustedProxies        []*net.IPNet

	// Zero means "use the production default" — only tests need to set these.
	InitialPlayerTimeout time.Duration
	PlayerIdleTimeout    time.Duration
}

func newRelay(cfg relayConfig) *relay {
	initialTimeout := cfg.InitialPlayerTimeout
	if initialTimeout == 0 {
		initialTimeout = initialPlayerInactivityTimeout
	}
	idleTimeout := cfg.PlayerIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = playerIdleTimeout
	}
	r := &relay{
		clients:               make(map[string]*client),
		portByID:              make(map[string]stickyPortInfo),
		usedPort:              make(map[int]bool),
		ipTunnels:             make(map[string]int),
		minPort:               cfg.MinPort,
		maxPort:               cfg.MaxPort,
		tokenSecret:           cfg.TokenSecret,
		maxTunnelsPerIP:       cfg.MaxTunnelsPerIP,
		maxPlayersPerIPTunnel: cfg.MaxPlayersPerIPTunnel,
		trustedProxies:        cfg.TrustedProxies,
		initialPlayerTimeout:  initialTimeout,
		idlePlayerTimeout:     idleTimeout,
		regLimiter:            newRegLimiter(),
	}
	go r.sweepLoop()
	return r
}

func (r *relay) sweepLoop() {
	for range time.Tick(stickyPortSweepTick) {
		r.sweepStalePorts()
	}
}

// sweepStalePorts forgets sticky-port memory for tokens that have been
// disconnected for longer than stickyPortTTL. It never touches a token that
// currently has a live tunnel, and it never affects an actual port
// reservation — that's already released at disconnect time; this only
// bounds the memory used to remember "which port should this token get back
// if it reconnects soon".
func (r *relay) sweepStalePorts() {
	cutoff := time.Now().Add(-stickyPortTTL)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, info := range r.portByID {
		if _, live := r.clients[id]; live {
			continue
		}
		if info.lastSeen.Before(cutoff) {
			delete(r.portByID, id)
		}
	}
}

// newID returns a 128-bit random connection identifier. Callers must check
// the error — a crypto/rand failure is treated as fatal to the specific
// connection being set up (rejected), never silently ignored.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// reservePort picks a free public port, preferring the client's previous sticky
// port, and binds a TCP listener on it. The port is reserved atomically.
// Callers must already hold r.regSerialize.
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
	sticky := r.portByID[clientID].port
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

// addTunnel/removeTunnel keep the per-IP active-tunnel count consistent.
// Both must be called while holding r.regSerialize (addTunnel, as part of
// registration) or are safe to call any time for removeTunnel (it only ever
// decrements its own client's own recorded IP).
func (r *relay) addTunnel(ip string) {
	r.mu.Lock()
	r.ipTunnels[ip]++
	r.mu.Unlock()
}

func (r *relay) removeTunnel(ip string) {
	r.mu.Lock()
	if r.ipTunnels[ip] > 0 {
		r.ipTunnels[ip]--
		if r.ipTunnels[ip] == 0 {
			delete(r.ipTunnels, ip)
		}
	}
	r.mu.Unlock()
}

func (r *relay) tunnelsForIP(ip string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ipTunnels[ip]
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

// isTrustedProxy reports whether a direct peer at remoteIP is allowed to
// supply an X-Forwarded-For header we should believe. Loopback is always
// trusted, since the documented production deployment always runs Caddy on
// the same host as the relay (deploy/Caddyfile proxies to 127.0.0.1:2526) —
// a directly-exposed relay reached from anywhere else can't spoof its
// apparent source IP via a forwarded header.
func isTrustedProxy(remoteIP string, trusted []*net.IPNet) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP extracts the real client address for rate limiting and per-IP
// tunnel/player limits. X-Forwarded-For is only honoured when the direct
// peer is a trusted proxy (see isTrustedProxy) — otherwise a directly
// exposed relay could trivially be spoofed by a client supplying its own
// X-Forwarded-For header.
func (r *relay) clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if fwd := req.Header.Get("X-Forwarded-For"); fwd != "" && isTrustedProxy(host, r.trustedProxies) {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		if trimmed := strings.TrimSpace(fwd); trimmed != "" {
			return trimmed
		}
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
	ip := r.clientIP(req)
	if !r.regLimiter.allow(ip) {
		sendErr(ws, "too many registration attempts, try again later")
		ws.Close()
		return
	}
	// Shared-secret gate: on a public/home relay this stops strangers who find
	// the URL from opening tunnels through the operator's connection. Uses a
	// constant-time comparison so response timing can't be used to recover
	// the secret one byte at a time.
	if r.tokenSecret != "" {
		want := "vp_" + r.tokenSecret
		ok := len(reg.Token) >= len(want) &&
			subtle.ConstantTimeCompare([]byte(reg.Token[:len(want)]), []byte(want)) == 1
		if !ok {
			sendErr(ws, "unauthorized token")
			ws.Close()
			return
		}
	}

	sum := sha256.Sum256([]byte(reg.Token))
	clientID := hex.EncodeToString(sum[:])

	// Everything from here through publishing the new client is one atomic
	// registration: evicting any previous tunnel for this token, checking the
	// per-IP tunnel limit, and reserving a port must all happen without
	// another concurrent registration interleaving, or two of the guarantees
	// this relay makes ("one live tunnel per token" and "an IP can't exceed
	// its tunnel limit") could both be violated by a race.
	r.regSerialize.Lock()

	r.mu.Lock()
	old := r.clients[clientID]
	if old != nil {
		delete(r.clients, clientID)
	}
	r.mu.Unlock()
	if old != nil {
		// Releases old's port and decrements its IP's tunnel count
		// synchronously, so a same-IP reconnect below sees an accurate,
		// already-freed count instead of racing its own eviction.
		old.close(r)
	}

	if r.tunnelsForIP(ip) >= r.maxTunnelsPerIP {
		r.regSerialize.Unlock()
		sendErr(ws, fmt.Sprintf("too many active tunnels from this address (max %d)", r.maxTunnelsPerIP))
		ws.Close()
		return
	}

	ln, port, err := r.reservePort(clientID)
	if err != nil {
		r.regSerialize.Unlock()
		sendErr(ws, err.Error())
		ws.Close()
		return
	}

	c := &client{
		id:         clientID,
		sourceIP:   ip,
		ws:         ws,
		publicPort: port,
		ln:         ln,
		rel:        r,
		players:    make(map[string]*playerConn),
		ipCounts:   make(map[string]int),
		blocked:    make(map[string]bool),
		startedAt:  time.Now(),
	}
	for _, blockedIP := range reg.BlockedIPs {
		c.blocked[blockedIP] = true
	}

	r.mu.Lock()
	r.clients[clientID] = c
	r.portByID[clientID] = stickyPortInfo{port: port, lastSeen: time.Now()}
	r.mu.Unlock()
	r.addTunnel(ip)

	r.regSerialize.Unlock()

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
		pc.enqueue(decoded)

	case "close":
		c.mu.Lock()
		pc := c.players[m.Conn]
		delete(c.players, m.Conn)
		if pc != nil {
			if host := pc.remoteHost(); c.ipCounts[host] > 0 {
				c.ipCounts[host]--
			}
		}
		c.mu.Unlock()
		if pc != nil {
			pc.close()
		}

	case "ping":
		c.send(Msg{Type: "pong"})
	}
}

// remoteHost returns the player's source IP (no port), used for per-IP
// player-slot accounting.
func (pc *playerConn) remoteHost() string {
	host, _, _ := net.SplitHostPort(pc.conn.RemoteAddr().String())
	return host
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

	id, err := newID()
	if err != nil {
		log.Printf("[relay] connection id generation failed, dropping connection: %v", err)
		conn.Close()
		return
	}

	pc := newPlayerConn(conn)

	// Check-and-reserve must be one atomic critical section — checking the
	// caps and then inserting in two separate locked sections would let many
	// concurrent connections all observe room below the limit before any of
	// them actually claims a slot, letting the real count run over the cap.
	c.mu.Lock()
	over := c.blocked[host] ||
		len(c.players) >= maxPlayersPerTunnel ||
		c.ipCounts[host] >= c.rel.maxPlayersPerIPTunnel
	if over {
		c.mu.Unlock()
		pc.close()
		return
	}
	c.players[id] = pc
	c.ipCounts[host]++
	if len(c.players) > c.peakPlayers {
		c.peakPlayers = len(c.players)
	}
	c.mu.Unlock()
	c.totalConns.Add(1)

	c.send(Msg{Type: "connect", Conn: id, IP: host})

	buf := make([]byte, 32*1024)
	// A newly-connected player must send its first byte within
	// initialPlayerInactivityTimeout (catches slot-squatting raw TCP
	// connections that never speak Minecraft). After that, the read
	// deadline relaxes to the much wider playerIdleTimeout, re-armed on
	// every read, so normal play (which involves regular client-side
	// traffic) never times out but a connection that's gone silent at the
	// TCP level eventually is reaped.
	deadline := c.rel.initialPlayerTimeout
	for {
		conn.SetReadDeadline(time.Now().Add(deadline))
		n, err := conn.Read(buf)
		if n > 0 {
			deadline = c.rel.idlePlayerTimeout
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
	if c.ipCounts[host] > 0 {
		c.ipCounts[host]--
	}
	c.mu.Unlock()
	pc.close()
}

// close tears down a client's listener and all player connections, releases
// its public port back to the pool, and decrements its source IP's active
// tunnel count. Idempotent — safe to call from both the eviction path (a
// newer registration for the same token) and the connection's own disconnect
// cleanup, whichever happens first.
func (c *client) close(r *relay) {
	c.closeOnce.Do(func() {
		if c.ln != nil {
			c.ln.Close()
		}
		c.mu.Lock()
		players := c.players
		c.players = make(map[string]*playerConn)
		c.mu.Unlock()
		for _, pc := range players {
			pc.close()
		}
		c.ws.Close()
		r.releasePort(c.publicPort)
		r.removeTunnel(c.sourceIP)
	})
}

func sendErr(ws *websocket.Conn, message string) {
	data, _ := json.Marshal(Msg{Type: "error", Message: message})
	ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
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

// shutdown closes every active tunnel — its listener, all player
// connections, and its control WebSocket. Used for graceful shutdown: this
// unblocks every handleWS goroutine's blocked ws.ReadMessage() call so the
// HTTP server's own Shutdown can complete promptly instead of waiting for
// handlers that would otherwise run for the lifetime of the process.
func (r *relay) shutdown() {
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.close(r)
	}
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

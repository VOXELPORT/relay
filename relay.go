package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// Client -> relay: register{token, client?, blocked_ips[], udp}, data{conn, data}, close{conn}, ping,
// udp{conn, data}, udp_close{conn}, claim_name{name}, release_name, listing{listing?},
// direct{port} (Direct mode: players connect straight to the host's own IP and port, port 0 turns it off).
//
// Relay -> client: port{port} | error{message}, connect{conn, ip}, data{conn, data}, close{conn}, pong,
// udp{conn, ip?, data}, udp_close{conn}, udp_error{message}, name{name, address},
// name_error{message}, listing_error{message}, notice{message, seconds} (an announcement from the
// operator, such as "relay restarting in 10 minutes"; an empty message clears it),
// direct{ip, port, address?} (Direct mode is on: the relay reached the host at ip:port, and address, if the
// host has a custom address, now points straight at it; port 0 means players use the relay again),
// direct_error{message} (Direct mode was refused or stopped working).
//
// Protocol v2 (frames.go): a host that registers with bin:1 and is answered with bin:1 in the port frame sends and
// receives TCP and UDP payloads as binary WebSocket messages instead of base64 inside "data" and "udp" frames. Control
// frames stay JSON.
//
// The udp, name and listing frames are additive: an older client never
// sends udp/claim_name/listing and never receives udp, name or listing frames
// unless it asked, so existing mod/app versions keep working unchanged.
type Msg struct {
	Type       string   `json:"type"`
	Token      string   `json:"token,omitempty"`
	BlockedIPs []string `json:"blocked_ips,omitempty"`
	Port       int      `json:"port,omitempty"`
	Conn       string   `json:"conn,omitempty"`
	IP         string   `json:"ip,omitempty"`
	Data       string   `json:"data,omitempty"`
	Message    string   `json:"message,omitempty"`
	UDP        bool     `json:"udp,omitempty"`
	Name       string   `json:"name,omitempty"`
	Address    string   `json:"address,omitempty"`
	Listing    *Listing `json:"listing,omitempty"`
	// Client names the app and version that opened the tunnel ("desktop/1.7.0",
	// "android/10"). It is optional, only goes into the relay's own log so a support
	// question can be answered from facts, and is never trusted for anything else.
	Client string `json:"client,omitempty"`
	// Seconds is how long until what a "notice" announces happens (0 = now, or no time given).
	Seconds int `json:"seconds,omitempty"`
	// Join codes (join.go): Code is shown to the host and typed by a player; Peer names one player waiting to
	// connect; Signal is a WebRTC setup message passed between them; P2P turns join codes on or off.
	Code   string `json:"code,omitempty"`
	Peer   string `json:"peer,omitempty"`
	Signal string `json:"signal,omitempty"`
	P2P    *bool  `json:"p2p,omitempty"`
	// Bin asks for (in register) and confirms (in port) protocol v2: payloads as binary frames. Older clients never send
	// it and ignore it in port, so they keep getting JSON.
	Bin int `json:"bin,omitempty"`
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
	// single source may make, to blunt scripted abuse of the
	// intentionally accountless /ws endpoint (fake-tunnel port exhaustion).
	// "Source" is a limitKey: an IPv4 address (shared by thousands of people
	// behind carrier-grade NAT, hence the room) or an IPv6 /64 (one household).
	regLimitMax    = 30
	regLimitMax6   = 10
	regLimitWindow = time.Minute

	// Defaults for the other per-source limits: active tunnels, and players per
	// tunnel. Same key rules as above; IPv6 is lower because one /64 is one site.
	defaultMaxTunnelsPerIP        = 25
	defaultMaxTunnelsPerIP6       = 5
	defaultMaxPlayersPerIPTunnel  = 32
	defaultMaxPlayersPerIPTunnel6 = 16

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
			if data == nil { // closeAfterFlush marker: everything before it is written
				pc.close()
				return
			}
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

// closeAfterFlush closes the player once everything already queued for it
// has been written. A host often sends its last bytes and "close" back to
// back (a Minecraft server closes right after answering a status ping); a
// plain close would race the writer and drop that reply.
func (pc *playerConn) closeAfterFlush() {
	select {
	case pc.outbox <- nil:
		// Bound how long a stuck player can hold its socket open.
		time.AfterFunc(playerWriteTimeout, pc.close)
	case <-pc.done:
	default: // outbox full: the player isn't reading anyway
		pc.close()
	}
}

func (pc *playerConn) close() {
	pc.closeOnce.Do(func() {
		close(pc.done)
		pc.conn.Close()
	})
}

// Why a tunnel ended. These go into the "[usage]" log line as ended=<code>, so a support
// question ("my server went offline") can be answered from the log instead of guessed at.
const (
	endHostQuit   = "host-quit"   // the host's app closed the tunnel on purpose (stopped, quit, updated)
	endLost       = "lost"        // the connection died without a goodbye: network drop, sleep, router reset
	endSilent     = "silent"      // the host sent nothing for idleReadTimeout: stalled or half-dead connection
	endReplaced   = "replaced"    // the same host registered again, so the old connection was replaced
	endSendFailed = "send-failed" // the relay could not write to the host's connection
	endShutdown   = "relay-stop"  // the relay itself was stopped or restarted
)

// classifyReadError turns the error that ended a tunnel's read loop into one of the codes above.
func classifyReadError(err error) string {
	var ne net.Error
	switch {
	case websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway):
		return endHostQuit
	case errors.As(err, &ne) && ne.Timeout():
		return endSilent
	default:
		return endLost
	}
}

// clientNamePattern is what a client may call itself: a short app name and version such as
// "desktop/1.7.0" or "android/10". Anything else is logged as "unknown", so a client can never
// put free text (or a line break) into the relay's log.
var clientNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}(/[A-Za-z0-9][A-Za-z0-9._+-]{0,15})?$`)

func cleanClientName(s string) string {
	if clientNamePattern.MatchString(s) {
		return s
	}
	return "unknown"
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
	ipCounts  map[string]int // limitKey(source IP) -> active player connections on this tunnel
	blocked   map[string]bool
	closeOnce sync.Once

	peers      map[string]*joiner // players waiting to connect peer-to-peer (join.go; guarded by mu)
	direct     *directState       // Direct mode (guarded by mu); nil unless the host turned it on and it was verified
	directAsks []time.Time        // when the host last asked for Direct mode (guarded by mu), to rate-limit it
	closed     chan struct{}      // closed when the tunnel is torn down

	udp *udpState // non-nil when the host asked for Bedrock (UDP) forwarding
	bin bool      // protocol v2: payloads travel as binary frames (set before the tunnel is published, never changed)

	badFrameAt atomic.Int64 // when a malformed binary frame was last logged (unix nanoseconds), to rate-limit the log
	listing    *Listing     // public server-list entry, nil unless the host opted in (guarded by mu)

	name  string // app and version the host reported (cleaned), for the log only
	route string // "direct" (the relay's own TLS listener) or "tunnel" (behind Cloudflare Tunnel or a proxy)
	endMu sync.Mutex
	ended string // why the tunnel ended; the first reason to be set wins

	// Usage metering — the foundation for per-token limits / billing.
	startedAt   time.Time    // when this tunnel registered (read-only)
	bytesIn     atomic.Int64 // player -> host (uploaded through the relay)
	bytesOut    atomic.Int64 // host -> player (downloaded through the relay)
	totalConns  atomic.Int64 // lifetime player connections accepted
	peakPlayers int          // max concurrent players (guarded by mu)
}

// endWith records why the tunnel ended. Only the first reason counts: when a tunnel is torn down
// on purpose (replaced, relay stopping) the read loop fails right after, and that failure must
// not overwrite the real reason.
func (c *client) endWith(reason string) {
	c.endMu.Lock()
	if c.ended == "" {
		c.ended = reason
	}
	c.endMu.Unlock()
}

func (c *client) endReason() string {
	c.endMu.Lock()
	defer c.endMu.Unlock()
	if c.ended == "" {
		return endLost
	}
	return c.ended
}

// send writes a control-channel frame to the host. Writes are serialised and
// deadline-bounded; if a write fails (or times out), the control channel is
// considered unusable and the whole tunnel is torn down — there is no way to
// recover a broken control channel, so leaving it half-alive would just leak
// resources and confuse the host.
func (c *client) send(m Msg) {
	data, _ := json.Marshal(m)
	c.writeFrame(websocket.TextMessage, data)
}

// relay is the multi-tenant hub. Every host token maps to at most one live tunnel.
type relay struct {
	mu        sync.Mutex
	clients   map[string]*client        // clientID -> live client
	portByID  map[string]stickyPortInfo // clientID -> last assigned port + when it was last used
	usedPort  map[int]bool              // reserved/in-use public ports
	ipTunnels map[string]int            // limitKey(source IP) -> count of currently-active tunnels

	// regSerialize makes the whole "evict any previous tunnel for this
	// token -> check per-IP tunnel limit -> reserve a port -> publish the
	// new tunnel" sequence atomic with respect to OTHER concurrent
	// registrations (same token or not). Registration is comparatively rare
	// next to data forwarding, so a single coarse lock here is simpler and
	// just as correct as fine-grained per-token locking, and it's the only
	// thing that actually guarantees "exactly one live tunnel per token"
	// and "the per-IP tunnel limit can't be bypassed by racing requests".
	regSerialize sync.Mutex

	minPort  int
	maxPort  int
	reserved map[int]bool // ports in the range that tunnels must never take (see relayConfig.ReservedPorts)

	// Per-source limits, each with an IPv4 value and a lower IPv6 /64 value (see limitKey).
	maxTunnelsPerIP        int
	maxTunnelsPerIP6       int
	maxPlayersPerIPTunnel  int
	maxPlayersPerIPTunnel6 int

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

	// names holds permanent custom addresses (nil = feature disabled);
	// publicHost is the hostname players use with a port (play.voxelport.in);
	// hidden lists server-list entries an operator has taken down.
	names      *nameStore
	join       *joinHub // join codes for peer-to-peer connections; nil = off
	publicHost string
	hidden     *hiddenList
	// supporters lists hosts the operator has thanked (badge, pinned in the list,
	// address kept); nil-safe, so it simply matches nobody when unset.
	supporters *supporterList

	// Operator API (admin.go): bans, and lifetime totals that survive
	// tunnels closing. banned is nil unless the API runs with -data-dir.
	banned        *banList
	startedAt     time.Time
	closedIn      atomic.Int64
	closedOut     atomic.Int64
	closedConns   atomic.Int64
	tunnelsServed atomic.Int64

	noticeMu sync.Mutex
	notice   *operatorNotice // the current announcement to hosts, nil when there is none

	// Direct mode (direct.go). Tests replace these; production leaves them nil / zero.
	directAllowIP func(ip string) bool
	directProbeFn func(ctx context.Context, ip string, port int) error
	directEvery   time.Duration
	directRetry   time.Duration
	directCleanup time.Duration
	directWG      sync.WaitGroup // Direct mode work running in the background (waited for in tests)
}

// operatorNotice is an announcement the operator sent to every host: "relay restarting in 10 minutes".
type operatorNotice struct {
	message string
	sentAt  time.Time
	at      time.Time // when the announced thing happens; zero for plain information
}

const (
	maxNoticeLen       = 200
	maxNoticeMinutes   = 24 * 60
	noticeAfterEvent   = 10 * time.Minute // keep showing it this long after the announced time
	noticeInfoLifetime = 6 * time.Hour    // a notice with no time expires by itself after this
)

// cleanNotice checks an announcement: plain text of a sensible length, no control characters, so
// it is safe to show in an app and to repeat in Minecraft chat.
func cleanNotice(message string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", errors.New("the message is empty")
	}
	if len([]rune(message)) > maxNoticeLen {
		return "", fmt.Errorf("the message is longer than %d characters", maxNoticeLen)
	}
	for _, ch := range message {
		if ch < 0x20 || ch == 0x7f {
			return "", errors.New("the message has a control character or line break in it")
		}
	}
	return message, nil
}

// setNotice announces message to every connected host, and to any host that connects before it
// expires. minutes is how long until the announced thing happens (0 for plain information).
// It returns how many hosts it was sent to.
func (r *relay) setNotice(message string, minutes int) (int, error) {
	message, err := cleanNotice(message)
	if err != nil {
		return 0, err
	}
	if minutes < 0 || minutes > maxNoticeMinutes {
		return 0, fmt.Errorf("minutes must be between 0 and %d", maxNoticeMinutes)
	}
	n := &operatorNotice{message: message, sentAt: time.Now()}
	if minutes > 0 {
		n.at = n.sentAt.Add(time.Duration(minutes) * time.Minute)
	}
	r.noticeMu.Lock()
	r.notice = n
	r.noticeMu.Unlock()

	hosts := r.liveClients()
	for _, c := range hosts {
		c.send(Msg{Type: "notice", Message: message, Seconds: minutes * 60})
	}
	log.Printf("[notice] sent to %d host(s), in %d min: %q", len(hosts), minutes, message)
	return len(hosts), nil
}

// clearNotice withdraws the current announcement (the apps hide it).
func (r *relay) clearNotice() int {
	r.noticeMu.Lock()
	had := r.notice != nil
	r.notice = nil
	r.noticeMu.Unlock()
	if !had {
		return 0
	}
	hosts := r.liveClients()
	for _, c := range hosts {
		c.send(Msg{Type: "notice"})
	}
	log.Printf("[notice] cleared (%d host(s) told)", len(hosts))
	return len(hosts)
}

// activeNotice is the announcement that should still be shown, and how many seconds are left
// until the thing it announces (0 once it is due, or when no time was given).
func (r *relay) activeNotice() (message string, seconds int, ok bool) {
	r.noticeMu.Lock()
	n := r.notice
	r.noticeMu.Unlock()
	if n == nil {
		return "", 0, false
	}
	now := time.Now()
	if n.at.IsZero() {
		if now.Sub(n.sentAt) > noticeInfoLifetime {
			return "", 0, false
		}
		return n.message, 0, true
	}
	if now.After(n.at.Add(noticeAfterEvent)) {
		return "", 0, false
	}
	if left := n.at.Sub(now); left > 0 {
		seconds = int(left.Round(time.Second) / time.Second)
	}
	return n.message, seconds, true
}

// liveClients is a snapshot of the connected hosts, safe to use without holding r.mu.
func (r *relay) liveClients() []*client {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c)
	}
	return out
}

// countIn is how many of ports lie within [lo, hi].
func countIn(ports []int, lo, hi int) int {
	n := 0
	seen := map[int]bool{}
	for _, p := range ports {
		if p >= lo && p <= hi && !seen[p] {
			seen[p] = true
			n++
		}
	}
	return n
}

// stickyPortInfo remembers which port a token last used, and when, so a
// reconnect can be handed the same port back and so stale entries can
// eventually be forgotten (see stickyPortTTL).
type stickyPortInfo struct {
	port     int
	lastSeen time.Time
	lastEnd  time.Time // when this token's last tunnel ended (zero if it has not ended yet)
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
	MaxTunnelsPerIP       int // per IPv4 address
	MaxPlayersPerIPTunnel int // per IPv4 address, per tunnel
	TrustedProxies        []*net.IPNet

	// The IPv6 /64 counterparts, and the registration limit per minute (zero = the default).
	MaxTunnelsPerIP6       int
	MaxPlayersPerIPTunnel6 int
	RegLimitIP             int
	RegLimitIP6            int

	Names      *nameStore
	Join       *joinHub
	PublicHost string
	Hidden     *hiddenList
	Supporters *supporterList

	// Capacity rules for custom addresses (zero = the default in names.go).
	NamePortGrace           time.Duration // how long an absent host keeps its name's port
	MaxReservedPortFraction float64       // share of the port range names may reserve
	NameClaimsPerIPDay      int           // new claims per IPv4 address per day
	NameClaimsPerIPDay6     int           // new claims per IPv6 /64 per day
	// ReservedPorts are inside the range but never handed to a tunnel: ports other programs on the machine use.
	ReservedPorts []int

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
	orDefault := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	r := &relay{
		clients:                make(map[string]*client),
		portByID:               make(map[string]stickyPortInfo),
		usedPort:               make(map[int]bool),
		ipTunnels:              make(map[string]int),
		minPort:                cfg.MinPort,
		maxPort:                cfg.MaxPort,
		reserved:               map[int]bool{},
		tokenSecret:            cfg.TokenSecret,
		maxTunnelsPerIP:        orDefault(cfg.MaxTunnelsPerIP, defaultMaxTunnelsPerIP),
		maxTunnelsPerIP6:       orDefault(cfg.MaxTunnelsPerIP6, defaultMaxTunnelsPerIP6),
		maxPlayersPerIPTunnel:  orDefault(cfg.MaxPlayersPerIPTunnel, defaultMaxPlayersPerIPTunnel),
		maxPlayersPerIPTunnel6: orDefault(cfg.MaxPlayersPerIPTunnel6, defaultMaxPlayersPerIPTunnel6),
		trustedProxies:         cfg.TrustedProxies,
		initialPlayerTimeout:   initialTimeout,
		idlePlayerTimeout:      idleTimeout,
		regLimiter:             newRegLimiter(orDefault(cfg.RegLimitIP, regLimitMax), orDefault(cfg.RegLimitIP6, regLimitMax6)),
		names:                  cfg.Names,
		join:                   cfg.Join,
		publicHost:             cfg.PublicHost,
		hidden:                 cfg.Hidden,
		supporters:             cfg.Supporters,
		startedAt:              time.Now(),
	}
	if r.publicHost == "" {
		r.publicHost = "localhost"
	}
	for _, p := range cfg.ReservedPorts {
		r.reserved[p] = true
	}
	if r.names != nil {
		r.names.configure(nameLimits{
			Grace:               cfg.NamePortGrace,
			MaxReservedFraction: cfg.MaxReservedPortFraction,
			PoolSize:            cfg.MaxPort - cfg.MinPort + 1 - countIn(cfg.ReservedPorts, cfg.MinPort, cfg.MaxPort),
			Keep:                r.supporters.has,
			ClaimsPerIPDay:      cfg.NameClaimsPerIPDay,
			ClaimsPerIPDay6:     cfg.NameClaimsPerIPDay6,
		})
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
		// Ports other programs on this machine use are never handed out.
		if r.reserved[port] {
			return nil, false
		}
		// A port behind someone else's custom address stays theirs while its host is away, for the port
		// grace period (always, for a supporter or a granted address): otherwise the address's DNS record
		// would point at a stranger. Once the grace has passed, the port is free for anyone, and the host
		// gets another when it returns (and its DNS record follows).
		if r.names != nil && r.names.portHeldFrom(port, clientID, time.Now()) {
			return nil, false
		}
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

	// Prefer the port behind this host's custom address, then the sticky
	// port, so a reconnecting host keeps its address.
	r.mu.Lock()
	sticky := r.portByID[clientID].port
	r.mu.Unlock()
	if r.names != nil {
		if e, ok := r.names.forClient(clientID); ok {
			sticky = e.Port
		}
	}
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

// tunnelLimit is how many active tunnels the source ip may have: the IPv4 limit, or the lower IPv6 /64 one.
func (r *relay) tunnelLimit(ip string) int {
	if isIPv6Key(limitKey(ip)) {
		return r.maxTunnelsPerIP6
	}
	return r.maxTunnelsPerIP
}

// playerLimit is how many players one source may have on a single tunnel.
func (r *relay) playerLimit(key string) int {
	if isIPv6Key(key) {
		return r.maxPlayersPerIPTunnel6
	}
	return r.maxPlayersPerIPTunnel
}

// addTunnel/removeTunnel keep the per-source active-tunnel count consistent (counted per limitKey).
// Both must be called while holding r.regSerialize (addTunnel, as part of
// registration) or are safe to call any time for removeTunnel (it only ever
// decrements its own client's own recorded IP).
func (r *relay) addTunnel(ip string) {
	key := limitKey(ip)
	r.mu.Lock()
	r.ipTunnels[key]++
	r.mu.Unlock()
}

func (r *relay) removeTunnel(ip string) {
	key := limitKey(ip)
	r.mu.Lock()
	if r.ipTunnels[key] > 0 {
		r.ipTunnels[key]--
		if r.ipTunnels[key] == 0 {
			delete(r.ipTunnels, key)
		}
	}
	r.mu.Unlock()
}

func (r *relay) tunnelsForIP(ip string) int {
	key := limitKey(ip)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ipTunnels[key]
}

// regLimiter tracks registration attempts per source IP so a script can't
// exhaust the (small, fixed) port pool by opening many fake tunnels.
type regLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time // limitKey -> attempt times within the window
	max4     int                    // attempts per window for an IPv4 address
	max6     int                    // ...for an IPv6 /64
}

func newRegLimiter(max4, max6 int) *regLimiter {
	l := &regLimiter{attempts: make(map[string][]time.Time), max4: max4, max6: max6}
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
	key := limitKey(ip)
	max := l.max4
	if isIPv6Key(key) {
		max = l.max6
	}
	now := time.Now()
	cutoff := now.Add(-regLimitWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, now)
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
// tunnel/player limits. X-Forwarded-For / X-Real-IP are only honoured when
// the direct peer is a trusted proxy (see isTrustedProxy) — otherwise a
// directly exposed relay could trivially be spoofed by a client supplying
// its own header. Both headers are checked because different reverse
// proxies default to different ones (e.g. nginx's proxy_set_header
// X-Real-IP vs. Caddy's automatic X-Forwarded-For) — accepting either from
// a trusted peer costs nothing since both are equally spoofable/trustable
// depending on the same isTrustedProxy check.
func (r *relay) clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if !isTrustedProxy(host, r.trustedProxies) {
		return host
	}
	if fwd := req.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		if trimmed := strings.TrimSpace(fwd); trimmed != "" {
			return trimmed
		}
	}
	if real := strings.TrimSpace(req.Header.Get("X-Real-IP")); real != "" {
		return real
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
	clientName := cleanClientName(reg.Client)
	route := "tunnel"
	if req.TLS != nil {
		route = "direct"
	}
	ip := r.clientIP(req)
	if !r.regLimiter.allow(ip) {
		sendErr(ws, "Too many connection attempts from your network in the last minute — wait a minute and try again. If you're on mobile data, try Wi-Fi.")
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
	if r.banned.banned(clientID) {
		log.Printf("[relay] refused: token %s… client=%s: blocked", clientID[:8], clientName)
		sendErr(ws, "This app has been blocked from the VoxelPort relay.")
		ws.Close()
		return
	}

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
		old.endWith(endReplaced)
		old.close(r)
	}

	if limit := r.tunnelLimit(ip); r.tunnelsForIP(ip) >= limit {
		r.regSerialize.Unlock()
		log.Printf("[relay] refused: token %s… client=%s: too many active tunnels from one address", clientID[:8], clientName)
		sendErr(ws, fmt.Sprintf("Too many VoxelPort servers are online from your network right now (limit %d). If you're on mobile data, try Wi-Fi.", limit))
		ws.Close()
		return
	}

	ln, port, err := r.reservePort(clientID)
	if err != nil {
		r.regSerialize.Unlock()
		log.Printf("[relay] refused: token %s… client=%s: %v", clientID[:8], clientName, err)
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
		name:       clientName,
		route:      route,
		closed:     make(chan struct{}),
		peers:      make(map[string]*joiner),
		bin:        reg.Bin == 1,
	}
	for _, blockedIP := range reg.BlockedIPs {
		c.blocked[blockedIP] = true
	}
	// Bound before the tunnel is published, so an eviction can never race
	// with c.udp being set.
	var udpErr error
	if reg.UDP {
		udpErr = c.startUDP()
	}

	r.mu.Lock()
	// How long this host was gone, if it came back within the hour: the number that tells a
	// quick network blip from a host that closed the app and came back later.
	cameBack := time.Duration(0)
	if prev, ok := r.portByID[clientID]; ok && !prev.lastEnd.IsZero() && time.Since(prev.lastEnd) < time.Hour {
		cameBack = time.Since(prev.lastEnd).Round(time.Second)
	}
	r.clients[clientID] = c
	r.portByID[clientID] = stickyPortInfo{port: port, lastSeen: time.Now()}
	r.mu.Unlock()
	r.addTunnel(ip)
	r.tunnelsServed.Add(1)

	r.regSerialize.Unlock()

	portReply := Msg{Type: "port", Port: port}
	if c.bin {
		portReply.Bin = 1 // both sides now speak protocol v2 (binary payload frames)
	}
	c.send(portReply)
	if message, seconds, ok := r.activeNotice(); ok {
		c.send(Msg{Type: "notice", Message: message, Seconds: seconds})
	}
	switch {
	case old != nil:
		// The host reconnected while its previous connection still looked alive: the old one had
		// silently died (a network blip on the host's side) and was replaced.
		log.Printf("[relay] tunnel up: token %s… -> public port %d client=%s route=%s replaced-a-dead-connection", clientID[:8], port, clientName, route)
	case cameBack > 0:
		log.Printf("[relay] tunnel up: token %s… -> public port %d client=%s route=%s back-after=%s", clientID[:8], port, clientName, route, cameBack)
	default:
		log.Printf("[relay] tunnel up: token %s… -> public port %d client=%s route=%s", clientID[:8], port, clientName, route)
	}

	if udpErr != nil {
		log.Printf("[udp] port %d: %v", port, udpErr)
		c.send(Msg{Type: "udp_error", Message: "Bedrock (UDP) forwarding is unavailable right now."})
	}
	if r.names != nil {
		if e, ok := r.names.forClient(clientID); ok {
			if e.Port != port {
				// Its reserved port was somehow busy; move the DNS record to
				// the port the host actually got.
				go r.names.repoint(clientID, port)
			}
			r.names.touch(clientID)
			c.send(Msg{Type: "name", Name: e.Name, Address: r.names.address(e.Name)})
		}
	}

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
		r.closedIn.Add(c.bytesIn.Load())
		r.closedOut.Add(c.bytesOut.Load())
		r.closedConns.Add(c.totalConns.Load())
		r.mu.Lock()
		if info, ok := r.portByID[clientID]; ok {
			info.lastEnd = time.Now()
			r.portByID[clientID] = info
		}
		r.mu.Unlock()
		if r.names != nil {
			r.names.touchClosed(clientID) // the port grace counts from now
		}
		// Durable per-session usage record (retained in journald/syslog — parse for billing).
		// ended= says why it ended (see the end* codes), client= and route= say what connected.
		log.Printf("[usage] token=%s port=%d uptime=%s conns=%d peak_players=%d in=%s out=%s ended=%s client=%s route=%s",
			clientID[:8], port, time.Since(c.startedAt).Round(time.Second),
			c.totalConns.Load(), peak, formatBytes(c.bytesIn.Load()), formatBytes(c.bytesOut.Load()),
			c.endReason(), c.name, c.route)
	}()

	for {
		ws.SetReadDeadline(time.Now().Add(idleReadTimeout))
		mt, data, err := ws.ReadMessage()
		if err != nil {
			c.endWith(classifyReadError(err))
			return
		}
		if mt == websocket.BinaryMessage {
			r.handleBinary(c, data)
			continue
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
		c.deliverToPlayer(m.Conn, decoded)

	case "close":
		c.mu.Lock()
		pc := c.players[m.Conn]
		delete(c.players, m.Conn)
		if pc != nil {
			c.releasePlayerCountLocked(pc.remoteHost())
		}
		c.mu.Unlock()
		if pc != nil {
			pc.closeAfterFlush()
		}

	case "ping":
		c.send(Msg{Type: "pong"})

	case "udp":
		c.handleHostUDP(m)

	case "udp_close":
		c.closeUDPSession(m.Conn, false)

	case "claim_name":
		if r.names == nil {
			c.send(Msg{Type: "name_error", Message: "Custom addresses aren't available on this relay."})
			return
		}
		name, port := m.Name, c.publicPort
		// DNS calls take a moment — never block this tunnel's data path on them.
		go func() {
			e, err := r.names.claimFrom(c.id, c.sourceIP, name, port)
			if err != nil {
				c.send(Msg{Type: "name_error", Message: err.Error()})
				return
			}
			c.send(Msg{Type: "name", Name: e.Name, Address: r.names.address(e.Name)})
			r.refreshDirect(c)
		}()

	case "release_name":
		if r.names == nil {
			return
		}
		go func() {
			if err := r.names.release(c.id); err != nil {
				c.send(Msg{Type: "name_error", Message: err.Error()})
				return
			}
			c.send(Msg{Type: "name"})
			r.refreshDirect(c)
		}()

	case "p2p", "p2p_rotate", "peer_signal", "peer_close":
		// Join codes: a host lets players connect to it peer-to-peer (join.go).
		r.handleHostP2P(c, m)

	case "direct":
		// Direct mode: the host opened a port on its router and wants players to skip the relay.
		// Port 0 turns it off. Verifying takes seconds, so it never blocks this tunnel.
		r.background(func() { r.handleDirect(c, m.Port) })

	case "listing":
		if m.Listing == nil {
			c.mu.Lock()
			c.listing = nil
			c.mu.Unlock()
			return
		}
		l, err := sanitizeListing(*m.Listing)
		if err != nil {
			c.send(Msg{Type: "listing_error", Message: err.Error()})
			return
		}
		c.mu.Lock()
		c.listing = &l
		c.mu.Unlock()
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

// deliverToPlayer hands bytes the host sent for connection id to that player, counting them. It is the one place both the
// JSON "data" frame and the binary frame end up, and it reports whether the connection exists.
func (c *client) deliverToPlayer(id string, payload []byte) bool {
	c.mu.Lock()
	pc := c.players[id]
	c.mu.Unlock()
	if pc == nil {
		return false
	}
	c.bytesOut.Add(int64(len(payload)))
	pc.enqueue(payload)
	return true
}

// admitPlayer registers a new player on the tunnel if the caps allow it: the tunnel-wide cap, and the per-source cap, where
// a source is a limitKey (an IPv4 address, or an IPv6 /64 — one household, however many addresses it uses). Check and insert
// are one critical section, so concurrent connections can't all see room and overrun the cap together.
func (c *client) admitPlayer(id string, pc *playerConn, host string) bool {
	key := limitKey(host)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked[host] || len(c.players) >= maxPlayersPerTunnel || c.ipCounts[key] >= c.rel.playerLimit(key) {
		return false
	}
	c.players[id] = pc
	c.ipCounts[key]++
	if len(c.players) > c.peakPlayers {
		c.peakPlayers = len(c.players)
	}
	return true
}

// releasePlayerCount gives back the per-source slot a player held. Callers hold c.mu.
func (c *client) releasePlayerCountLocked(host string) {
	if key := limitKey(host); c.ipCounts[key] > 0 {
		c.ipCounts[key]--
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
	if !c.admitPlayer(id, pc, host) {
		pc.close()
		return
	}
	c.totalConns.Add(1)

	c.send(Msg{Type: "connect", Conn: id, IP: host})

	// The payload is read straight into buf after room for a binary frame's header, so a v2 tunnel needs no second copy.
	raw, _ := rawID(id)
	buf := make([]byte, frameHeaderLen+32*1024)
	if c.bin {
		putFrameHeader(buf, frameTCP, raw)
	}
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
		n, err := conn.Read(buf[frameHeaderLen:])
		if n > 0 {
			deadline = c.rel.idlePlayerTimeout
			c.bytesIn.Add(int64(n))
			if c.bin {
				c.sendFramed(buf[:frameHeaderLen+n])
			} else {
				c.send(Msg{Type: "data", Conn: id, Data: base64.StdEncoding.EncodeToString(buf[frameHeaderLen : frameHeaderLen+n])})
			}
		}
		if err != nil {
			break
		}
	}

	c.send(Msg{Type: "close", Conn: id})
	c.mu.Lock()
	delete(c.players, id)
	c.releasePlayerCountLocked(host)
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
		if c.udp != nil {
			c.udp.close()
		}
		if c.closed != nil {
			close(c.closed)
		}
		r.stopDirect(c) // players of a stopped host go back to the relay, and its IP leaves DNS
		r.hostGone(c)   // its join code stops working and players waiting to connect are told
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
// does NOT return a per-tunnel list: publishing every tunnel's public port would
// turn it into a directory of everyone's games. Only hosts that explicitly opt in
// appear publicly, via /api/servers (see listing.go).
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

	status := map[string]any{
		"online":  true,
		"tunnels": count,
		"players": totalPlayers,
		"totals": map[string]any{
			"bytes_in":    totalIn,
			"bytes_out":   totalOut,
			"total_conns": totalConns,
		},
	}
	if message, seconds, ok := r.activeNotice(); ok {
		status["notice"] = map[string]any{"message": message, "seconds_left": seconds}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(status)
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
	// Tell every host first (in parallel, and never for more than two seconds in total), so an app can
	// say "restarting, reconnecting" instead of looking broken.
	var told sync.WaitGroup
	for _, c := range clients {
		c.endWith(endShutdown)
		told.Add(1)
		go func(c *client) {
			defer told.Done()
			c.send(Msg{Type: "notice", Message: "The VoxelPort relay is restarting. Aapka server apne aap reconnect ho jayega (your server reconnects by itself)."})
		}(c)
	}
	done := make(chan struct{})
	go func() { told.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
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

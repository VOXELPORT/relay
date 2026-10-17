// loadgen is a live load / soak generator for the VoxelPort relay.
//
// It behaves like real traffic: it opens N host tunnels (each a WebSocket
// "register" like the mod/app does), receives each tunnel's public port, then
// opens M real TCP "player" connections to play.voxelport.in:<port> per host.
// Every player pushes bytes; each host echoes them back, so both the upload and
// download paths (and /api/status player counts) reflect genuine, live
// connections — not a fabricated number.
//
// Two ways to drive the players:
//
//   - the original way: a small payload every interval (-payload-bytes, -interval);
//   - throughput mode (-bytes-per-sec N): every player sends N bytes a second, in timestamped frames, and the echo
//     is timed, so the report shows Mbps each way and p50/p99 echo latency. Add -bin to use protocol v2 (binary
//     data frames) and -relay-pid to see the relay's own CPU and memory next to the traffic.
//
// Examples:
//
//	loadgen -hosts 5 -players 20 -duration 10m
//	loadgen -hosts 10 -players 10 -bytes-per-sec 100000 -duration 5m -bin -relay-pid 1234
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Msg mirrors the relay's wire envelope (see relay.go).
type Msg struct {
	Type    string `json:"type"`
	Token   string `json:"token,omitempty"`
	Port    int    `json:"port,omitempty"`
	Conn    string `json:"conn,omitempty"`
	IP      string `json:"ip,omitempty"`
	Data    string `json:"data,omitempty"`
	Message string `json:"message,omitempty"`
	Bin     int    `json:"bin,omitempty"`
}

// Protocol v2 frame layout (see the relay's README): kind byte, 16 id bytes, payload.
const (
	frameTCP       byte = 0x01
	frameHeaderLen      = 17
)

// Live counters, reported by the progress ticker.
var (
	hostsUp      atomic.Int64
	playersUp    atomic.Int64
	playersPeak  atomic.Int64
	bytesSent    atomic.Int64 // player -> relay
	bytesEchoed  atomic.Int64 // what the hosts received and echoed back (relay -> host)
	bytesBack    atomic.Int64 // relay -> player (the echo arriving, throughput mode)
	dialFailures atomic.Int64
	disconnects  atomic.Int64 // players or hosts that were cut off before the run ended
)

// tokenPrefix, when set, is embedded so generated tokens read vp_<prefix>… and
// pass a relay running with a matching -token-secret.
var tokenPrefix string

// latencies collects echo round-trip times: the current report window, and the whole run.
type latencies struct {
	mu     sync.Mutex
	window []int64
	all    []int64
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.window = append(l.window, int64(d))
	l.mu.Unlock()
}

// takeWindow returns the percentiles of the window since the last call (and folds it into the whole run).
func (l *latencies) takeWindow() (p50, p99 time.Duration, n int) {
	l.mu.Lock()
	w := l.window
	l.window = nil
	l.all = append(l.all, w...)
	l.mu.Unlock()
	return percentiles(w)
}

func (l *latencies) total() (p50, p99 time.Duration, n int) {
	l.mu.Lock()
	all := append([]int64(nil), l.all...)
	all = append(all, l.window...)
	l.mu.Unlock()
	return percentiles(all)
}

func percentiles(v []int64) (p50, p99 time.Duration, n int) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	sorted := append([]int64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(sorted)-1))
		return time.Duration(sorted[i])
	}
	return at(0.50), at(0.99), len(sorted)
}

var echoLatency latencies

// settings is what the run was asked to do, shared by every host and player.
type settings struct {
	relayWS    string
	playerHost string
	players    int
	payload    int
	interval   time.Duration
	ramp       time.Duration
	bytesPerS  int  // throughput mode when > 0
	bin        bool // ask the relay for protocol v2
}

func main() {
	relayWS := flag.String("relay-ws", "wss://relay.voxelport.in/ws", "host WebSocket endpoint (register here)")
	playerHost := flag.String("player-host", "play.voxelport.in", "hostname players TCP-connect to")
	hosts := flag.Int("hosts", 5, "number of host tunnels to open")
	players := flag.Int("players", 20, "player TCP connections per host")
	durStr := flag.String("duration", "10m", "how long to sustain the load (e.g. 90s, 30m, 3h)")
	payload := flag.Int("payload-bytes", 200, "bytes each player sends per tick (original mode)")
	intStr := flag.String("interval", "5s", "how often each player sends a payload (original mode)")
	rampStr := flag.String("ramp", "30s", "spread connection setup over this window")
	progStr := flag.String("progress", "10s", "progress report interval")
	insecure := flag.Bool("insecure", false, "skip TLS verification (testing only)")
	tokenPref := flag.String("token-prefix", "", "secret to embed so tokens are shaped vp_<secret>… (matches a gated relay)")
	bytesPerSec := flag.Int("bytes-per-sec", 0, "throughput mode: bytes per second each player sends (and gets echoed back); 0 = the original payload/interval mode")
	bin := flag.Bool("bin", false, "ask the relay for protocol v2 (binary data frames) instead of base64 in JSON")
	relayPID := flag.Int("relay-pid", 0, "process id of the relay on this machine (Linux): report its CPU and memory too")
	flag.Parse()
	tokenPrefix = *tokenPref

	duration := mustDur(*durStr, "duration")
	interval := mustDur(*intStr, "interval")
	ramp := mustDur(*rampStr, "ramp")
	progress := mustDur(*progStr, "progress")
	if *bytesPerSec < 0 {
		log.Fatalf("invalid -bytes-per-sec %d", *bytesPerSec)
	}
	cfg := settings{
		relayWS: *relayWS, playerHost: *playerHost, players: *players, payload: *payload, interval: interval,
		ramp: ramp, bytesPerS: *bytesPerSec, bin: *bin,
	}

	log.SetFlags(log.Ltime)
	log.Printf("loadgen starting: %d hosts × %d players = %d target connections",
		*hosts, *players, *hosts**players)
	mode := fmt.Sprintf("payload=%dB interval=%s", *payload, interval)
	if cfg.bytesPerS > 0 {
		mode = fmt.Sprintf("throughput: %d B/s per player (%s/s total each way)", cfg.bytesPerS, human(int64(cfg.bytesPerS**hosts**players)))
	}
	proto := "v1 (base64 in JSON)"
	if cfg.bin {
		proto = "v2 (binary frames)"
	}
	log.Printf("relay-ws=%s player-host=%s duration=%s %s ramp=%s protocol=%s", *relayWS, *playerHost, duration, mode, ramp, proto)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	// Ctrl+C ends the run early and cleanly.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		log.Printf("interrupt received — draining connections…")
		cancel()
	}()

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	if *insecure {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	// Stagger host startup across the ramp window.
	hostGap := time.Duration(0)
	if *hosts > 1 {
		hostGap = ramp / time.Duration(*hosts)
	}

	started := time.Now()
	go reportProgress(ctx, progress, *relayWS, *relayPID)

	var wg sync.WaitGroup
	for h := 0; h < *hosts; h++ {
		select {
		case <-ctx.Done():
		case <-time.After(hostGap):
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runHost(ctx, idx, dialer, cfg)
		}(h)
	}
	wg.Wait()

	elapsed := time.Since(started)
	log.Printf("done after %s. sent=%s echoed=%s peak_players=%d dial_failures=%d disconnects=%d",
		elapsed.Round(time.Second), human(bytesSent.Load()), human(bytesEchoed.Load()), playersPeak.Load(), dialFailures.Load(), disconnects.Load())
	if cfg.bytesPerS > 0 {
		p50, p99, n := echoLatency.total()
		secs := elapsed.Seconds()
		log.Printf("summary: avg up %.1f Mbps, avg down %.1f Mbps, echo latency p50 %s p99 %s over %d echoes",
			mbps(bytesSent.Load(), secs), mbps(bytesBack.Load(), secs), p50.Round(time.Microsecond), p99.Round(time.Microsecond), n)
	}
}

// runHost opens one tunnel and drives its players until ctx is cancelled.
func runHost(ctx context.Context, idx int, dialer websocket.Dialer, cfg settings) {
	token := genToken()
	ws, _, err := dialer.DialContext(ctx, cfg.relayWS, nil)
	if err != nil {
		log.Printf("[host %d] dial failed: %v", idx, err)
		dialFailures.Add(1)
		return
	}
	defer ws.Close()

	var writeMu sync.Mutex
	send := func(m Msg) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(m)
	}
	sendRaw := func(mt int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteMessage(mt, data)
	}

	reg := Msg{Type: "register", Token: token}
	if cfg.bin {
		reg.Bin = 1
	}
	if err := send(reg); err != nil {
		log.Printf("[host %d] register write failed: %v", idx, err)
		return
	}

	// First frame back must be the assigned port.
	var port Msg
	if err := ws.ReadJSON(&port); err != nil {
		log.Printf("[host %d] no port frame: %v", idx, err)
		return
	}
	if port.Type == "error" {
		log.Printf("[host %d] relay rejected register: %s", idx, port.Message)
		return
	}
	if port.Type != "port" || port.Port == 0 {
		log.Printf("[host %d] unexpected first frame: %+v", idx, port)
		return
	}
	useBin := cfg.bin && port.Bin == 1
	if cfg.bin && !useBin {
		log.Printf("[host %d] the relay did not confirm protocol v2: this tunnel stays on JSON", idx)
	}
	hostsUp.Add(1)
	defer hostsUp.Add(-1)
	log.Printf("[host %d] tunnel up on %s:%d — opening %d players", idx, cfg.playerHost, port.Port, cfg.players)

	// Reader loop: echo any data the relay forwards from players back to them,
	// so the host->player (bytesOut) path is exercised too.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				if ctx.Err() == nil {
					disconnects.Add(1)
					log.Printf("[host %d] tunnel lost: %v", idx, err)
				}
				return
			}
			if mt == websocket.BinaryMessage {
				if len(data) > frameHeaderLen && data[0] == frameTCP {
					bytesEchoed.Add(int64(len(data) - frameHeaderLen))
					_ = sendRaw(websocket.BinaryMessage, data) // the same frame goes back: same id, same bytes
				}
				continue
			}
			var m Msg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m.Type == "data" {
				bytesEchoed.Add(int64(base64Len(m.Data)))
				_ = send(Msg{Type: "data", Conn: m.Conn, Data: m.Data})
			}
		}
	}()

	// Keepalive pings.
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-readerDone:
				return
			case <-t.C:
				_ = send(Msg{Type: "ping"})
			}
		}
	}()

	// Spread player connects across the ramp window.
	playerGap := time.Duration(0)
	if cfg.players > 1 {
		playerGap = cfg.ramp / time.Duration(cfg.players)
	}

	addr := net.JoinHostPort(cfg.playerHost, fmt.Sprintf("%d", port.Port))
	var pwg sync.WaitGroup
	for p := 0; p < cfg.players; p++ {
		select {
		case <-ctx.Done():
			pwg.Wait()
			return
		case <-time.After(playerGap):
		}
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			if cfg.bytesPerS > 0 {
				runThroughputPlayer(ctx, addr, cfg.bytesPerS)
			} else {
				runPlayer(ctx, addr, cfg.payload, cfg.interval)
			}
		}()
	}
	pwg.Wait()
}

// runPlayer opens one TCP connection and pushes payloads until ctx ends.
func runPlayer(ctx context.Context, addr string, payload int, interval time.Duration) {
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		dialFailures.Add(1)
		return
	}
	defer conn.Close()

	n := playersUp.Add(1)
	if n > playersPeak.Load() {
		playersPeak.Store(n)
	}
	defer playersUp.Add(-1)

	// Drain whatever the relay echoes back.
	go io.Copy(io.Discard, conn)

	buf := make([]byte, payload)
	rand.Read(buf)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write(buf); err != nil {
				if ctx.Err() == nil {
					disconnects.Add(1)
				}
				return
			}
			bytesSent.Add(int64(len(buf)))
		}
	}
}

// frameSizeFor is the size of the timestamped frames a player sends at bytesPerSec: about a tenth of a second of
// traffic, but never so small that the timestamp doesn't fit or so big that one frame is a whole burst.
func frameSizeFor(bytesPerSec int) int {
	f := bytesPerSec / 10
	if f < 64 {
		f = 64
	}
	if f > 16*1024 {
		f = 16 * 1024
	}
	return f
}

// runThroughputPlayer sends frames at a steady rate; each starts with its send time, and the host echoes it back
// unchanged, so the difference is the round trip through the relay, the host and the relay again.
func runThroughputPlayer(ctx context.Context, addr string, bytesPerSec int) {
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		dialFailures.Add(1)
		return
	}
	defer conn.Close()

	n := playersUp.Add(1)
	if n > playersPeak.Load() {
		playersPeak.Store(n)
	}
	defer playersUp.Add(-1)

	size := frameSizeFor(bytesPerSec)
	every := time.Duration(float64(time.Second) * float64(size) / float64(bytesPerSec))

	// Reader: frames come back whole and in order (TCP), so read exactly one at a time and time it.
	go func() {
		buf := make([]byte, size)
		for {
			if _, err := io.ReadFull(conn, buf); err != nil {
				return
			}
			bytesBack.Add(int64(size))
			sent := int64(binary.BigEndian.Uint64(buf[:8]))
			echoLatency.add(time.Duration(time.Now().UnixNano() - sent))
		}
	}()

	frame := make([]byte, size)
	rand.Read(frame)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			binary.BigEndian.PutUint64(frame[:8], uint64(time.Now().UnixNano()))
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write(frame); err != nil {
				if ctx.Err() == nil {
					disconnects.Add(1)
				}
				return
			}
			bytesSent.Add(int64(size))
		}
	}
}

// reportProgress prints local counters and, when reachable, the relay's own
// /api/status view so you can confirm the two agree.
func reportProgress(ctx context.Context, every time.Duration, relayWS string, relayPID int) {
	statusURL := statusURLFrom(relayWS)
	t := time.NewTicker(every)
	defer t.Stop()
	lastTime := time.Now()
	var lastSent, lastBack int64
	cpu := newCPUSampler(relayPID)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			secs := now.Sub(lastTime).Seconds()
			sent, back := bytesSent.Load(), bytesBack.Load()
			line := fmt.Sprintf("[progress] hosts_up=%d players_up=%d peak=%d sent=%s echoed=%s dial_fail=%d disconnects=%d",
				hostsUp.Load(), playersUp.Load(), playersPeak.Load(),
				human(sent), human(bytesEchoed.Load()), dialFailures.Load(), disconnects.Load())
			if back > 0 || echoLatencyHasData() {
				p50, p99, n := echoLatency.takeWindow()
				line += fmt.Sprintf("  | up %.1f Mbps down %.1f Mbps | echo p50 %s p99 %s (%d)",
					mbps(sent-lastSent, secs), mbps(back-lastBack, secs), p50.Round(time.Microsecond), p99.Round(time.Microsecond), n)
			}
			if relayPID > 0 {
				line += "  | relay " + cpu.sample(now)
			}
			if s := fetchStatus(statusURL); s != "" {
				line += "  | status: " + s
			}
			log.Print(line)
			lastTime, lastSent, lastBack = now, sent, back
		}
	}
}

func echoLatencyHasData() bool {
	echoLatency.mu.Lock()
	defer echoLatency.mu.Unlock()
	return len(echoLatency.window)+len(echoLatency.all) > 0
}

// mbps is megabits per second for n bytes over secs seconds.
func mbps(n int64, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return float64(n) * 8 / 1e6 / secs
}

// base64Len is how many bytes a base64 string decodes to (without decoding it: the host only echoes it).
func base64Len(s string) int {
	n := len(s) / 4 * 3
	if len(s) >= 2 && s[len(s)-1] == '=' {
		n--
		if s[len(s)-2] == '=' {
			n--
		}
	}
	return n
}

func fetchStatus(url string) string {
	if url == "" {
		return ""
	}
	c := http.Client{Timeout: 8 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var s struct {
		Tunnels int `json:"tunnels"`
		Players int `json:"players"`
	}
	if json.NewDecoder(resp.Body).Decode(&s) != nil {
		return ""
	}
	return fmt.Sprintf("tunnels=%d players=%d", s.Tunnels, s.Players)
}

// statusURLFrom turns wss://relay.voxelport.in/ws into https://relay…/api/status.
func statusURLFrom(relayWS string) string {
	scheme := "https"
	rest := relayWS
	switch {
	case len(rest) > 6 && rest[:6] == "wss://":
		rest = rest[6:]
	case len(rest) > 5 && rest[:5] == "ws://":
		scheme, rest = "http", rest[5:]
	default:
		return ""
	}
	if i := indexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return scheme + "://" + rest + "/api/status"
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// genToken returns a fresh vp_ device token matching the relay's accepted shape.
func genToken() string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	b := make([]byte, 22)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		b[i] = alpha[n.Int64()]
	}
	return "vp_" + tokenPrefix + string(b)
}

func mustDur(s, name string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Fatalf("invalid -%s %q: %v", name, s, err)
	}
	return d
}

func human(n int64) string {
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

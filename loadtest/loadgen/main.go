// loadgen is a live load / soak generator for the VoxelPort relay.
//
// It behaves like real traffic: it opens N host tunnels (each a WebSocket
// "register" like the mod/app does), receives each tunnel's public port, then
// opens M real TCP "player" connections to play.voxelport.in:<port> per host.
// Every player pushes a small payload on an interval; each host echoes the
// bytes back, so both the upload and download paths (and /api/status player
// counts) reflect genuine, live connections — not a fabricated number.
//
// Example:
//
//	loadgen -hosts 5 -players 20 -duration 10m
//
// That shows up as 5 tunnels × 20 = 100 concurrent players on /api/status.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
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
}

// Live counters, reported by the progress ticker.
var (
	hostsUp      atomic.Int64
	playersUp    atomic.Int64
	playersPeak  atomic.Int64
	bytesSent    atomic.Int64 // player -> relay
	bytesEchoed  atomic.Int64 // relay -> player (echo received)
	dialFailures atomic.Int64
)

// tokenPrefix, when set, is embedded so generated tokens read vp_<prefix>… and
// pass a relay running with a matching -token-secret.
var tokenPrefix string

func main() {
	relayWS := flag.String("relay-ws", "wss://relay.voxelport.in/ws", "host WebSocket endpoint (register here)")
	playerHost := flag.String("player-host", "play.voxelport.in", "hostname players TCP-connect to")
	hosts := flag.Int("hosts", 5, "number of host tunnels to open")
	players := flag.Int("players", 20, "player TCP connections per host")
	durStr := flag.String("duration", "10m", "how long to sustain the load (e.g. 90s, 30m, 3h)")
	payload := flag.Int("payload-bytes", 200, "bytes each player sends per tick")
	intStr := flag.String("interval", "5s", "how often each player sends a payload")
	rampStr := flag.String("ramp", "30s", "spread connection setup over this window")
	progStr := flag.String("progress", "15s", "progress report interval")
	insecure := flag.Bool("insecure", false, "skip TLS verification (testing only)")
	tokenPref := flag.String("token-prefix", "", "secret to embed so tokens are shaped vp_<secret>… (matches a gated relay)")
	flag.Parse()
	tokenPrefix = *tokenPref

	duration := mustDur(*durStr, "duration")
	interval := mustDur(*intStr, "interval")
	ramp := mustDur(*rampStr, "ramp")
	progress := mustDur(*progStr, "progress")

	log.SetFlags(log.Ltime)
	log.Printf("loadgen starting: %d hosts × %d players = %d target connections",
		*hosts, *players, *hosts**players)
	log.Printf("relay-ws=%s player-host=%s duration=%s payload=%dB interval=%s ramp=%s",
		*relayWS, *playerHost, duration, *payload, interval, ramp)

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

	go reportProgress(ctx, progress, *relayWS)

	var wg sync.WaitGroup
	for h := 0; h < *hosts; h++ {
		select {
		case <-ctx.Done():
		case <-time.After(hostGap):
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runHost(ctx, idx, dialer, *relayWS, *playerHost, *players, *payload, interval, ramp)
		}(h)
	}
	wg.Wait()

	log.Printf("done. sent=%s echoed=%s peak_players=%d dial_failures=%d",
		human(bytesSent.Load()), human(bytesEchoed.Load()), playersPeak.Load(), dialFailures.Load())
}

// runHost opens one tunnel and drives its players until ctx is cancelled.
func runHost(ctx context.Context, idx int, dialer websocket.Dialer, relayWS, playerHost string,
	nPlayers, payload int, interval, ramp time.Duration) {

	token := genToken()
	ws, _, err := dialer.DialContext(ctx, relayWS, nil)
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

	if err := send(Msg{Type: "register", Token: token}); err != nil {
		log.Printf("[host %d] register write failed: %v", idx, err)
		return
	}

	// First frame back must be the assigned port.
	var reg Msg
	if err := ws.ReadJSON(&reg); err != nil {
		log.Printf("[host %d] no port frame: %v", idx, err)
		return
	}
	if reg.Type == "error" {
		log.Printf("[host %d] relay rejected register: %s", idx, reg.Message)
		return
	}
	if reg.Type != "port" || reg.Port == 0 {
		log.Printf("[host %d] unexpected first frame: %+v", idx, reg)
		return
	}
	port := reg.Port
	hostsUp.Add(1)
	defer hostsUp.Add(-1)
	log.Printf("[host %d] tunnel up on %s:%d — opening %d players", idx, playerHost, port, nPlayers)

	// Reader loop: echo any data the relay forwards from players back to them,
	// so the host->player (bytesOut) path is exercised too.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var m Msg
			if err := ws.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == "data" {
				bytesEchoed.Add(int64(len(m.Data)))
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
	if nPlayers > 1 {
		playerGap = ramp / time.Duration(nPlayers)
	}

	addr := net.JoinHostPort(playerHost, fmt.Sprintf("%d", port))
	var pwg sync.WaitGroup
	for p := 0; p < nPlayers; p++ {
		select {
		case <-ctx.Done():
			pwg.Wait()
			return
		case <-time.After(playerGap):
		}
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			runPlayer(ctx, addr, payload, interval)
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
				return
			}
			bytesSent.Add(int64(len(buf)))
		}
	}
}

// reportProgress prints local counters and, when reachable, the relay's own
// /api/status view so you can confirm the two agree.
func reportProgress(ctx context.Context, every time.Duration, relayWS string) {
	statusURL := statusURLFrom(relayWS)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			line := fmt.Sprintf("[progress] hosts_up=%d players_up=%d peak=%d sent=%s echoed=%s dial_fail=%d",
				hostsUp.Load(), playersUp.Load(), playersPeak.Load(),
				human(bytesSent.Load()), human(bytesEchoed.Load()), dialFailures.Load())
			if s := fetchStatus(statusURL); s != "" {
				line += "  | relay: " + s
			}
			log.Print(line)
		}
	}
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

// silence unused import if base64 becomes unreferenced during edits.
var _ = base64.StdEncoding

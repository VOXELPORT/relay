package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sampleEvery = 5 * time.Second
	shortLen    = 720  // 1 hour of 5-second samples
	longLen     = 1440 // 24 hours of 1-minute averages
)

// upstreams talks to the relay's operator API and the website's stats API.
type upstreams struct {
	relay, web, token string
	client            *http.Client
}

func (u *upstreams) get(base, path string) (map[string]any, error) {
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+u.token)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errString(resp.Status)
	}
	return out, nil
}

func (u *upstreams) action(body []byte) (int, map[string]any) {
	req, _ := http.NewRequest("POST", u.relay+"/action", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return http.StatusBadGateway, map[string]any{"error": "relay unreachable"}
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	return resp.StatusCode, out
}

type errString string

func (e errString) Error() string { return string(e) }

// sample is one point on the dashboard's charts.
type sample struct {
	T        int64   `json:"t"` // unix ms
	CPU      float64 `json:"cpu"`
	MemUsed  float64 `json:"mem"` // bytes
	NetRx    float64 `json:"rx"`  // bytes/s on the chosen interface
	NetTx    float64 `json:"tx"`
	RelayIn  float64 `json:"rin"` // bytes/s through the relay, players -> hosts
	RelayOut float64 `json:"rout"`
	Players  float64 `json:"players"`
	Tunnels  float64 `json:"tunnels"`
	TempC    float64 `json:"temp"`
	Load1    float64 `json:"load"`
	DiskUsed float64 `json:"disk"` // bytes used on /
	WebLive  float64 `json:"web"`  // website visitors active in the last 5 minutes
}

type sampler struct {
	iface string
	up    *upstreams
	st    *store
	tr    *tracker

	mu        sync.Mutex
	short     []sample
	long      []sample
	pending   []sample // samples since the last 1-minute average
	latest    map[string]any
	relay     map[string]any
	relayErr  string
	web       map[string]any
	webErr    string
	services  map[string]string
	prevCPU   [2]uint64 // busy, total
	prevNet   [2]uint64 // rx, tx
	prevRelay [2]float64
	prevAt    time.Time
}

func newSampler(iface string, up *upstreams, st *store) *sampler {
	if iface == "" {
		iface = busiestIface()
	}
	s := &sampler{iface: iface, up: up, st: st, tr: newTracker(st)}
	// Pick up where we left off: the charts survive a restart.
	now := time.Now()
	s.short = st.raw5s(now.Add(-time.Hour), now)
	s.long = st.minutes(now.Add(-24*time.Hour), now)
	return s
}

func (s *sampler) run() {
	s.tick()
	for range time.Tick(sampleEvery) {
		s.tick()
	}
}

func (s *sampler) tick() {
	now := time.Now()
	relay, relayErr := s.up.get(s.up.relay, "/state")
	web, webErr := s.up.get(s.up.web, "/stats")
	busy, total := readCPU()
	rx, tx := readNet(s.iface)
	memTotal, memAvail := readMem()
	services := serviceStates("voxelport-relay", "voxelport-web", "voxelport-admin", "cloudflared", "tailscaled", "wings", "docker")

	s.tr.observe(relay, relayErr, webErr, services)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.services = services
	s.relay, s.web = relay, web
	s.relayErr, s.webErr = errText(relayErr), errText(webErr)

	pt := sample{T: now.UnixMilli(), MemUsed: float64(memTotal - memAvail)}
	var rIn, rOut float64
	if relay != nil {
		t, _ := relay["totals"].(map[string]any)
		rIn, _ = t["bytes_in"].(float64)
		rOut, _ = t["bytes_out"].(float64)
		pt.Players, _ = t["players"].(float64)
		pt.Tunnels, _ = t["tunnels"].(float64)
	}
	if !s.prevAt.IsZero() {
		dt := now.Sub(s.prevAt).Seconds()
		if total > s.prevCPU[1] {
			pt.CPU = 100 * float64(busy-s.prevCPU[0]) / float64(total-s.prevCPU[1])
		}
		pt.NetRx = rate(float64(rx), float64(s.prevNet[0]), dt)
		pt.NetTx = rate(float64(tx), float64(s.prevNet[1]), dt)
		pt.RelayIn = rate(rIn, s.prevRelay[0], dt)
		pt.RelayOut = rate(rOut, s.prevRelay[1], dt)
	}
	s.prevCPU = [2]uint64{busy, total}
	s.prevNet = [2]uint64{rx, tx}
	s.prevRelay = [2]float64{rIn, rOut}
	s.prevAt = now

	var disk syscall.Statfs_t
	diskTotal, diskFree := uint64(0), uint64(0)
	if syscall.Statfs("/", &disk) == nil {
		diskTotal = disk.Blocks * uint64(disk.Bsize)
		diskFree = disk.Bavail * uint64(disk.Bsize)
	}
	load := readLoad()
	temp := readTemp()
	pt.TempC, pt.DiskUsed = temp, float64(diskTotal-diskFree)
	if len(load) > 0 {
		pt.Load1 = load[0]
	}
	if web != nil {
		pt.WebLive, _ = web["live"].(float64)
	}

	s.short = appendCapped(s.short, pt, shortLen)
	s.st.addRaw(pt)
	s.pending = append(s.pending, pt)
	if len(s.pending) >= int(time.Minute/sampleEvery) {
		m := average(s.pending)
		s.long = appendCapped(s.long, m, longLen)
		s.st.addMinute(m)
		s.pending = s.pending[:0]
	}
	s.latest = map[string]any{
		"cpu":          pt.CPU,
		"cores":        numCPU(),
		"load":         load,
		"mem_total":    memTotal,
		"mem_used":     memTotal - memAvail,
		"disk_total":   diskTotal,
		"disk_free":    diskFree,
		"uptime":       readUptime(),
		"temp_c":       temp,
		"iface":        s.iface,
		"net_rx":       pt.NetRx,
		"net_tx":       pt.NetTx,
		"net_rx_total": rx,
		"net_tx_total": tx,
		"relay_in":     pt.RelayIn,
		"relay_out":    pt.RelayOut,
		"hostname":     hostname(),
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func rate(cur, prev, dt float64) float64 {
	if dt <= 0 || cur < prev { // counter reset (service restarted)
		return 0
	}
	return (cur - prev) / dt
}

func appendCapped(s []sample, p sample, n int) []sample {
	s = append(s, p)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

func average(ps []sample) sample {
	var a sample
	for _, p := range ps {
		a.CPU += p.CPU
		a.MemUsed += p.MemUsed
		a.NetRx += p.NetRx
		a.NetTx += p.NetTx
		a.RelayIn += p.RelayIn
		a.RelayOut += p.RelayOut
		a.Players += p.Players
		a.Tunnels += p.Tunnels
		a.TempC += p.TempC
		a.Load1 += p.Load1
		a.DiskUsed += p.DiskUsed
		a.WebLive += p.WebLive
	}
	n := float64(len(ps))
	return sample{T: ps[len(ps)-1].T, CPU: a.CPU / n, MemUsed: a.MemUsed / n, NetRx: a.NetRx / n, NetTx: a.NetTx / n,
		RelayIn: a.RelayIn / n, RelayOut: a.RelayOut / n, Players: a.Players / n, Tunnels: a.Tunnels / n,
		TempC: a.TempC / n, Load1: a.Load1 / n, DiskUsed: a.DiskUsed / n, WebLive: a.WebLive / n}
}

// history returns chart points for a range: the last hour and day come from
// memory, longer ranges from the saved minute / hourly files.
func (s *sampler) history(rng string) map[string]any {
	now := time.Now()
	var pts []sample
	switch rng {
	case "24h":
		s.mu.Lock()
		pts = append([]sample(nil), s.long...)
		s.mu.Unlock()
	case "7d":
		pts = downsample(s.st.minutes(now.AddDate(0, 0, -7), now), 1000)
	case "30d":
		pts = downsample(s.st.hourly(now.AddDate(0, 0, -30), now), 1000)
	case "1y":
		pts = downsample(s.st.hourly(now.AddDate(-1, 0, 0), now), 1000)
	case "all":
		pts = downsample(s.st.hourly(time.Unix(0, 0), now), 1000)
	default:
		rng = "1h"
		s.mu.Lock()
		pts = append([]sample(nil), s.short...)
		s.mu.Unlock()
	}
	if pts == nil {
		pts = []sample{}
	}
	return map[string]any{"range": rng, "points": pts}
}

func overview(up *upstreams, s *sampler, dl *downloads) map[string]any {
	s.mu.Lock()
	out := map[string]any{
		"now":       time.Now().UnixMilli(),
		"system":    s.latest,
		"services":  s.services,
		"relay":     s.relay,
		"relay_err": s.relayErr,
		"web":       s.web,
		"web_err":   s.webErr,
	}
	s.mu.Unlock()
	out["downloads"] = dl.snapshot()
	return out
}

// ─── /proc readers (Linux) ───────────────────────────────────────────────────

func readCPU() (busy, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	for i, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, total
}

func readMem() (total, avail uint64) {
	data, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			avail = n * 1024
		}
	}
	return
}

func netCounters() map[string][2]uint64 {
	out := map[string][2]uint64{}
	data, _ := os.ReadFile("/proc/net/dev")
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[strings.TrimSpace(name)] = [2]uint64{rx, tx}
	}
	return out
}

func readNet(iface string) (rx, tx uint64) {
	c := netCounters()[iface]
	return c[0], c[1]
}

// busiestIface picks the physical interface with the most traffic, skipping
// loopback, Docker, bridges, veths and VPNs.
func busiestIface() string {
	best, bestN := "", uint64(0)
	for name, c := range netCounters() {
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "pelican") {
			continue
		}
		if c[0]+c[1] > bestN {
			best, bestN = name, c[0]+c[1]
		}
	}
	return best
}

func readLoad() []float64 {
	data, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(data))
	out := []float64{}
	for i := 0; i < 3 && i < len(f); i++ {
		v, _ := strconv.ParseFloat(f[i], 64)
		out = append(out, v)
	}
	return out
}

func readUptime() float64 {
	data, _ := os.ReadFile("/proc/uptime")
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// readTemp returns the hottest thermal zone in °C, or 0 if none is readable.
func readTemp() float64 {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	max := 0.0
	for _, z := range zones {
		data, err := os.ReadFile(z)
		if err != nil {
			continue
		}
		v, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if v/1000 > max && v/1000 < 150 {
			max = v / 1000
		}
	}
	return max
}

func numCPU() int {
	data, _ := os.ReadFile("/proc/cpuinfo")
	return strings.Count(string(data), "\nprocessor") + boolInt(strings.HasPrefix(string(data), "processor"))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// serviceStates reads systemd unit states (read-only; never starts or stops anything).
func serviceStates(units ...string) map[string]string {
	out := map[string]string{}
	cmd := exec.Command("systemctl", append([]string{"is-active"}, units...)...)
	data, _ := cmd.Output() // non-zero exit when any unit is inactive; output is still valid
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, u := range units {
		if i < len(lines) {
			out[u] = strings.TrimSpace(lines[i])
		} else {
			out[u] = "unknown"
		}
	}
	return out
}

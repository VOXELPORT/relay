//go:build linux

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// machineInfo is the operator API's GET /system: raw counters for the
// machine the relay runs on, so a dashboard elsewhere (the NA relay is
// watched from the home server) can chart CPU, memory and internet use.
// Counters are cumulative; the dashboard turns them into rates.
func machineInfo() map[string]any {
	busy, total := sysCPU()
	memTotal, memAvail := sysMem()
	iface := sysBusiestIface()
	net := sysNetCounters()[iface]
	var disk syscall.Statfs_t
	diskTotal, diskFree := uint64(0), uint64(0)
	if syscall.Statfs("/", &disk) == nil {
		diskTotal = disk.Blocks * uint64(disk.Bsize)
		diskFree = disk.Bavail * uint64(disk.Bsize)
	}
	host, _ := os.Hostname()
	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")
	return map[string]any{
		"hostname":   host,
		"cpu_busy":   busy,
		"cpu_total":  total,
		"cores":      strings.Count("\n"+string(cpuinfo), "\nprocessor"),
		"mem_total":  memTotal,
		"mem_avail":  memAvail,
		"disk_total": diskTotal,
		"disk_free":  diskFree,
		"load":       sysFloats("/proc/loadavg", 3),
		"uptime":     sysFloats("/proc/uptime", 1),
		"iface":      iface,
		"net_rx":     net[0],
		"net_tx":     net[1],
	}
}

func sysCPU() (busy, total uint64) {
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

func sysMem() (total, avail uint64) {
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

func sysNetCounters() map[string][2]uint64 {
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

// sysBusiestIface is the physical interface with the most traffic (skipping
// loopback, containers and VPNs) — on a VPS, the one the provider bills.
func sysBusiestIface() string {
	best, bestN := "", uint64(0)
	for name, c := range sysNetCounters() {
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "tailscale") {
			continue
		}
		if c[0]+c[1] > bestN {
			best, bestN = name, c[0]+c[1]
		}
	}
	return best
}

func sysFloats(path string, n int) []float64 {
	data, _ := os.ReadFile(path)
	f := strings.Fields(string(data))
	out := []float64{}
	for i := 0; i < n && i < len(f); i++ {
		v, _ := strconv.ParseFloat(f[i], 64)
		out = append(out, v)
	}
	return out
}

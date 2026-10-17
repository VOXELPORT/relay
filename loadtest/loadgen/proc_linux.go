package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// cpuSampler reports a process's CPU use (as a share of one core, so a busy multi-threaded process can pass 100%) and
// resident memory, read from /proc. It is how a run tells "the relay's CPU is the limit" from "the network link is".
type cpuSampler struct {
	pid      int
	lastTick int64
	lastAt   time.Time
}

func newCPUSampler(pid int) *cpuSampler {
	s := &cpuSampler{pid: pid}
	if pid > 0 {
		s.lastTick, _ = processTicks(pid)
		s.lastAt = time.Now()
	}
	return s
}

// clockTicksPerSecond is USER_HZ, which is 100 on every Linux the relay runs on.
const clockTicksPerSecond = 100

// processTicks is utime+stime of pid, in clock ticks.
func processTicks(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name is in parentheses and may contain spaces: fields start after the last ")".
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("unexpected /proc stat format")
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 13 {
		return 0, fmt.Errorf("short /proc stat")
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("bad /proc stat numbers")
	}
	return utime + stime, nil
}

func processRSS(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, err := strconv.ParseInt(f[1], 10, 64)
				return kb * 1024, err
			}
		}
	}
	return 0, fmt.Errorf("no VmRSS")
}

// sample returns "cpu 123% rss 45.6MB" for the time since the last call.
func (s *cpuSampler) sample(now time.Time) string {
	ticks, err := processTicks(s.pid)
	if err != nil {
		return fmt.Sprintf("pid %d: not readable (%v)", s.pid, err)
	}
	rss, _ := processRSS(s.pid)
	secs := now.Sub(s.lastAt).Seconds()
	pct := 0.0
	if secs > 0 {
		pct = float64(ticks-s.lastTick) / clockTicksPerSecond / secs * 100
	}
	s.lastTick, s.lastAt = ticks, now
	return fmt.Sprintf("cpu %.0f%% rss %s", pct, human(rss))
}

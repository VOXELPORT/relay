//go:build !linux

package main

import "time"

// Reading another process's CPU and memory from /proc is only done on Linux, where the relay runs.
type cpuSampler struct{}

func newCPUSampler(pid int) *cpuSampler { return &cpuSampler{} }

func (*cpuSampler) sample(time.Time) string { return "cpu/rss: only on Linux" }

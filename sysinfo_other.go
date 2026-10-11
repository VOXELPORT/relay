//go:build !linux

package main

// machineInfo is only implemented on Linux, where the relay is deployed.
func machineInfo() map[string]any { return nil }

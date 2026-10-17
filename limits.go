package main

import (
	"net/netip"
	"strings"
)

// Per-address limits (tunnels, players, registrations, name claims) are counted per "key", not per raw address:
//
//   - An IPv4 address is its own key. Thousands of people can share one (carrier-grade NAT, which most Indian mobile
//     networks use), which is why the IPv4 limits are generous.
//   - An IPv6 address is reduced to its /64, because a single home or phone is handed a whole /64 and can use any
//     address in it: counting full addresses would let one person get thousands of "different" ones.
//   - An IPv4-mapped IPv6 address (::ffff:203.0.113.5) is the same address as the IPv4 one.
//   - Anything that is not an address is returned unchanged, so a bad value can never be merged with a good one.

// limitKey returns the key per-address limits are counted under for ip.
func limitKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

// isIPv6Key reports whether key (from limitKey) stands for an IPv6 /64, which has its own, lower limits.
func isIPv6Key(key string) bool {
	return strings.HasSuffix(key, "/64") && strings.Contains(key, ":")
}

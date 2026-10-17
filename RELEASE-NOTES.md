# Capacity release

A wider port pool, custom addresses that can no longer squat it, limits that work for people on shared mobile addresses, and
a cheaper way to carry game data. **It is backward compatible**: every app and mod version that is already out keeps working
against it, unchanged.

This build also contains Direct mode (a host can have players connect straight to their router instead of through the relay) and join codes (a player connects peer-to-peer with a code; the relay only introduces them). Both are opt-in per host and described in the README. Direct mode adds one DNS record per host that turns it on, on top of that host's custom address.

## What changed

### More room: ports and custom addresses

- **The default port range is `25500–29999`** (4,500 ports, was `25500–25999`). The service file, scripts and docs are updated.
  `-reserved-ports` (default `25565`) lists ports inside the range that are never handed to a tunnel, for other programs on the
  machine. A `-listen` or `-direct-listen` port inside the range must be listed there, or the relay refuses to start.
- **A custom address keeps its port for a week, not for its whole life.** The name itself still lives 60 days, but its port is
  held only `-name-port-grace` (default `168h`) after the host was last seen. After that the port is free for anyone; if the
  owner returns and it is taken, they are given another port and the DNS record is moved, so the address players type does not
  change. **Supporters' and operator-granted addresses keep their port for good**, as before.
- **Custom addresses may reserve at most half the range** (`-max-reserved-port-fraction`, default `0.5`; only names inside their
  grace, or kept for good, count). Past that, new claims fail with "Custom addresses are full right now — try again later."
  Supporters and operator grants are exempt.
- **New claims are limited per source**: `-name-claims-per-ip-day` (default 10 per IPv4 address) and
  `-name-claims-per-ip6-day` (default 3 per IPv6 /64), per 24 hours. The existing per-host attempt limit stays.

- **`-names-supporters-only`** (off by default) makes new custom addresses a supporter perk: only hosts in `supporters.txt` and the
  operator's own grants get a new one; everyone else is told so. Addresses that already exist keep working, and releasing one is
  still allowed. Use it when the DNS zone is near its record limit (each address is two records).

### Limits that work behind carrier-grade NAT

Per-source limits now count per *key* (an IPv4 address, or the /64 of an IPv6 address; IPv4-mapped IPv6 counts as IPv4), with
separate IPv4 and IPv6 values:

| Limit | IPv4 | IPv6 /64 | Flags |
|---|---|---|---|
| active tunnels | 25 | 5 | `-max-tunnels-per-ip`, `-max-tunnels-per-ip6` |
| players per tunnel | 32 | 16 | `-max-players-per-ip`, `-max-players-per-ip6` |
| registrations per minute | 30 | 10 | `-reg-limit-ip`, `-reg-limit-ip6` |
| new custom addresses per day | 10 | 3 | `-name-claims-per-ip-day`, `-name-claims-per-ip6-day` |

The old flag names keep working and now mean the IPv4 value. (The old defaults were 5 tunnels and 16 players per address and
10 registrations a minute, which locked out ordinary people on mobile networks.) Refusal messages now say what happened and
suggest Wi-Fi.

### Protocol v2: binary data frames (opt-in)

A host that sends `"bin": 1` in `register` and is answered with `"bin": 1` in `port` sends and receives game data as binary
WebSocket messages (kind byte, 16 raw id bytes, payload) instead of base64 inside JSON. Control frames stay JSON. About 33%
less bandwidth, and far less CPU per byte: encoding and decoding a 64 KB payload went from about 500 µs to about 15 µs
(`go test -bench DataPath`), and in a short loopback test at 8 Mbps each way the relay used about half the CPU and answered
about twice as fast. Hosts that don't ask are served exactly as before. Details: the README's "Protocol v2" section.

### Load testing

`loadtest/loadgen` can drive a steady number of bytes per second per player and report Mbps each way, echo latency (p50/p99),
disconnects and the relay's own CPU and memory, in either protocol. `loadtest/THROUGHPUT.md` has recipes and how to read them.

## New flags

`-reserved-ports`, `-name-port-grace`, `-max-reserved-port-fraction`, `-name-claims-per-ip-day`, `-name-claims-per-ip6-day`,
`-max-tunnels-per-ip6`, `-max-players-per-ip6`, `-reg-limit-ip`, `-reg-limit-ip6`. Defaults above; all in the README's flag table.
Changed defaults: `-max-port` 29999, `-max-tunnels-per-ip` 25, `-max-players-per-ip` 32.

## Upgrading: do it in this order

1. **Deploy the relay first.** It is backward compatible. A restart makes every host reconnect by itself within seconds.
   If your service passes `-min-port`/`-max-port` explicitly, those are what apply: change them to use the wider range (and add
   any port another program uses, or your `-direct-listen` port, to `-reserved-ports`).
2. **Then the router and firewall**: forward/allow the wider player range (`25500-29999`, TCP, plus UDP for Bedrock) if you widen it.
3. **Then ship client updates** that ask for protocol v2. Old clients never need updating for any of this.

State on disk (`-data-dir`: `names.json`, `hidden.txt`, `supporters.txt`, …) is read as it is; nothing was added to it, so a
rollback to the previous binary is safe.

If you also run a branch with other changes (for example Direct mode and join codes), merge this into it before deploying:
deploying a build made from `main` alone would drop those features.

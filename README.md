# VoxelPort Relay

The server-side counterpart to the VoxelPort desktop app. It:

1. Accepts secure WebSocket (`wss://`) connections from a host (the desktop app, or
   older releases of the discontinued Fabric mod).
2. Identifies the host by its auto-generated device token (`vp_…`) — see
   "Authentication model" below for exactly what this does and does not provide.
3. Assigns each host a public TCP port (sticky per token while the relay is up).
4. Listens on that port and bridges raw bytes between vanilla Minecraft players
   and the host, which forwards them to the real local server.

```
Player ──TCP──▶ play.voxelport.in:25xxx ──┐
                                          │  (relay)
Host  ──wss──▶ relay.voxelport.in:443 ────┘──▶ 127.0.0.1:25565 (local server)
```

Stateless and pure-Go (no cgo, no DB) — the output is a single static binary.

For the full trust model, threat model, and abuse model, see [SECURITY.md](SECURITY.md).
This README covers build/run/protocol/limits.

## Authentication model

VoxelPort uses anonymous bearer device tokens as tunnel identities. Tokens
are generated locally by the app and are not tied to an account — the
official relay accepts any well-formed token (`vp_[A-Za-z0-9_-]{10,}`).
**Knowledge of a token is sufficient to reconnect as that tunnel identity, so
a token should be treated as a secret**, the same way you'd treat a password.

- The token's SHA-256 hash is the relay's internal identity for a tunnel —
  the raw token itself is never logged (see `SECURITY.md`).
- Exactly one active tunnel per token — a new registration for the same
  token evicts the old one.
- This is genuinely accountless: there is no signup, no password, no
  database of registered hosts. That's a deliberate product decision, not an
  oversight — don't read "any well-formed token is accepted" as a bug.

### Token gate (`-token-secret`)

An optional flag requires every accepted token to start with `vp_<secret>…`.
Read this carefully before relying on it:

- It is a **shared-prefix convenience gate**, not per-user authentication. It
  stops a stranger who merely *finds the relay's URL* from opening a tunnel
  through it — nothing more.
- It does **not** work with the stock app out of the box: their tokens
  are generated locally with no prefix convention. To use this gate, you
  (the relay operator) must also configure every host you expect to connect
  — e.g. via the app's token field — to use a token starting with your
  chosen secret.
- Because VoxelPort is open source, a secret compiled into or configured
  alongside a client is not secret from someone reading the source or the
  binary. It raises the bar against casual discovery, not against a
  motivated attacker who has read this file.
- The comparison itself is constant-time (`crypto/subtle`), so response
  timing can't be used to recover the secret byte-by-byte — but that only
  matters if you're relying on the secret being unknown in the first place.

If you want the official public relay's openness with a bit of abuse
resistance, the limits below (not this flag) are what's actually doing that
work.

## Build

```bash
go build -o voxelport-relay .
# cross-compile for a Linux VPS:
GOOS=linux GOARCH=amd64 go build -o voxelport-relay .
```

## Run

```bash
./voxelport-relay -listen 127.0.0.1:2526 -min-port 25500 -max-port 29999
# serve wss:// directly (no Caddy):
./voxelport-relay -listen :443 -tls-cert fullchain.pem -tls-key privkey.pem
```

| Flag | Default | Purpose |
|---|---|---|
| `-listen` | `:8080` | panel + WebSocket listen address |
| `-min-port` / `-max-port` | 25500 / 29999 | public port range assignable to tunnels (4,500 ports) |
| `-reserved-ports` | 25565 | comma-separated ports inside the range that are never handed to a tunnel (ports other programs on the machine use). A `-listen` or `-direct-listen` port inside the range must be listed here, or the relay refuses to start |
| `-tls-cert` / `-tls-key` | (unset) | serve `wss://` directly instead of behind a reverse proxy |
| `-token-secret` | (unset) | shared-prefix gate — see "Token gate" above |
| `-max-tunnels-per-ip` / `-max-tunnels-per-ip6` | 25 / 5 | max simultaneous active tunnels from one IPv4 address / one IPv6 /64. IPv4 is higher because thousands of people can share one address (carrier-grade NAT on mobile networks); an IPv6 /64 is one household |
| `-max-players-per-ip` / `-max-players-per-ip6` | 32 / 16 | max simultaneous player connections from one IPv4 address / one IPv6 /64, per tunnel |
| `-reg-limit-ip` / `-reg-limit-ip6` | 30 / 10 | registration attempts per minute from one IPv4 address / one IPv6 /64 |
| `-trusted-proxy-cidrs` | (unset) | CIDRs allowed to supply `X-Forwarded-For`/`X-Real-IP`; loopback is always trusted |
| `-name-port-grace` | 168h | how long a custom address keeps its port reserved after its host was last seen. Supporters' and operator-granted addresses keep it for good. After the grace the port is free for anyone; when the host returns and the port is taken it gets another one and the DNS record follows, so the address does not change |
| `-max-reserved-port-fraction` | 0.5 | largest share of the port range custom addresses may reserve (only names inside their grace, or kept for good, count). Past it, new claims fail with "Custom addresses are full right now"; supporters and operator grants are exempt |
| `-names-supporters-only` | off | new custom addresses only for supporters (`supporters.txt`) and the operator's own grants. Every address is DNS records and a Cloudflare zone holds a fixed number, so this stops the zone from filling while the perk is paid. Existing addresses keep working, re-claiming one and releasing one still work; an ordinary host asking for a new one is told so in English and Hinglish |
| `-name-claims-per-ip-day` | 10 | new custom addresses one IPv4 address may claim per 24 hours |
| `-name-claims-per-ip6-day` | 3 | new custom addresses one IPv6 /64 may claim per 24 hours |

Sends `SIGTERM`/`SIGINT` for graceful shutdown: stops accepting new
connections, closes every active tunnel (control WebSocket, all player
connections, the public listener), and exits — bounded to 15s before a
forced exit, so a supervisor (systemd, Docker) can always stop it cleanly.

## Endpoints

| Path          | Purpose                                             |
|---------------|-----------------------------------------------------|
| `/ws`         | host WebSocket control connection                   |
| `/api/status` | JSON: `{online, tunnels, players, totals}`          |
| `/`           | read-only HTML status panel                         |

## Limits

The relay enforces a set of fixed and configurable limits to keep the
intentionally accountless `/ws` endpoint from being trivially abused. None
of these require an app update — see "Compatibility" below.

| Limit | Value | Purpose |
|---|---|---|
| Control-channel message size | 4 MB | bounds memory a single WebSocket frame can force the relay to allocate |
| Registration attempts | 30 per IPv4 address / 10 per IPv6 /64, per minute (`-reg-limit-ip`, `-reg-limit-ip6`) | blunts scripted registration of many fake tunnels |
| Active tunnels per source | 25 per IPv4 address / 5 per IPv6 /64 (`-max-tunnels-per-ip`, `-max-tunnels-per-ip6`) | stops one source from consuming the whole port pool |
| Control-channel idle timeout | 90s (re-armed per message) | reclaims a tunnel whose host vanished without a clean TCP close |
| Players per tunnel | 256 concurrent | stops one attacker from flooding a single host with connections |
| Players per source, per tunnel | 32 per IPv4 address / 16 per IPv6 /64 (`-max-players-per-ip`, `-max-players-per-ip6`) | stops one source from occupying most of a tunnel's player slots |
| New custom addresses per source | 10 per IPv4 address / 3 per IPv6 /64, per 24 hours | stops a script from claiming addresses to squat ports |
| Share of the port range names may reserve | 50% (`-max-reserved-port-fraction`) | offline custom addresses can never starve the hosts that are online |
| How long an absent host keeps its address's port | 7 days (`-name-port-grace`) | after that the port goes back to the pool (supporters and granted addresses keep theirs) |
| Player initial-inactivity timeout | 30s | drops a raw TCP connection that never sends anything (slot squatting) |
| Player idle timeout | 10 min (re-armed per read) | reaps a connection that's gone silent at the TCP level after being active |
| Player write queue | 128 messages, then disconnect | a slow/stuck player can't block the host's control channel or other players (see "Slow players" in SECURITY.md) |
| Sticky-port memory | 1 hour after disconnect | bounds memory used remembering "give this token its old port back" |
| Connection ID | 128 bits (`crypto/rand`) | not guessable/spoofable |

**What "source" means.** Limits are counted per *key*, not per raw address: an IPv4 address is its own key; an
IPv6 address is reduced to its /64 (one home or phone is handed a whole /64, so counting full addresses would let it
look like thousands of people); an IPv4-mapped IPv6 address (`::ffff:1.2.3.4`) is the IPv4 address. IPv4 limits are
generous because most Indian mobile networks put thousands of subscribers behind one IPv4 address (carrier-grade NAT):
a low per-address limit would lock real people out. Refusals say so in plain words ("Too many VoxelPort servers are
online from your network right now (limit 25). If you're on mobile data, try Wi-Fi.").

`X-Forwarded-For` and `X-Real-IP` are only trusted from loopback (the
documented Caddy/nginx-on-the-same-host deployments — different reverse
proxies default to different headers, so both are honored) or
`-trusted-proxy-cidrs` — a directly exposed relay can't have its rate
limits bypassed by a client spoofing either header.

## Wire protocol

JSON text frames (matches the app's tunnel client):

| Direction      | Frame |
|----------------|-------|
| host → relay   | `{"type":"register","token":"vp_…","client":"desktop/1.7.0"?,"blocked_ips":[...],"udp":true?}` |
| host → relay   | `{"type":"ping"}` |
| host → relay   | `{"type":"data","conn":id,"data":base64}` |
| host → relay   | `{"type":"close","conn":id}` |
| relay → host   | `{"type":"port","port":N}` |
| relay → host   | `{"type":"connect","conn":id,"ip":"1.2.3.4"}` |
| relay → host   | `{"type":"data","conn":id,"data":base64}` |
| relay → host   | `{"type":"close","conn":id}` |
| relay → host   | `{"type":"pong"}` |
| relay → host   | `{"type":"error","message":"..."}` |
| relay → host   | `{"type":"notice","message":"…","seconds":N?}`: an announcement from the operator; an empty message withdraws it |

### Bedrock (UDP)

A host that registers with `"udp":true` also gets UDP forwarded on the same
public port. Each remote address is a session:

| Direction      | Frame |
|----------------|-------|
| relay → host   | `{"type":"udp","conn":id,"ip":"1.2.3.4","data":base64}` (`ip` only on a session's first datagram) |
| host → relay   | `{"type":"udp","conn":id,"data":base64}` |
| either way     | `{"type":"udp_close","conn":id}` |
| relay → host   | `{"type":"udp_error","message":"..."}` (UDP couldn't be opened; TCP still works) |

Limits: 256 sessions per tunnel, 8 per source IP, 2048-byte datagrams, 60 s
idle timeout. Until a session has sent 4 datagrams, the host may send it at
most 4× the bytes it received plus 600 — so a spoofed source address can't
turn the relay into an amplifier, while a real RakNet ping/pong still fits.

### Protocol v2: binary data frames (opt-in)

In v1 every payload byte travels as base64 inside a JSON text frame, about a third more bandwidth, and a lot more CPU,
than the bytes themselves. A host can ask for v2:

1. The host's `register` frame adds `"bin":1`.
2. A relay that supports it answers with `{"type":"port","port":N,"bin":1}`. A relay that doesn't (or a host that
   didn't ask) never sets `bin`, and everything stays v1.
3. From then on, TCP and UDP payloads travel in **both directions** as *binary* WebSocket messages:

```
byte 0        kind    0x01 = TCP data, 0x02 = UDP datagram
bytes 1..16   the connection (or UDP session) id: the 16 raw bytes of the 32-character hex id the relay sends in
              `connect` (or the first UDP frame)
bytes 17..    the payload
```

Everything else stays a JSON text frame in both modes: `connect`, `close`, `ping`, `pong`, `udp_close`, names, listing,
errors and notices. In v2 a UDP session has no JSON first frame, so its source address (`ip`) is not sent (nothing but the
log ever used it). A host in v2 may still send JSON `data` / `udp` frames; the relay accepts them. A binary frame on a
tunnel that did not ask for v2 is ignored.

The relay drops, and logs (at most once per 30 s per tunnel), binary frames that are shorter than 17 bytes, have an unknown
kind, name an unknown connection, or carry a UDP datagram over 2048 bytes. Byte counters (`in=`/`out=` in the usage log, the
status totals) count payload bytes only, identically in both modes.

On a 64 KB payload, encode + decode takes about 15 µs in v2 against about 500 µs in v1 (`go test -bench DataPath`).

### Custom addresses

| Direction      | Frame |
|----------------|-------|
| host → relay   | `{"type":"claim_name","name":"steve"}` / `{"type":"release_name"}` |
| relay → host   | `{"type":"name","name":"steve","address":"steve.voxelport.in"}` (empty after release; also sent right after `port` on every connection) |
| relay → host   | `{"type":"name_error","message":"..."}` |

A claimed name creates `_minecraft._tcp.<name>` (SRV → `-public-host`:port,
which Java clients follow) and `<name>` (CNAME → `-public-host`) in the
Cloudflare zone `-dns-zone`, using `$CF_DNS_API_TOKEN` (Zone:Read + DNS:Edit).
The name permanently reserves its port for that token — even while offline —
until it's released or unused for 60 days. Names are 3–20 of `a-z0-9-`;
infrastructure names and offensive words are refused, existing DNS records are
never overwritten, and claims are rate-limited. State lives in
`<-data-dir>/names.json`; without `-data-dir` and the token, the feature is off.

### Direct mode

Lets a host's players skip the relay when the host opened a port on its router (UPnP).

| Direction      | Frame |
|----------------|-------|
| host → relay   | `{"type":"direct","port":25565}` asks for it; `{"type":"direct","port":0}` turns it off. Apps send it again after every reconnect. |
| relay → host   | `{"type":"direct","ip":"83.45.12.9","port":25565,"address":"steve.voxelport.in"}` — it works (`address` only if the host has a custom address) |
| relay → host   | `{"type":"direct","port":0}` — players use the relay again |
| relay → host   | `{"type":"direct_error","message":"..."}` — refused, or stopped working |

The relay never takes an address from the host: it uses the IP the tunnel connected from, and only a public IPv4 address
is accepted (not private, shared/CGNAT, loopback or reserved ranges, and not IPv6). It connects there and asks for the
Minecraft server-list status; only a real Minecraft answer counts. Then, for a host with a custom address, the SRV record
is pointed at an A record `<name>--d` (a label no claim can collide with, since names can't contain `--`) with the host's
IP and port, with a 60-second TTL. A host that stops answering two checks in a row (every 45 s) is put back on the relay and
told; it resumes by itself. A host that turns it off, disconnects or is replaced goes back at once and its IP leaves DNS
(retried if DNS fails, and never undoing a newer session of the same host). Addresses a relay restart left pointing at a
host are put back two minutes after start.

The host's IP address is public in DNS while this is on (the host opted in and was told), but it is never logged and never
written to disk: `names.json` only records `"direct": true`. Each host may ask 6 times per 10 minutes. The operator API's
tunnel list shows only whether a tunnel is direct.

### Join codes (peer-to-peer)

A host can let players connect to it directly, peer-to-peer, with a code; the relay only introduces them.

| Direction       | Frame |
|-----------------|-------|
| host → relay    | `{"type":"p2p","p2p":true}` turns join codes on (`false` off); `{"type":"p2p_rotate"}` makes a new code; `{"type":"peer_signal","peer":"…","signal":"…"}` answers a player; `{"type":"peer_close","peer":"…"}` drops one |
| relay → host    | `{"type":"join_code","code":"K7M2Q-9XH4T"}` (empty when off; sent after every change); `{"type":"peer","peer":"…","client":"desktop/1.9.0"}` a player is waiting; `{"type":"peer_signal",…}` from that player; `{"type":"peer_gone","peer":"…"}`; `{"type":"join_error","message":"…"}` |
| player → `/join` | `{"type":"join","code":"…","client":"…"}` first, then `{"type":"signal","signal":"…"}`, `ping`, `leave` |
| `/join` → player | `{"type":"joined","peer":"…","address":"play.voxelport.in","port":26043}` (the host's ordinary relay address, a fallback), `{"type":"signal","signal":"…"}`, `pong`, `{"type":"join_error","message":"…"}` |

The signals are WebRTC offers, answers and network candidates (at most 8 KiB each); the relay never looks inside them and no game
traffic touches it. A code is 10 characters of Crockford base32 (shown XXXXX-XXXXX, 50 bits), derived with HMAC-SHA256 from a key in
`-data-dir` (`join.key`) and the host's id plus how many times the host rotated it (`join-epochs.json`), so it is stable across
sessions and restarts and a rotation revokes the old one at once. It only works while the host is online with join codes on. Wrong
codes are limited to 10 per address per 10 minutes; each address may hold 4 `/join` connections, each host 16 waiting players,
and a player 200 setup messages a minute; a `/join` connection ends when idle for 90 s or after 10 minutes. Waiting players are
told when the host leaves, turns codes off or rotates. Codes and IP addresses are never logged; only the key and the rotation counts
are saved. Without `-data-dir` the key lives in memory, so codes change on restart.

### Public server list

| Direction      | Frame |
|----------------|-------|
| host → relay   | `{"type":"listing","listing":{"title","description","version","mode","players","max_players","bedrock"}}` |
| host → relay   | `{"type":"listing"}` (withdraw) |
| relay → host   | `{"type":"listing_error","message":"..."}` |

Opted-in, live tunnels appear at `GET /api/servers` (CORS-open, cached 15 s).
Nothing is persisted. Text is cleaned (control characters and § codes
stripped, lengths capped) and offensive words refused. Operators can hide an
entry without a restart by adding its custom name, or the first 8+ hex
characters of its token ID from the logs, to `<-data-dir>/hidden.txt`.

### Supporters

Hosts the operator wants to thank (people who tipped) can be marked in the
dashboard ("Make supporter") or by adding their custom name, or the first 8+
hex characters of their token ID, to `<-data-dir>/supporters.txt` (same format
as `hidden.txt`; re-read every minute). A supporter gets: `"supporter": true`
on their `/api/servers` entry (the website shows a badge) and a place at the
top of that list; and their custom address is never released for disuse (free
ones expire after 60 days unused). Nothing else changes: no extra capacity, and
nothing is taken from free hosts. The operator API also has
`{"action":"grant_name","id":"<id>","name":"vp"}`, which gives a host a 1–2
character address (hosts can't claim those themselves; it replaces the address
they have now, and fails if the name is taken). This adds no wire-protocol
frames: existing apps are unaffected.

## Reading the log

Every tunnel writes two lines, enough to answer "why did my server go offline?":

```
[relay] tunnel up: token 7af8e6d0… -> public port 26042 client=desktop/1.7.0 route=direct back-after=2m52s
[usage] token=7af8e6d0 port=26042 uptime=5m36s conns=4 peak_players=1 in=134B out=480B ended=lost client=desktop/1.7.0 route=direct
```

- `client=` is the app and version the host reported in its register frame (`desktop/1.7.0`,
  `android/10`, `cli/…`). It is optional and checked against a strict pattern, so a client can
  never write free text into the log; anything else shows as `unknown`.
- `route=` is `direct` (the relay's own TLS listener) or `tunnel` (behind Cloudflare Tunnel or a proxy).
- `back-after=` says how long the host was gone when it came back within the hour.
  `replaced-a-dead-connection` means it reconnected while its old connection still looked alive:
  the host's network blipped and the relay had not noticed yet.
- `ended=` says why the tunnel ended:

| `ended=`    | Meaning |
|-------------|---------|
| `host-quit`   | the host's app closed the tunnel on purpose (stopped, quit, updating) |
| `lost`        | the connection died with no goodbye: network drop, sleep, router reset |
| `silent`      | nothing from the host for 90 seconds: a stalled or half-dead connection |
| `replaced`    | the same host registered again, so the old connection was replaced |
| `send-failed` | the relay could not write to the host's connection |
| `relay-stop`  | the relay itself was stopped or restarted |

No IP addresses are written to the log.

## Announcing to hosts

The operator can send every connected host a short notice (200 characters, plain text), for
example "The VoxelPort relay will restart." Do not put the time in the text: the apps add it
("in 10 min") from the `minutes` value and count it down. Apps that understand it show a banner and
repeat it in Minecraft chat; older apps ignore it. Hosts who connect after it was sent get it too, with the time
that is left, and `/api/status` shows it as `"notice":{"message":…,"seconds_left":…}`.

Send it from the dashboard ("Announce to hosts"), or on the operator API:

```sh
curl -H "Authorization: Bearer $VOXELPORT_ADMIN_TOKEN" -d '{"action":"notice","message":"The VoxelPort relay will restart.","minutes":10}' http://127.0.0.1:2528/action
curl -H "Authorization: Bearer $VOXELPORT_ADMIN_TOKEN" -d '{"action":"clear_notice"}' http://127.0.0.1:2528/action
```

A notice with a time expires 10 minutes after it; one without a time expires after 6 hours. When
the relay is stopped on purpose it tells every host "restarting" first.

## Compatibility

Every fix and limit added to this relay preserves this wire protocol exactly
— same frame types, same fields, same JSON shapes. **Existing published
app versions (and the discontinued mod) work unchanged against this relay**, with no update
required. The only externally-observable differences: connection IDs are
now 32 hex characters instead of 16 (clients only ever echo these back as
opaque strings, so length was never assumed), and abusive/stuck
connections get disconnected sooner than before. Protocol v2 (binary data frames) is opt-in per host: a host that
doesn't send `"bin":1` is served exactly as before.

## Security notes

- Token format validated (`vp_[A-Za-z0-9_-]{10,}`); identity is `sha256(token)`.
- Exactly one active tunnel per token, guaranteed even under concurrent
  registration attempts for the same token (see `regSerialize` in relay.go).
- Per-host `blocked_ips` drop unwanted player connections at accept time.
- Runs as a non-root user with `CAP_NET_BIND_SERVICE`; the systemd unit is hardened.
- Full trust model, threat model, and abuse model: [SECURITY.md](SECURITY.md).

## Deploy

See [DEPLOY.md](DEPLOY.md). Tests: `go test ./...` and `go test -race ./...`.

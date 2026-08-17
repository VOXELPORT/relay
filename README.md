# VoxelPort Relay

The server-side counterpart to the VoxelPort mod and desktop app. It:

1. Accepts secure WebSocket (`wss://`) connections from a host (the Fabric mod or
   the desktop app).
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
are generated locally on the mod/app and are not tied to an account — the
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
- It does **not** work with the stock mod/app out of the box: their tokens
  are generated locally with no prefix convention. To use this gate, you
  (the relay operator) must also configure every host you expect to connect
  — e.g. via the mod's `/voxel token <token>` command or `VOXELPORT_TOKEN`
  env var, or the app's token field — to use a token starting with your
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
./voxelport-relay -listen 127.0.0.1:2526 -min-port 25500 -max-port 25999
# serve wss:// directly (no Caddy):
./voxelport-relay -listen :443 -tls-cert fullchain.pem -tls-key privkey.pem
```

| Flag | Default | Purpose |
|---|---|---|
| `-listen` | `:8080` | panel + WebSocket listen address |
| `-min-port` / `-max-port` | 25500 / 25999 | public port range assignable to tunnels |
| `-tls-cert` / `-tls-key` | (unset) | serve `wss://` directly instead of behind a reverse proxy |
| `-token-secret` | (unset) | shared-prefix gate — see "Token gate" above |
| `-max-tunnels-per-ip` | 5 | max simultaneous active tunnels from one source IP |
| `-max-players-per-ip` | 16 | max simultaneous player connections from one source IP, per tunnel |
| `-trusted-proxy-cidrs` | (unset) | CIDRs allowed to supply `X-Forwarded-For`/`X-Real-IP`; loopback is always trusted |

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
of these require a mod/app update — see "Compatibility" below.

| Limit | Value | Purpose |
|---|---|---|
| Control-channel message size | 4 MB | bounds memory a single WebSocket frame can force the relay to allocate |
| Registration attempts | 10 per source IP per minute | blunts scripted registration of many fake tunnels |
| Active tunnels per source IP | 5 (`-max-tunnels-per-ip`) | stops one IP from consuming the whole port pool |
| Control-channel idle timeout | 90s (re-armed per message) | reclaims a tunnel whose host vanished without a clean TCP close |
| Players per tunnel | 256 concurrent | stops one attacker from flooding a single host with connections |
| Players per source IP, per tunnel | 16 (`-max-players-per-ip`) | stops one IP from occupying most of a tunnel's player slots |
| Player initial-inactivity timeout | 30s | drops a raw TCP connection that never sends anything (slot squatting) |
| Player idle timeout | 10 min (re-armed per read) | reaps a connection that's gone silent at the TCP level after being active |
| Player write queue | 128 messages, then disconnect | a slow/stuck player can't block the host's control channel or other players (see "Slow players" in SECURITY.md) |
| Sticky-port memory | 1 hour after disconnect | bounds memory used remembering "give this token its old port back" |
| Connection ID | 128 bits (`crypto/rand`) | not guessable/spoofable |

`X-Forwarded-For` and `X-Real-IP` are only trusted from loopback (the
documented Caddy/nginx-on-the-same-host deployments — different reverse
proxies default to different headers, so both are honored) or
`-trusted-proxy-cidrs` — a directly exposed relay can't have its rate
limits bypassed by a client spoofing either header.

## Wire protocol

JSON text frames (matches the mod's `ServerRelayService` and the app's tunnel client):

| Direction      | Frame |
|----------------|-------|
| host → relay   | `{"type":"register","token":"vp_…","blocked_ips":[...]}` |
| host → relay   | `{"type":"ping"}` |
| host → relay   | `{"type":"data","conn":id,"data":base64}` |
| host → relay   | `{"type":"close","conn":id}` |
| relay → host   | `{"type":"port","port":N}` |
| relay → host   | `{"type":"connect","conn":id,"ip":"1.2.3.4"}` |
| relay → host   | `{"type":"data","conn":id,"data":base64}` |
| relay → host   | `{"type":"close","conn":id}` |
| relay → host   | `{"type":"pong"}` |
| relay → host   | `{"type":"error","message":"..."}` |

TCP only (Java Edition). Bedrock/UDP is out of scope.

## Compatibility

Every fix and limit added to this relay preserves this wire protocol exactly
— same frame types, same fields, same JSON shapes. **Existing published
mod/app versions work unchanged against this relay**, with no update
required. The only externally-observable differences: connection IDs are
now 32 hex characters instead of 16 (clients only ever echo these back as
opaque strings, so length was never assumed), and abusive/stuck
connections get disconnected sooner than before.

## Security notes

- Token format validated (`vp_[A-Za-z0-9_-]{10,}`); identity is `sha256(token)`.
- Exactly one active tunnel per token, guaranteed even under concurrent
  registration attempts for the same token (see `regSerialize` in relay.go).
- Per-host `blocked_ips` drop unwanted player connections at accept time.
- Runs as a non-root user with `CAP_NET_BIND_SERVICE`; the systemd unit is hardened.
- Full trust model, threat model, and abuse model: [SECURITY.md](SECURITY.md).

## Deploy

See [DEPLOY.md](DEPLOY.md). Tests: `go test ./...` and `go test -race ./...`.

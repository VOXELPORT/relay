# VoxelPort Relay

The server-side counterpart to the VoxelPort mod and desktop app. It:

1. Accepts secure WebSocket (`wss://`) connections from a host (the Fabric mod or
   the desktop app).
2. Authenticates the host's auto-generated device token (`vp_…`) — **no Discord,
   no account, no database**. Any well-formed token is accepted; the token's
   SHA-256 is the host's stable identity.
3. Assigns each host a public TCP port (sticky per token while the relay is up).
4. Listens on that port and bridges raw bytes between vanilla Minecraft players
   and the host, which forwards them to the real local server.

```
Player ──TCP──▶ play.voxelport.in:25xxx ──┐
                                          │  (relay)
Host  ──wss──▶ relay.voxelport.in:443 ────┘──▶ 127.0.0.1:25565 (local server)
```

Stateless and pure-Go (no cgo, no DB) — the output is a single static binary.

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

Flags: `-listen`, `-min-port`, `-max-port`, `-tls-cert`, `-tls-key`.

## Endpoints

| Path          | Purpose                                             |
|---------------|-----------------------------------------------------|
| `/ws`         | host WebSocket control connection                   |
| `/api/status` | JSON: `{online, tunnels, forwards:[{port,players}]}`|
| `/`           | read-only HTML status panel                         |

## Limits

The relay enforces a few fixed limits to keep the intentionally accountless
`/ws` endpoint from being trivially abused:

| Limit | Value | Purpose |
|---|---|---|
| Control-channel message size | 4 MB | bounds memory a single WebSocket frame can force the relay to allocate |
| Registration attempts | 10 per source IP per minute | blunts scripted registration of many fake tunnels to exhaust the port pool |
| Control-channel idle timeout | 90s (re-armed on every message) | reclaims a tunnel whose host vanished without a clean TCP close |
| Players per tunnel | 256 concurrent | stops one attacker from flooding a single host with connections |

These apply regardless of `-token-secret`. None of them require a client
update — existing mod/app installs are unaffected.

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

## Security notes

- Token format validated (`vp_[A-Za-z0-9_-]{10,}`); identity is `sha256(token)`.
- One active tunnel per token — a new registration drops the old one.
- Per-host `blocked_ips` drop unwanted player connections at accept time.
- Runs as a non-root user with `CAP_NET_BIND_SERVICE`; the systemd unit is hardened.

## Deploy

See [DEPLOY.md](DEPLOY.md). Tests: `go test ./...`.

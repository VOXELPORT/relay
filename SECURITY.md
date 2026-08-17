# Security

This document describes what the VoxelPort relay actually protects against,
what it doesn't, and what its operator can see. It is written to be
accurate, not reassuring — if something below sounds like a limitation
rather than a feature, that's intentional.

## Security model

### What this protects against

- **No inbound port forwarding on the host's network.** The host opens a
  single outbound `wss://` connection; the relay is the only thing with a
  public listening port. This is the entire point of the project.
- **Casual discovery of other people's tunnels.** `/api/status` returns only
  aggregate counts, never a per-tunnel list of ports — see "Ports are not
  secret" below for what this does *not* mean.
- **Cross-tunnel interference.** One host's connection IDs have no authority
  in another host's tunnel; there is no global connection-ID namespace (see
  `relay.go`'s per-client `players` map and `TestCrossTunnelIsolation`).
- **Resource-exhaustion abuse of the relay itself**, within the limits
  documented in README.md's "Limits" section (registration rate, tunnels per
  IP, players per tunnel and per IP, message size, idle timeouts).
- **Token confidentiality in transit**, when the deployment is configured
  correctly for TLS (see "What TLS protects" below) — the default relay URL
  the mod/app use is `wss://`.

### What this does NOT protect against

- **A compromised or malicious relay operator.** The relay is a real
  in-the-loop proxy — see "What the relay operator can see" below. If you
  don't trust the operator of a given relay, don't use it; self-host your
  own (the relay is open source for exactly this reason).
- **A malicious Minecraft player.** Anything a normal Minecraft client could
  do to a server it's connected to, it can still do through the relay —
  the relay forwards bytes, it doesn't referee gameplay.
- **A stolen device token.** See "Authentication model" in README.md —
  possession of a token is sufficient to reconnect as that tunnel identity.
  There's no secondary factor.
- **End-to-end encryption of Minecraft traffic.** See "What TLS does not
  provide" below.
- **Arbitrary abuse of the relay as generic infrastructure**, if the
  operator hasn't configured `-token-secret` and doesn't otherwise rate-limit
  or monitor at the network level beyond what's built in. See "Arbitrary TCP
  forwarding" below.

## What the relay operator can see

Running a relay (yours or someone else's) means that relay process — and
whoever operates the machine it runs on — can observe:

- The host's real IP address (the direct peer of the `/ws` connection).
- Each player's real IP address (needed for `blocked_ips` and normal
  Minecraft server operation; forwarded to the host, not stored).
- The public port assigned to a tunnel.
- Connection timestamps and durations, and aggregate byte counts (used for
  the `[usage]` log line — see README.md).
- **The raw Minecraft protocol traffic passing through the tunnel.** The
  relay only base64-wraps bytes for JSON transport over the control
  WebSocket; it does not encrypt, and vanilla Minecraft's own protocol isn't
  encrypted either (outside of the login/auth handshake, which is Mojang's
  concern, not the relay's). A relay operator with the intent and access to
  inspect process memory or add logging could read gameplay packets. The
  stock relay code does not do this — but that's a code-review claim, not a
  cryptographic guarantee, because nothing here is end-to-end encrypted.

None of this is unusual for a proxy — it's the same category of trust you
place in any VPN, reverse proxy, or CDN — but it's stated explicitly here
because "no port forwarding" should not be misread as "the relay can't see
your traffic."

## What TLS protects

The host ↔ relay **control** WebSocket, when using `wss://` (the default),
is TLS-encrypted end to end between the host and the relay — this protects
the device token and control-channel metadata from anyone observing the
network path between the host and the relay (e.g. the host's own ISP, or a
network attacker between them).

## What TLS does NOT provide

**Player ↔ relay traffic is raw TCP, not TLS**, matching how vanilla
Minecraft normally connects to a server directly (Minecraft's Java Edition
protocol is not TLS-wrapped). The relay does not add encryption to this leg
either. So:

- TLS on the control channel protects the *host's* connection to the relay.
  It does **not** provide end-to-end encryption between a *player* and the
  *host* — the relay itself is always an unencrypted midpoint for gameplay
  traffic, by design (see "What the relay operator can see").
- This matches what would happen if the host ran a public Minecraft server
  directly with no relay at all — Minecraft traffic has never been
  end-to-end encrypted between a vanilla client and a vanilla server. The
  relay does not make this better or worse; it's stated here so nobody
  assumes `wss://` on the control channel implies anything about the game
  traffic itself.

## Abuse model

The relay exposes a real public TCP port per active tunnel. Treat that port
the same way you'd treat any Minecraft server's port exposed directly to the
internet:

- **Public scanning.** Internet-wide scanners will find and probe open
  ports, including relay-assigned ones, same as any other public TCP port.
- **Malicious Minecraft clients.** A player connecting through the relay is
  indistinguishable from one connecting directly — normal Minecraft-server
  hardening (whitelist, anti-cheat, `blocked_ips`) is the host's
  responsibility, not the relay's.
- **TCP connection floods.** Bounded per-tunnel and per-IP by the limits in
  README.md, but a sufficiently large distributed flood is still a
  real-world DoS risk against any given tunnel's assigned port, same as it
  would be against a directly-exposed server.
- **Automated probing / bots.** Same exposure as any public Minecraft
  server address.
- **Relay infrastructure abuse.** Covered by the per-IP tunnel/registration
  limits — see README.md.

### Ports are not secret

`/api/status` deliberately does not list individual tunnels' ports, to
reduce *casual* discovery — someone browsing the status page can't build a
directory of every live server. This is **not** a security boundary: the
assigned port is a normal public TCP port in a known, fairly small range
(`-min-port`..`-max-port`, 500 ports by default), and is trivially
discoverable by scanning that range or by simply being told the address by
a player who already has it (which is the whole point — that's how people
join). Treat an assigned port as public information, not a secret.

### Arbitrary TCP forwarding

The relay forwards whatever bytes a registered host sends, without
validating that they form a valid Minecraft handshake. This means a custom
(non-VoxelPort) client could, in principle, use a registered tunnel to
proxy non-Minecraft TCP traffic through the relay.

This was evaluated and deliberately **not** filtered in this pass: Minecraft's
handshake (a varint-length-prefixed packet containing protocol version,
server address, and next-state) could be validated, but doing so robustly
across every supported Minecraft/Fabric/Paper version without misclassifying
legitimate traffic or adding a fragile, half-correct parser was judged higher
risk than the abuse it would prevent. Rather than ship a protection that
might silently break real players on some client version, this is documented
here as a known, accepted risk: **the relay is not currently a
Minecraft-only forwarder**, it is a generic authenticated TCP relay whose
intended use is Minecraft. If this becomes a real operational problem,
revisit with actual traffic samples across supported versions before
attempting handshake validation.

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

<!-- TODO(operator): replace with a real, monitored contact — e.g. a
     security@voxelport.in address or a private GitHub Security Advisory.
     This is a placeholder; nothing currently monitors it. -->
Report vulnerabilities privately to: **security@voxelport.in** (placeholder —
configure a real, monitored address or enable GitHub's private security
advisories for this repository before relying on this contact).

Please include: the affected component (relay/mod/app), reproduction steps,
and the realistic impact you believe it has. We'll acknowledge receipt and
aim to have a fix or mitigation plan before any public disclosure.

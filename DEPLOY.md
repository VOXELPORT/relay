# Going live — VoxelPort relay (Ubuntu x86_64, Caddy)

Assumes DNS for `relay.voxelport.in` **and** `play.voxelport.in` already points at
the VPS IP. `VPS` = run on the server over SSH; `LOCAL` = run on your machine.

## 1. Build & upload the relay binary  (LOCAL)

```bash
cd relay
GOOS=linux GOARCH=amd64 go build -o voxelport-relay .
scp voxelport-relay YOUR_VPS:/tmp/voxelport-relay
```

## 2. Install the relay  (VPS)

```bash
sudo useradd --system --no-create-home voxelport || true
sudo mkdir -p /opt/voxelport
sudo mv /tmp/voxelport-relay /opt/voxelport/voxelport-relay
sudo chmod +x /opt/voxelport/voxelport-relay
```

## 3. TLS via Caddy  (VPS)

Install Caddy, then use [deploy/Caddyfile](deploy/Caddyfile) — it fetches a
Let's Encrypt cert for `relay.voxelport.in` and proxies to the relay's local
`127.0.0.1:2526`.

```bash
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

(Alternatively skip Caddy and run the relay with `-tls-cert`/`-tls-key` on `:443`.)

## 4. Firewall  (VPS)

```bash
sudo ufw allow 443/tcp            # wss:// control connection (via Caddy)
sudo ufw allow 25500:25999/tcp    # public Minecraft player ports
```

## 5. Install the service  (VPS)

```bash
sudo cp deploy/voxelport-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now voxelport-relay
sudo systemctl status voxelport-relay
```

The unit runs `voxelport-relay -listen 127.0.0.1:2526 -min-port 25500 -max-port 25999`.
Edit the `ExecStart` line to change the port range.

## 6. Verify

```bash
curl -s https://relay.voxelport.in/api/status
# {"forwards":[],"online":true,"tunnels":0}
```

Then start a host (mod or desktop app) — it should print a
`play.voxelport.in:<port>` address, and `/api/status` should list the tunnel.

## Notes

- **No Discord, no accounts, no database.** Hosts identify with an
  auto-generated `vp_…` device token created on first run — an anonymous
  bearer identity, not conventional account authentication. See README.md's
  "Authentication model" for exactly what this does and doesn't guarantee.
- `play.voxelport.in` and `relay.voxelport.in` should both resolve to this VPS.
  Players join `play.voxelport.in:<assigned-port>`; hosts connect to
  `wss://relay.voxelport.in`.
- The service stops via `systemctl stop` (SIGTERM) cleanly and quickly — it
  drains active tunnels itself before exiting, no forced kill needed under
  normal conditions.

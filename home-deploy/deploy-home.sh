#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# VoxelPort home relay — one-shot installer for the Ubuntu mini PC.
#
# RUN IT LIKE:   sudo bash deploy-home.sh
#
# DO THESE FIRST (the TLS cert step will fail otherwise):
#   1. DNS: point  relay.voxelport.in  AND  play.voxelport.in  A records
#           at your home STATIC public IP.
#   2. Router: forward  TCP 80, 443, 25500-25999  ->  this machine's LAN IP.
#   3. Keep this script and the 'voxelport-relay' binary in the SAME folder.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

DOMAIN="relay.voxelport.in"
MINPORT=25500
MAXPORT=25999
HERE="$(cd "$(dirname "$0")" && pwd)"
BIN_SRC="$HERE/voxelport-relay"

if [[ $EUID -ne 0 ]]; then echo "Please run with sudo:  sudo bash deploy-home.sh"; exit 1; fi
if [[ ! -f "$BIN_SRC" ]]; then echo "ERROR: 'voxelport-relay' binary not found next to this script."; exit 1; fi

echo "==> [1/6] Service user + install dir"
id voxelport &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin voxelport
mkdir -p /opt/voxelport
install -m 0755 -o voxelport -g voxelport "$BIN_SRC" /opt/voxelport/voxelport-relay

echo "==> [2/6] systemd service"
cat > /etc/systemd/system/voxelport-relay.service <<EOF
[Unit]
Description=VoxelPort Relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=voxelport
Group=voxelport
WorkingDirectory=/opt/voxelport
ExecStart=/opt/voxelport/voxelport-relay -listen 127.0.0.1:2526 -min-port ${MINPORT} -max-port ${MAXPORT}
Restart=on-failure
RestartSec=3
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

echo "==> [3/6] Caddy (TLS) — installing if missing"
export DEBIAN_FRONTEND=noninteractive
if ! command -v caddy &>/dev/null; then
  apt-get install -y -q debian-keyring debian-archive-keyring apt-transport-https curl gnupg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key'      | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -q
  apt-get install -y -q caddy
fi

echo "==> [4/6] Caddyfile for ${DOMAIN}"
cat > /etc/caddy/Caddyfile <<EOF
${DOMAIN} {
	reverse_proxy 127.0.0.1:2526 {
		transport http {
			read_timeout  0
			write_timeout 0
			dial_timeout  10s
		}
		flush_interval -1
	}
}
EOF

echo "==> [5/6] Firewall (ufw)"
if command -v ufw &>/dev/null; then
  ufw allow 22/tcp                >/dev/null 2>&1 || true   # LAN SSH (not forwarded to internet)
  ufw allow 80/tcp                >/dev/null 2>&1 || true
  ufw allow 443/tcp               >/dev/null 2>&1 || true
  ufw allow ${MINPORT}:${MAXPORT}/tcp >/dev/null 2>&1 || true
  ufw --force enable              >/dev/null 2>&1 || true
fi

echo "==> [6/6] Starting services"
systemctl daemon-reload
systemctl enable --now voxelport-relay
systemctl restart caddy 2>/dev/null || systemctl reload caddy 2>/dev/null || true

sleep 2
echo
echo "Local relay check (should be JSON):"
curl -s http://127.0.0.1:2526/api/status || echo "  (relay not responding locally — check: systemctl status voxelport-relay)"
echo
echo "─────────────────────────────────────────────────────────────"
echo "Installed. Caddy needs ~30-60s to fetch the TLS certificate."
echo "Then, from ANY device, confirm it's public:"
echo "    curl https://${DOMAIN}/api/status"
echo
echo "Useful commands:"
echo "    systemctl status voxelport-relay      # relay health"
echo "    journalctl -u voxelport-relay -f       # live logs (incl. [usage] lines)"
echo "    systemctl status caddy                 # TLS / cert status"
echo "─────────────────────────────────────────────────────────────"

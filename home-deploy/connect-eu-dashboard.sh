#!/usr/bin/env bash
# Connects the home dashboard (voxelport-admin) to the Europe relay.
# Run ON THE HOME SERVER:   sudo bash connect-eu-dashboard.sh
#
# Needs, in /tmp:  voxelport-admin-new   (the new dashboard binary, linux amd64)
# It asks you to paste the Europe relay's operator token (not echoed).
#
# It changes: /etc/voxelport/admin.env (+1 line), /etc/systemd/system/voxelport-admin.service (flags added,
# backup .prev), /opt/voxelport-admin/voxelport-admin (backup .prev), and adds voxelport-eu-tunnel.service.
# Rollback is at the bottom of the output.
set -euo pipefail
[ "$EUID" -eq 0 ] || { echo "Run with sudo."; exit 1; }

EU_IP=95.216.212.149
EXPECT_FP='SHA256:RIMwhd8AoCDy4fvHQEj3Zb6jiL7x+ZM+qp88+rfqqOA'
NEWBIN=/tmp/voxelport-admin-new
[ -f "$NEWBIN" ] || { echo "Missing $NEWBIN (copy the new dashboard binary there first)."; exit 1; }

echo "==> 1/6 The Europe server's SSH identity"
KH=$(mktemp)
ssh-keyscan -t ed25519 "$EU_IP" 2>/dev/null > "$KH"
FP=$(ssh-keygen -lf "$KH" | awk '{print $2}')
echo "    fingerprint: $FP"
[ "$FP" = "$EXPECT_FP" ] || { echo "    DOES NOT MATCH the expected $EXPECT_FP : stopping."; rm -f "$KH"; exit 1; }
install -d -m 0700 -o trazhub -g trazhub /home/trazhub/.ssh
grep -q "$EU_IP" /home/trazhub/.ssh/known_hosts 2>/dev/null || { cat "$KH" >> /home/trazhub/.ssh/known_hosts; chown trazhub:trazhub /home/trazhub/.ssh/known_hosts; }
rm -f "$KH"

echo "==> 2/6 The Europe relay's token"
read -r -s -p "    Paste the Europe token and press Enter: " EUTOK; echo
[ "${#EUTOK}" -ge 24 ] || { echo "    That looks too short."; exit 1; }
if grep -q '^VOXELPORT_ADMIN_TOKEN2=' /etc/voxelport/admin.env; then
  sed -i "s|^VOXELPORT_ADMIN_TOKEN2=.*|VOXELPORT_ADMIN_TOKEN2=${EUTOK}|" /etc/voxelport/admin.env
else
  printf 'VOXELPORT_ADMIN_TOKEN2=%s\n' "$EUTOK" >> /etc/voxelport/admin.env
fi
chmod 600 /etc/voxelport/admin.env

echo "==> 3/6 The tunnel to the Europe relay's operator API (127.0.0.1:2531)"
cat > /etc/systemd/system/voxelport-eu-tunnel.service <<'UNIT'
[Unit]
Description=SSH tunnel to the VoxelPort Europe relay operator API (loopback 2531)
After=network-online.target
Wants=network-online.target

[Service]
User=trazhub
ExecStart=/usr/bin/ssh -N -L 127.0.0.1:2531:127.0.0.1:2528 -i /home/trazhub/.ssh/id_ed25519 -o IdentitiesOnly=yes -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=20 -o ServerAliveCountMax=3 -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/home/trazhub/.ssh/known_hosts root@95.216.212.149
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now voxelport-eu-tunnel
sleep 4
echo "    tunnel: $(systemctl is-active voxelport-eu-tunnel)"
echo -n "    Europe relay through the tunnel: "
curl -s -m 8 -H "Authorization: Bearer ${EUTOK}" http://127.0.0.1:2531/state | head -c 120; echo
curl -s -m 8 -H "Authorization: Bearer ${EUTOK}" http://127.0.0.1:2531/state | grep -q '"public_host"' || { echo "    The Europe API did not answer: stopping before touching the dashboard."; exit 1; }

echo "==> 4/6 Backups"
cp -a /opt/voxelport-admin/voxelport-admin /opt/voxelport-admin/voxelport-admin.prev
cp -a /etc/systemd/system/voxelport-admin.service /etc/systemd/system/voxelport-admin.service.prev

echo "==> 5/6 New dashboard + Europe flags"
install -m 0755 "$NEWBIN" /opt/voxelport-admin/voxelport-admin
FLAGS='-relay2 http://127.0.0.1:2531 -region2-id eu -region2-label Europe -region2-short EU "-region2-provider=Europe VPS" -region2-egress-free-gb 0 -region2-since 2026-10-19'
if ! grep -q -- '-relay2' /etc/systemd/system/voxelport-admin.service; then
  sed -i "s|^\(ExecStart=.*-tz Asia/Kolkata\)\$|\1 ${FLAGS}|" /etc/systemd/system/voxelport-admin.service
fi
sed -i 's|voxelport-na-tunnel.service|voxelport-eu-tunnel.service|' /etc/systemd/system/voxelport-admin.service
grep -c -- '-relay2' /etc/systemd/system/voxelport-admin.service | sed 's/^/    -relay2 lines in the unit: /'
systemctl daemon-reload
systemctl restart voxelport-admin
sleep 3

echo "==> 6/6 Check"
echo "    dashboard: $(systemctl is-active voxelport-admin)"
journalctl -u voxelport-admin -n 4 --no-pager | cut -c1-160 | sed 's/^/    /'
cat <<'DONE'

Done. Open the dashboard (Tailscale): you should see "EU relay" in the header and a "Europe server" card.

ROLLBACK (if anything looks wrong):
  sudo cp /opt/voxelport-admin/voxelport-admin.prev /opt/voxelport-admin/voxelport-admin
  sudo cp /etc/systemd/system/voxelport-admin.service.prev /etc/systemd/system/voxelport-admin.service
  sudo systemctl daemon-reload && sudo systemctl restart voxelport-admin
  sudo systemctl disable --now voxelport-eu-tunnel
DONE

VoxelPort — home relay setup (self-contained, no SSH exposed)
=============================================================

This folder has two files:
  - voxelport-relay     the Linux relay binary (metering build, matches production)
  - deploy-home.sh      one-shot installer

You run this ON the Ubuntu mini PC. Nothing needs to be exposed to the internet
except the relay ports below.

────────────────────────────────────────────────────────────────────────
DO THESE FIRST (before running the script)
────────────────────────────────────────────────────────────────────────
1. Install Ubuntu Server 24.04 LTS on the mini PC. Give it a STATIC LAN IP
   (a DHCP reservation on your router, e.g. 192.168.1.50).

2. DNS — point these A records at your home STATIC public IP:
       relay.voxelport.in   ->  <your static IP>
       play.voxelport.in    ->  <your static IP>

3. Router — forward these to the mini PC's LAN IP (all TCP):
       80, 443, 25500-25999
   (Do NOT forward 22 — you manage over LAN.)

────────────────────────────────────────────────────────────────────────
COPY THIS FOLDER TO THE MINI PC (over your LAN)
────────────────────────────────────────────────────────────────────────
Option 1 — USB stick: copy both files onto it, plug into the mini PC.

Option 2 — scp from this Windows PC (Git Bash / PowerShell), replacing the
IP with the mini PC's LAN IP and 'ubuntu' with your username:
       scp voxelport-relay deploy-home.sh ubuntu@192.168.1.50:~/

────────────────────────────────────────────────────────────────────────
RUN IT (on the mini PC)
────────────────────────────────────────────────────────────────────────
       cd ~            # (or wherever you copied the files)
       sudo bash deploy-home.sh

It installs the relay + Caddy (TLS) + systemd service + firewall, and starts
everything. Wait ~30-60s for the certificate, then from any device:

       curl https://relay.voxelport.in/api/status
       -> {"online":true,"tunnels":0,"players":0,...}

That's it — your home relay is live. Tell me once it responds and I'll run the
full player join-test against it and help you cut DNS over from the VPS.

Keep the Vultr VPS alive as a backup: if home ever goes down, flip the two A
records back to 139.84.164.87 and everyone reconnects.

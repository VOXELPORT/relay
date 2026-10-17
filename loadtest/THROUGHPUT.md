# Finding the relay's real throughput ceiling

How many players the relay can carry is decided by two things: the **CPU** (every byte is read, framed, written) and the
**link** (the machine's network connection). These recipes tell you which one runs out first, on the home mini PC, in
protocol v1 (base64 in JSON) and v2 (binary frames).

`loadgen`'s throughput mode drives it: every player sends a steady number of bytes per second in timestamped frames, the
fake hosts echo them back, and every 10 seconds it prints how fast data actually moved each way, how long the echo took
(p50 and p99), how many connections were cut off, and the relay's own CPU and memory.

> **Run these against a throwaway relay on the mini PC, as below, not against the live one.** Real players share the
> machine's CPU and network with the test, and a heavy run can make their game lag. Do it when nobody is playing.

## Set up (on the mini PC, once)

From a checkout of this repo on any machine with Go (or copy the two binaries):

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o relay .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o loadgen ./loadtest/loadgen
scp relay loadgen you@mini-pc:~/capacity-test/
```

Start a test relay of its own. It listens on loopback only, uses ports nothing else uses, and has no custom addresses:

```bash
cd ~/capacity-test
./relay -listen 127.0.0.1:2599 -min-port 28100 -max-port 28900 &
echo $! > relay.pid
```

Make sure the real relay is not using those ports (`ss -ltn | grep ':281'` shows nothing), and stop the test relay at the end
(`kill $(cat relay.pid)`).

## Recipe 1: 100 players × 100 KB/s (10 MB/s each way, 80 Mbps)

```bash
cd ~/capacity-test
for mode in json bin; do
  flag=""; [ $mode = bin ] && flag="-bin"
  ./loadgen -relay-ws ws://127.0.0.1:2599/ws -player-host 127.0.0.1 \
    -hosts 10 -players 10 -bytes-per-sec 100000 -ramp 30s -duration 5m \
    -relay-pid $(cat relay.pid) $flag 2>&1 | tee recipe1-$mode.log
done
```

## Recipe 2: 200 players × 150 KB/s (30 MB/s each way, 240 Mbps)

```bash
cd ~/capacity-test
for mode in json bin; do
  flag=""; [ $mode = bin ] && flag="-bin"
  ./loadgen -relay-ws ws://127.0.0.1:2599/ws -player-host 127.0.0.1 \
    -hosts 10 -players 20 -bytes-per-sec 150000 -ramp 60s -duration 5m \
    -relay-pid $(cat relay.pid) $flag 2>&1 | tee recipe2-$mode.log
done
```

(Ten hosts from one machine is inside the per-address limits: 25 tunnels per IPv4 address, 32 players per address per
tunnel, 30 registrations a minute. Raise `-hosts` only up to those limits, or run the relay with higher ones.)

## What to record

For each of the four runs (recipe × mode), from the last few `[progress]` lines and the `summary` line:

| Number | Where | Why |
|---|---|---|
| achieved up / down Mbps | `up … down …` | compare with the target (80 or 240 Mbps each way) |
| echo p50 / p99 | `echo p50 … p99 …` | p99 is what a player feels as lag; it climbs before anything breaks |
| disconnects | `disconnects=` | must stay 0; any means players were dropped |
| relay CPU % | `relay cpu …` | 100% is one core fully used; the relay uses several |
| relay memory | `rss` | should be flat, not growing through the run |
| machine totals | `top` / `htop` while it runs | the load generator uses CPU too |

## Reading the result

Look at whether the run **kept up** (achieved Mbps close to the target, p99 steady), then at the CPU:

- **Kept up, p99 low and steady:** the machine carries this load with room to spare. Run a bigger recipe to find the edge.
- **Fell short of the target (or p99 climbs run after run), relay CPU high** (several hundred percent on this
  16-thread machine, or the relay at about one full core per tunnel, since each host's tunnel is read by one goroutine):
  **CPU is the ceiling.** Compare the `json` and `bin` runs: binary frames cost far less CPU per byte, so this is where
  protocol v2 earns its keep.
- **Fell short, relay CPU low or moderate, the whole machine busy** (`top` shows `loadgen` high): the generator is the
  limit, not the relay. Run it with fewer players per process, or from a second machine.
- **Fell short with everything idle:** something is throttling the connections (a firewall rule, a per-address limit; look for
  "refused" lines in the relay's log).

**This test never touches the network card** (everything is on loopback), so it finds the CPU ceiling only. The **link**
ceiling you work out from the line speed: each byte a player sends crosses the relay's connection twice (in from the player,
out to the host) and again on the way back, so the uplink and downlink each carry about **the total player traffic**. If your
connection gives 300 Mbps down and 50 Mbps up, the 50 is the number that decides how many players fit, long before the CPU
does. A speed test from the mini PC gives the real figures; divide the smaller by the per-player rate (a busy Minecraft player
is roughly 20–100 KB/s down and a tenth of that up).

The relay's real ceiling is the lower of the two.

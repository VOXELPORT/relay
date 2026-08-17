# VoxelPort Live Relay Soak Test

This folder contains a Windows build of the live relay soak tester:

- `live-soak.exe`
- `run-live-soak.bat`
- `run-live-soak.ps1`

The tester acts like one plugin connection, receives the relay-assigned public
port, then opens many real TCP player connections to `play.voxelport.in:<port>`.
Each player sends bytes and verifies the echo path end to end.

## Run

Create a temporary relay token first. Then either double-click
`run-live-soak.bat` and paste the token, or run:

```powershell
$env:LIVE_RELAY_TOKEN = "vp_your_temp_token_here"
.\run-live-soak.ps1
Remove-Item Env:LIVE_RELAY_TOKEN
```

You can also run the exe directly:

```powershell
.\live-soak.exe -token "vp_your_temp_token_here"
```

The script defaults to:

- 150 players
- 3 hours
- 1024 bytes per player every second
- 2 minute connect ramp

Do not use a real user's token. There is no relay database or admin API —
the relay is accountless and stateless (see the main README's "Compatibility
& trust model" section). A "temporary token" is just any `vp_`-shaped string
you make up for the test; there's nothing to revoke or delete afterward. Its
only persistent trace is the relay's in-memory sticky-port mapping, which is
forgotten automatically once the token has been disconnected for longer than
the sticky-port TTL (see relay.go's `stickyPortTTL`).

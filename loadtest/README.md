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

Do not use a real user's token. After the test, revoke or delete the temporary
token from the relay database/admin API.

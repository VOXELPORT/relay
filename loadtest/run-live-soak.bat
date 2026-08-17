@echo off
setlocal
cd /d "%~dp0"

echo VoxelPort Live Relay Soak Test
echo.
if "%LIVE_RELAY_TOKEN%"=="" (
  set /p LIVE_RELAY_TOKEN=Paste temporary relay token:
)

echo.
echo Starting test. Press Ctrl+C to stop early.
echo.
live-soak.exe -ws wss://play.voxelport.in/ -player-host play.voxelport.in -players 1000 -duration 3h -payload-bytes 1024 -interval 1s -connect-ramp 2m -progress-every 30s

echo.
echo Test finished with exit code %ERRORLEVEL%.
echo Remember to delete/revoke the temporary relay token.
pause

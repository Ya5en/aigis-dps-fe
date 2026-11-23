@echo off
rem build.bat: VERSION -> versioninfo.json -> syso -> web dist -> single exe
rem Version single source of truth is the VERSION file (SemVer; see README).
setlocal
cd /d "%~dp0"

set VER=
set /p VER=<VERSION
if "%VER%"=="" (
  echo [!] VERSION is empty
  exit /b 1
)

go run ./tools/genver || exit /b 1
go tool goversioninfo -64 -o rsrc_windows_amd64.syso versioninfo.json || exit /b 1

cd /d "%~dp0web" || exit /b 1
call npm run build || exit /b 1
cd /d "%~dp0"
go build -ldflags "-H windowsgui -X main.version=%VER%" -o dpsmeter.exe . || exit /b 1
echo BUILD OK: dpsmeter.exe (v%VER%)

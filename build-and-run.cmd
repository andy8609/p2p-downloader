@echo off
rem p2pdl one-click build + run (double-click this file).
rem Passes all arguments to build-and-run.ps1, e.g.:
rem   build-and-run.cmd -Test
rem   build-and-run.cmd -Target gui
setlocal
rem Match the UTF-8 console output used by the .ps1 script (avoids mojibake).
chcp 65001 >nul

set "PS=powershell.exe"
where pwsh.exe >nul 2>nul && set "PS=pwsh.exe"

"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%~dp0build-and-run.ps1" %*
set "RC=%ERRORLEVEL%"

echo.
if not "%RC%"=="0" (
  echo [FAILED] exit code %RC%
) else (
  echo [DONE] build finished, GUI process started in background.
)
echo Press any key to close this window...
pause >nul
exit /b %RC%

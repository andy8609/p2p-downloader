# p2pdl one-click build + run
#
# What it does: calls build.ps1 to produce binaries in dist/, verifies that all
# expected .exe files exist, then launches the desktop GUI.
# Double-click build-and-run.cmd to use it, or run this script from PowerShell.
#
# Usage:
#   .\build-and-run.ps1                 # build all targets (dl / dlengine / gui) and launch the GUI
#   .\build-and-run.ps1 -Test           # run go vet + all unit tests before building
#   .\build-and-run.ps1 -Target gui     # build the GUI only
#   .\build-and-run.ps1 -NoRun          # build only, do not launch anything
#
# Notes:
#   - build.ps1 downloads a private Go toolchain into .tools/ when missing (~79MB, first run only).
#   - The first build also fetches Go module dependencies via goproxy.cn and may take a few minutes.
#   - The GUI requires the Microsoft Edge WebView2 Runtime (bundled with Windows 11 and recent
#     Windows 10; the GUI offers a download link when it is missing).
#
# IMPORTANT: keep this file ASCII-only.
# Windows PowerShell 5.1 decodes BOM-less script files with the ANSI code page (e.g. CP936),
# which corrupts non-ASCII string literals and breaks parsing. English messages avoid that
# entirely. Chinese documentation for the project lives in README.md and docs/.

[CmdletBinding()]
param(
    [ValidateSet('all', 'dlengine', 'dl', 'gui')]
    [string]$Target = 'all',

    [switch]$Test,

    [switch]$NoRun
)

$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$Root = $PSScriptRoot
$BuildScript = Join-Path $Root 'build.ps1'
$DistDir = Join-Path $Root 'dist'

function Write-Step([string]$Text) {
    Write-Host ''
    Write-Host "==> $Text" -ForegroundColor Cyan
}

Write-Host '=================================================='
Write-Host '  p2pdl - build and run'
Write-Host ('  project: {0}' -f $Root)
Write-Host '=================================================='

if (-not (Test-Path -LiteralPath $BuildScript)) {
    Write-Host ('[FAILED] build script not found: {0}' -f $BuildScript) -ForegroundColor Red
    exit 1
}

try {
    # ------------------------------------------------------------------
    # 1. Build
    # ------------------------------------------------------------------
    if ($Test) { $modeText = 'with tests' } else { $modeText = 'tests skipped' }
    Write-Step ('building (Target={0}, {1})' -f $Target, $modeText)

    Push-Location -LiteralPath $Root
    try {
        if ($Test) {
            & $BuildScript -Target $Target -Test
        }
        else {
            & $BuildScript -Target $Target
        }
        if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) {
            throw ('build.ps1 returned exit code {0}' -f $LASTEXITCODE)
        }
    }
    finally {
        Pop-Location
    }

    # ------------------------------------------------------------------
    # 2. Verify artifacts
    # ------------------------------------------------------------------
    $expected = switch ($Target) {
        'all' { @('dl.exe', 'dlengine.exe', 'p2pdl-gui.exe') }
        'gui' { @('p2pdl-gui.exe') }
        'dl' { @('dl.exe') }
        'dlengine' { @('dlengine.exe') }
    }

    Write-Step 'checking build artifacts'
    $missing = @()
    foreach ($name in $expected) {
        $path = Join-Path $DistDir $name
        if (Test-Path -LiteralPath $path) {
            $size = (Get-Item -LiteralPath $path).Length
            Write-Host ('  [OK]      {0,-16} {1,12:N0} bytes' -f $name, $size) -ForegroundColor Green
        }
        else {
            $missing += $name
            Write-Host ('  [MISSING] {0}' -f $name) -ForegroundColor Red
        }
    }
    if ($missing.Count -gt 0) {
        throw ('missing artifacts: {0}' -f ($missing -join ', '))
    }

    # ------------------------------------------------------------------
    # 3. Run
    # ------------------------------------------------------------------
    if ($NoRun) {
        Write-Step 'skipping launch (-NoRun)'
        exit 0
    }

    $gui = Join-Path $DistDir 'p2pdl-gui.exe'
    if (Test-Path -LiteralPath $gui) {
        Write-Step 'launching the desktop GUI (p2pdl-gui.exe)'
        Write-Host '  The engine (dlengine.exe) is started automatically from the same folder.'
        Write-Host '  Default download folder: %USERPROFILE%\Downloads\p2pdl'
        Start-Process -FilePath $gui -WorkingDirectory $DistDir
        Write-Host ''
        Write-Host '[DONE] Build finished and the GUI has been started.' -ForegroundColor Green
    }
    else {
        Write-Step 'no GUI in this target, showing CLI help instead'
        & (Join-Path $DistDir 'dl.exe') help
    }
}
catch {
    Write-Host ''
    Write-Host ('[FAILED] {0}' -f $_.Exception.Message) -ForegroundColor Red
    exit 1
}

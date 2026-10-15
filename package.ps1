# p2pdl Windows 打包脚本
#
# 产物（全部落在 dist/）：
#   p2pdl-<版本>-win-x64.zip            免安装包：解压后双击 p2pdl-gui.exe 即可用
#   p2pdl-<版本>-win-x64.zip.sha256     校验和
#   install.ps1                         可选安装脚本（复制到用户目录 + 开始菜单快捷方式）
#   uninstall.ps1                       卸载脚本
#
# 设计取舍：
#   - 「引擎独立进程」是架构基石（界面崩溃不中断下载），因此免安装包内是
#     p2pdl-gui.exe + p2pdl-engine.exe 两个可执行文件，而不是强行塞进单个 exe。
#   - 不做自解压/静默安装：脚本化的 install.ps1 可读、可审、可复现，
#     不引入 Inno Setup/NSIS 等额外工具链。
#
# 用法：
#   .\package.ps1                 # 构建并打包
#   .\package.ps1 -Version 1.2.3  # 指定版本号
#   .\package.ps1 -SkipTests      # 跳过测试（仅用于快速出包）

[CmdletBinding()]
param(
    [string]$Version = '',
    [switch]$SkipTests,
    [switch]$Clean
)

$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$Root = $PSScriptRoot
$DistDir = Join-Path $Root 'dist'
$BuildDir = Join-Path $DistDir 'package'

if ($Clean) {
    foreach ($p in @($BuildDir, (Join-Path $DistDir 'package.tmp'))) {
        if (Test-Path $p) { Remove-Item $p -Recurse -Force }
    }
}

# ---------------------------------------------------------------------------
# 工具定位（与 build.ps1 保持一致的隔离策略）
# ---------------------------------------------------------------------------
$localGo = Join-Path $Root '.tools\go\bin\go.exe'
if (Test-Path $localGo) {
    $GoExe = $localGo
} elseif (Get-Command go -ErrorAction SilentlyContinue) {
    $GoExe = (Get-Command go).Source
} else {
    throw '未找到 Go 工具链，请先执行 build.ps1 完成初始化'
}
$GoRoot = Split-Path (Split-Path $GoExe -Parent) -Parent

$env:GOROOT = $GoRoot
$env:GOPATH = Join-Path $Root '.tools\gopath'
$env:GOCACHE = Join-Path $Root '.tools\gocache'
$env:GOTMPDIR = Join-Path $Root '.tools\tmp'
if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }
$env:GOFLAGS = '-mod=mod'
New-Item -ItemType Directory -Force -Path $env:GOPATH, $env:GOCACHE, $env:GOTMPDIR, $BuildDir | Out-Null

# ---------------------------------------------------------------------------
# 版本号
# ---------------------------------------------------------------------------
$gitExe = (Get-Command git -ErrorAction SilentlyContinue).Source
function Get-GitValue([string[]]$Args, [string]$Default) {
    if (-not $gitExe) { return $Default }
    try {
        $out = & $gitExe -C $Root @Args 2>$null
        if ($LASTEXITCODE -eq 0 -and $out) { return ($out | Select-Object -First 1).ToString().Trim() }
    } catch { }
    return $Default
}
if (-not $Version) { $Version = Get-GitValue @('describe', '--tags', '--always') '1.0.0' }

# Windows 的 VERSIONINFO 需要四段版本号（a.b.c.d）；非数字后缀（如 -dirty）会被剥离。
$ver4 = $Version -replace '^v', ''
if ($ver4 -notmatch '^\d+\.\d+\.\d+\.\d+$') {
    $nums = @()
    foreach ($part in ($ver4 -split '[.\-+]')) {
        if ($part -match '^\d+$') { $nums += $part } else { break }
    }
    while ($nums.Count -lt 4) { $nums += '0' }
    $ver4 = ($nums[0..3] -join '.')
}
$Commit = Get-GitValue @('rev-parse', '--short', 'HEAD') 'unknown'
$BuildTime = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

Write-Host "打包 p2pdl $Version (commit $Commit)"

$ldflags = @(
    '-s', '-w',
    "-X 'p2pdl/internal/version.Version=$Version'",
    "-X 'p2pdl/internal/version.Commit=$Commit'",
    "-X 'p2pdl/internal/version.BuildTime=$BuildTime'"
) -join ' '

# ---------------------------------------------------------------------------
# 0. 质量门禁
# ---------------------------------------------------------------------------
if (-not $SkipTests) {
    Write-Host '--- 运行测试门禁 ---'
    & (Join-Path $Root 'build.ps1') -Target dlengine -Test
    if ($LASTEXITCODE -ne 0) { throw '测试未通过，已中止打包' }
}

# ---------------------------------------------------------------------------
# 1. 生成图标与 Windows 资源（图标 + 版本信息 + manifest）
# ---------------------------------------------------------------------------
Write-Host '--- 生成应用图标 ---'
& $GoExe run ./build/tools/makeicon build/icon.ico
if ($LASTEXITCODE -ne 0) { throw '图标生成失败' }

$winres = Join-Path $env:GOPATH 'bin\go-winres.exe'
if (-not (Test-Path $winres)) {
    Write-Host '--- 安装 go-winres ---'
    & $GoExe install github.com/tc-hib/go-winres@latest
    if ($LASTEXITCODE -ne 0) { throw 'go-winres 安装失败' }
}

# 资源文件（.syso）会被同包内的 go build 自动链接。
# 两个入口使用不同的 manifest：GUI 需要 DPI 感知，控制台程序需要控制台子系统。
$targets = @(
    @{ Name = 'p2pdl-gui';    Pkg = './cmd/p2pdl-gui'; Out = 'p2pdl-gui.exe';    Tags = 'desktop,production'; Manifest = 'gui';     Description = 'p2pdl 下载器（图形界面）' },
    @{ Name = 'p2pdl-engine'; Pkg = './cmd/dlengine';  Out = 'p2pdl-engine.exe'; Tags = '';                   Manifest = 'cli';     Description = 'p2pdl 下载引擎' },
    @{ Name = 'dl';           Pkg = './cmd/dl';        Out = 'dl.exe';           Tags = '';                   Manifest = 'cli';     Description = 'p2pdl 命令行客户端' }
)

foreach ($t in $targets) {
    # --out 是「输出文件名前缀」而不是目录：生成的文件名形如 <前缀>_windows_amd64.syso，
    # 它必须位于目标包目录内才会被 go build 自动链接。
    $resDir = Join-Path $Root ("cmd\" + ($t.Pkg -replace '^\./cmd/', ''))
    $resOut = Join-Path $resDir 'rsrc'
    Write-Host "--- 生成 Windows 资源: $($t.Name) ---"
    & $winres simply `
        --icon build/icon.ico `
        --manifest $t.Manifest `
        --product-name 'p2pdl' `
        --file-description $t.Description `
        --file-version $ver4 `
        --product-version $ver4 `
        --copyright "p2pdl contributors" `
        --out $resOut
    if ($LASTEXITCODE -ne 0) { throw "生成 $($t.Name) 资源失败" }
}

# ---------------------------------------------------------------------------
# 2. 构建
# ---------------------------------------------------------------------------
$stage = Join-Path $BuildDir 'p2pdl'
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Force -Path $stage | Out-Null

foreach ($t in $targets) {
    $outPath = Join-Path $stage $t.Out
    Write-Host "--- 构建 $($t.Name) ---"
    if ($t.Tags) {
        & $GoExe build -trimpath -tags $t.Tags -ldflags $ldflags -o $outPath $t.Pkg
    } else {
        & $GoExe build -trimpath -ldflags $ldflags -o $outPath $t.Pkg
    }
    if ($LASTEXITCODE -ne 0) { throw "构建 $($t.Name) 失败" }
}

# ---------------------------------------------------------------------------
# 3. 附加文件（说明、许可、卸载）
# ---------------------------------------------------------------------------
$readme = @"
p2pdl $Version —— Windows 下载工具（HTTP 多段并发 / BitTorrent / 断点续传 / 大文件）

【快速开始】
  1. 解压本压缩包到任意目录（例如 D:\Tools\p2pdl）
  2. 双击 p2pdl-gui.exe
  3. 在输入框粘贴 HTTP 链接或磁力链（magnet:?xt=...），点击「开始下载」
     也可以把 .torrent 文件直接拖进窗口

【包含的程序】
  p2pdl-gui.exe     图形界面（需要 Microsoft Edge WebView2 Runtime）
  p2pdl-engine.exe  下载引擎（由界面自动拉起，也可手动运行查看日志）
  dl.exe            命令行客户端

【系统要求】
  Windows 10 1809 及以上（x64）
  Microsoft Edge WebView2 Runtime —— Windows 11 与新版 Windows 10 已内置；
  若缺失，启动界面时会提示并引导到微软官方下载页。

【命令行用法】
  dl serve --data-dir D:\downloads        前台运行引擎
  dl add "https://example.com/a.iso"      新建任务并显示进度
  dl list / pause / resume / recheck / rm 任务管理
  dl limit 2MB                            全局限速
  dl help                                 查看全部命令

【数据与配置位置】
  下载目录      %USERPROFILE%\Downloads\p2pdl
  配置文件      %APPDATA%\p2pdl\settings.json
  任务元数据    <下载目录>\.p2pdl\

【常见问题】
  下载没有开始？
    · 打开界面底部的「运行日志」查看原因
    · 检查磁盘剩余空间（开始前会预检查）
  关掉窗口后下载会停止吗？
    · 默认会停止。关闭时的确认框里可选「保留后台下载」，此时下载会继续，
      再次打开界面即可接管。
  校验失败怎么办？
    · 在任务上点「校验」：若此前生成过分块清单，只会重新下载受损的少量数据块，
      而不是整个文件。

【法律声明】
  本工具只实现通用的 HTTP / BitTorrent 协议，不内置任何资源索引、种子站点或搜索功能。
  请遵守你所在地区的法律法规与著作权要求，仅用于下载你有权获取的内容。
"@
Set-Content -Path (Join-Path $stage 'README.txt') -Value $readme -Encoding UTF8

$license = @"
MIT License

Copyright (c) p2pdl contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

--------------------------------------------------------------------
第三方依赖（详见 docs/third-party.md）：
  github.com/anacrolix/torrent  MPL-2.0
  github.com/wailsapp/wails/v2  MIT
  golang.org/x/time             BSD-3-Clause
"@
Set-Content -Path (Join-Path $stage 'LICENSE.txt') -Value $license -Encoding UTF8

# 卸载脚本：随包分发，便于用户彻底清理自身目录与配置
$uninstall = @"
# 卸载 p2pdl：删除安装目录、配置与开始菜单快捷方式（不删除已下载的文件）
`$ErrorActionPreference = 'Stop'
`$installDir = Split-Path -Parent `$MyInvocation.MyCommand.Path
`$configDir  = Join-Path `$env:APPDATA 'p2pdl'
`$shortcut   = Join-Path `$env:APPDATA 'Microsoft\Windows\Start Menu\Programs\p2pdl.lnk'

if (Test-Path `$shortcut) { Remove-Item `$shortcut -Force; Write-Host "已删除快捷方式" }
if (Test-Path `$configDir) { Remove-Item `$configDir -Recurse -Force; Write-Host "已删除配置 " + `$configDir }
Write-Host "已下载的文件不会被删除，请按需手动清理下载目录。"
Write-Host "可手动删除安装目录: " + `$installDir
"@
Set-Content -Path (Join-Path $stage 'uninstall.ps1') -Value $uninstall -Encoding UTF8

# ---------------------------------------------------------------------------
# 4. 免安装 ZIP + 校验和
# ---------------------------------------------------------------------------
$zipName = "p2pdl-$Version-win-x64.zip"
$zipPath = Join-Path $DistDir $zipName
if (Test-Path $zipPath) { Remove-Item $zipPath -Force }

Write-Host '--- 压缩免安装包 ---'
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zipPath -CompressionLevel Optimal

$hash = (Get-FileHash $zipPath -Algorithm SHA256).Hash.ToLower()
$shaFile = "$zipPath.sha256"
Set-Content -Path $shaFile -Value ("{0}  {1}" -f $hash, $zipName) -Encoding UTF8

# ---------------------------------------------------------------------------
# 5. 安装脚本（可选，比静默安装更可审计）
# ---------------------------------------------------------------------------
$install = @"
# 安装 p2pdl 到当前用户目录并创建开始菜单快捷方式（无需管理员权限）
`$ErrorActionPreference = 'Stop'
`$source     = Split-Path -Parent `$MyInvocation.MyCommand.Path
`$installDir = Join-Path `$env:LOCALAPPDATA 'Programs\p2pdl'
`$startMenu  = Join-Path `$env:APPDATA 'Microsoft\Windows\Start Menu\Programs'

Write-Host "安装到 " + `$installDir
New-Item -ItemType Directory -Force -Path `$installDir | Out-Null
Get-ChildItem -Path `$source -Filter '*.exe' | ForEach-Object { Copy-Item `$_.FullName `$installDir -Force }
Get-ChildItem -Path `$source -Filter '*.txt' | ForEach-Object { Copy-Item `$_.FullName `$installDir -Force }

`$exe = Join-Path `$installDir 'p2pdl-gui.exe'
`$lnk = Join-Path `$startMenu 'p2pdl.lnk'
`$shell = New-Object -ComObject WScript.Shell
`$sc = `$shell.CreateShortcut(`$lnk)
`$sc.TargetPath = `$exe
`$sc.WorkingDirectory = `$installDir
`$sc.Description = 'p2pdl 下载器'
`$sc.Save()

Write-Host "已创建开始菜单快捷方式: " + `$lnk
Write-Host "安装完成。卸载请运行安装目录下的 uninstall.ps1"
"@
Set-Content -Path (Join-Path $DistDir 'install.ps1') -Value $install -Encoding UTF8
Copy-Item (Join-Path $DistDir 'install.ps1') (Join-Path $stage 'install.ps1') -Force
Copy-Item (Join-Path $stage 'uninstall.ps1') (Join-Path $DistDir 'uninstall.ps1') -Force

# 重新打包以包含 install.ps1
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zipPath -CompressionLevel Optimal -Force
$hash = (Get-FileHash $zipPath -Algorithm SHA256).Hash.ToLower()
Set-Content -Path $shaFile -Value ("{0}  {1}" -f $hash, $zipName) -Encoding UTF8

# ---------------------------------------------------------------------------
# 6. 汇总
# ---------------------------------------------------------------------------
Write-Host ''
Write-Host '打包完成：'
Get-ChildItem $stage -File | ForEach-Object {
    '{0,-22} {1,12:N0} bytes' -f $_.Name, $_.Length | Write-Host
}
Write-Host ''
Write-Host ("免安装包: {0} ({1:N0} bytes)" -f $zipPath, (Get-Item $zipPath).Length)
Write-Host ("SHA-256 : {0}" -f $hash)

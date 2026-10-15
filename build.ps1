# p2pdl 构建脚本（Windows / PowerShell 5.1+）
#
# 设计目标：
#   - 一条命令完成构建，无需预先安装或配置系统级 Go；
#   - 工具链、模块缓存、下载缓存全部落在项目内 .tools/ 下，不污染用户环境；
#   - 版本信息（版本号 / 提交号 / 构建时间）通过 ldflags 注入 internal/version。
#
# 用法：
#   .\build.ps1                  # 构建全部目标到 dist/
#   .\build.ps1 -Target dlengine # 只构建引擎进程
#   .\build.ps1 -Test            # 构建前先跑 go vet 与单元测试
#   .\build.ps1 -Version 1.0.0   # 显式指定版本号
#   .\build.ps1 -Clean           # 清理 dist/ 与临时构建产物

[CmdletBinding()]
param(
    [ValidateSet('all', 'dlengine', 'dl', 'gui')]
    [string]$Target = 'all',

    [string]$Version = '',

    [switch]$Test,

    [switch]$Clean
)

$ErrorActionPreference = 'Stop'
# 统一控制台输出编码，避免中文在 GBK 代码页下乱码。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$Root = $PSScriptRoot
$DistDir = Join-Path $Root 'dist'

# ---------------------------------------------------------------------------
# 清理
# ---------------------------------------------------------------------------
if ($Clean) {
    foreach ($p in @($DistDir, (Join-Path $Root '.tools\gocache'))) {
        if (Test-Path $p) { Remove-Item $p -Recurse -Force; Write-Host "已清理 $p" }
    }
    if (-not $Test -and $Version -eq '' -and $Target -eq 'all') { exit 0 }
}

# ---------------------------------------------------------------------------
# 定位 Go 工具链：优先环境变量中的 go，其次项目内 .tools/go
# ---------------------------------------------------------------------------
$localGo = Join-Path $Root '.tools\go\bin\go.exe'
if (-not (Test-Path $localGo)) {
    # 「一条命令构建」的前提：缺工具链时自动获取，而不是让用户先去看文档。
    $goVersion = '1.27.2'
    $goZipName = "go$goVersion.windows-amd64.zip"
    $goSha256 = '1314008898bd40df77af4b014f777f08873dbdfbcd3d92308728ee03304fe04f'
    $toolsDir = Join-Path $Root '.tools'
    $goZip = Join-Path $toolsDir $goZipName
    New-Item -ItemType Directory -Force -Path $toolsDir | Out-Null

    Write-Host "未找到 Go 工具链，正在下载 go$goVersion（约 79MB，仅首次）…"
    # go.dev / dl.google.com 在部分网络下不可达，因此使用官方镜像。
    $urls = @(
        "https://golang.google.cn/dl/$goZipName",
        "https://mirrors.aliyun.com/golang/$goZipName"
    )
    $downloaded = $false
    foreach ($url in $urls) {
        try {
            Write-Host "  ← $url"
            Invoke-WebRequest -Uri $url -OutFile $goZip -UseBasicParsing -TimeoutSec 900
            $downloaded = $true
            break
        } catch {
            Write-Host "  下载失败: $($_.Exception.Message)"
            if (Test-Path $goZip) { Remove-Item $goZip -Force }
        }
    }
    if (-not $downloaded) {
        throw "无法下载 Go 工具链。请手动下载 go$goVersion.windows-amd64.zip 并解压到 .tools/，或安装系统级 Go 1.24+ 后重试。"
    }

    $hash = (Get-FileHash $goZip -Algorithm SHA256).Hash.ToLower()
    if ($hash -ne $goSha256) {
        Remove-Item $goZip -Force
        throw "Go 工具链校验和不匹配（得到 $hash，期望 $goSha256），已删除下载文件"
    }
    Write-Host '  校验和验证通过，正在解压…'
    Expand-Archive -Path $goZip -DestinationPath $toolsDir -Force
    Remove-Item $goZip -Force
    $localGo = Join-Path $Root '.tools\go\bin\go.exe'
    if (-not (Test-Path $localGo)) { throw '解压后仍未找到 go.exe' }
}

if (Test-Path $localGo) {
    $GoExe = $localGo
} elseif (Get-Command go -ErrorAction SilentlyContinue) {
    $GoExe = (Get-Command go).Source
} else {
    throw '未找到 Go 工具链'
}
$GoRoot = Split-Path (Split-Path $GoExe -Parent) -Parent

# ---------------------------------------------------------------------------
# 项目内隔离的 Go 环境（不写入用户级 go env 配置）
# ---------------------------------------------------------------------------
$env:GOROOT  = $GoRoot
$env:GOPATH  = Join-Path $Root '.tools\gopath'
$env:GOCACHE = Join-Path $Root '.tools\gocache'
# go test 会把测试二进制放在 GOTMPDIR；本项目内 .tmp/ 目录下无法执行 exe（Windows 限制），
# 因此显式指向 .tools/tmp。
$env:GOTMPDIR = Join-Path $Root '.tools\tmp'
# go.dev / dl.google.com 在部分网络下不可达，默认走国内模块代理。
if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }
$env:GOFLAGS = '-mod=mod'
New-Item -ItemType Directory -Force -Path $env:GOPATH, $env:GOCACHE, $env:GOTMPDIR, $DistDir | Out-Null

# ---------------------------------------------------------------------------
# 版本信息注入
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

if (-not $Version) {
    $Version = Get-GitValue @('describe', '--tags', '--always', '--dirty') '0.0.0-dev'
}
$Commit    = Get-GitValue @('rev-parse', '--short', 'HEAD') 'unknown'
$BuildTime = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

$ldflags = @(
    '-s', '-w',
    "-X 'p2pdl/internal/version.Version=$Version'",
    "-X 'p2pdl/internal/version.Commit=$Commit'",
    "-X 'p2pdl/internal/version.BuildTime=$BuildTime'"
) -join ' '

Write-Host "Go       : $(& $GoExe version)"
Write-Host "版本     : $Version (commit $Commit, $BuildTime)"

# ---------------------------------------------------------------------------
# 单元测试执行器
# ---------------------------------------------------------------------------
# Windows 实时防护会在 exe 刚写入磁盘的瞬间持有扫描锁，此时 fork/exec 会返回
# "Access is denied"。直接跑 `go test ./...` 会命中这个竞争（go test 总是把测试
# 二进制写到随机临时目录后立即执行）。因此改为：先 go test -c 显式编译到固定路径，
# 短暂等待后执行；如果仍被系统拒绝，则退避重试。
function Invoke-GoTests {
    $pkgs = & $GoExe list ./...
    if ($LASTEXITCODE -ne 0) { throw 'go list 失败' }

    $ran = 0
    foreach ($pkg in $pkgs) {
        if (-not $pkg) { continue }
        $probe = & $GoExe list -f '{{if or .TestGoFiles .XTestGoFiles}}yes{{end}}' $pkg
        if ($probe -ne 'yes') { continue }

        $ran++
        $safe = $pkg -replace '[^A-Za-z0-9]', '_'
        $bin = Join-Path $env:GOTMPDIR ($safe + '.test.exe')

        & $GoExe test -c -o $bin $pkg
        if ($LASTEXITCODE -ne 0) { throw "测试二进制编译失败: $pkg" }
        Start-Sleep -Milliseconds 400

        $passed = $false
        for ($attempt = 1; $attempt -le 4; $attempt++) {
            Write-Host "--- 运行测试 $pkg (第 $attempt 次尝试) ---"
            # 参数写成单个 token（等号形式），避免 PowerShell 对 -test.timeout 这类参数的额外解释。
            & $bin '-test.timeout=300s'
            if ($LASTEXITCODE -eq 0) { $passed = $true; break }
            # 无法从退出码区分「测试失败」与「被防护拦截」，故统一退避重试。
            Start-Sleep -Milliseconds (600 * $attempt)
        }
        if (-not $passed) { throw "测试未通过: $pkg" }
    }

    if ($ran -eq 0) { Write-Host '（没有发现测试文件）' }
    else { Write-Host "全部测试通过（$ran 个包）" }
}

# ---------------------------------------------------------------------------
# 质量门禁
# ---------------------------------------------------------------------------
if ($Test) {
    Write-Host '--- go vet ./... ---'
    & $GoExe vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet 未通过' }

    Write-Host '--- go test ---'
    Invoke-GoTests
}

# ---------------------------------------------------------------------------
# 构建目标
# ---------------------------------------------------------------------------
# GUI 需要 desktop,production 标签：Wails 的资产服务与生产模式资源依赖它们。
$targets = [ordered]@{
    dlengine = @{ Pkg = './cmd/dlengine'; Out = 'dlengine.exe'; Tags = '' }
    dl       = @{ Pkg = './cmd/dl';       Out = 'dl.exe';       Tags = '' }
    gui      = @{ Pkg = './cmd/p2pdl-gui'; Out = 'p2pdl-gui.exe'; Tags = 'desktop,production' }
}
if ($Target -ne 'all') { $targets = [ordered]@{ $Target = $targets[$Target] } }

foreach ($name in $targets.Keys) {
    $spec = $targets[$name]
    $out = Join-Path $DistDir $spec.Out
    Write-Host "--- 构建 $name -> $out ---"
    if ($spec.Tags) {
        & $GoExe build -trimpath -tags $spec.Tags -ldflags $ldflags -o $out $spec.Pkg
    } else {
        & $GoExe build -trimpath -ldflags $ldflags -o $out $spec.Pkg
    }
    if ($LASTEXITCODE -ne 0) { throw "构建 $name 失败" }
}

Write-Host ''
Write-Host '构建完成，产物：'
Get-ChildItem $DistDir -Filter *.exe | ForEach-Object {
    '{0,-16} {1,10:N0} bytes' -f $_.Name, $_.Length | Write-Host
}
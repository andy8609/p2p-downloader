# Windows 打包与分发（T14）

实现：`package.ps1`（打包）、`build/tools/makeicon`（图标生成）、`internal/gui/platform_windows.go`（WebView2 检测与引导）。

---

## 1. 产物

运行 `.\package.ps1` 后，`dist/` 下产出：

| 产物 | 说明 |
|---|---|
| `p2pdl-<版本>-win-x64.zip` | **免安装包**：解压后双击 `p2pdl-gui.exe` 即可用 |
| `p2pdl-<版本>-win-x64.zip.sha256` | SHA-256 校验和 |
| `install.ps1` | 可选安装脚本（装到用户目录 + 开始菜单快捷方式 + 卸载入口） |
| `uninstall.ps1` | 卸载脚本（不删除已下载的文件） |
| `dist/package/p2pdl/` | 打包暂存目录，便于检查内容 |

包内文件：

```
p2pdl-gui.exe        图形界面（11.9 MB，含图标与版本信息）
p2pdl-engine.exe     下载引擎（21.3 MB，由界面拉起，也可单独运行）
dl.exe               命令行客户端（21.3 MB）
README.txt           使用说明与常见问题
LICENSE.txt          MIT + 第三方依赖许可
install.ps1          可选安装脚本
uninstall.ps1        卸载脚本
```

**为什么不是一个 exe**：引擎独立进程是架构基石（界面崩溃/卡顿不中断下载）。
把引擎塞进 GUI 进程能换来单文件分发，但会牺牲这一保证，因此不做这种交换。
用户侧的体验差异仅是「解压后有两个 exe」，安装脚本会自动处理。

---

## 2. 打包脚本做了什么

```
build.ps1 -Test            ← 质量门禁（默认开启，-SkipTests 可跳过）
makeicon build/icon.ico    ← 生成 6 个尺寸的图标（16/32/48/64/128/256）
go-winres simply ...       ← 生成 rsrc_windows_amd64.syso（图标 + VERSIONINFO + manifest）
go build -trimpath ...     ← 构建三个二进制（GUI 带 -tags desktop,production）
Compress-Archive           ← 免安装 ZIP
Get-FileHash               ← SHA-256
```

**图标与版本信息实测结果**：

```
p2pdl-gui    | product=p2pdl | fileVer=1.0.0.0 | prodVer=1.0.0.0
p2pdl-engine | product=p2pdl | fileVer=1.0.0.0 | prodVer=1.0.0.0
dl           | product=p2pdl | fileVer=1.0.0.0 | prodVer=1.0.0.0
图标提取     | 32x32（多尺寸 ICO 内嵌 PNG）
```

> 踩坑记录（已修）：`go-winres simply` 的 `--out` 是**输出文件名前缀**而不是目录。
> 最初传目录导致 `.syso` 落在 `cmd/` 下、未被链接，图标显示的是 Wails 默认图标、
> VERSIONINFO 全为空。现在传 `<包目录>/rsrc`。
> 另外 VERSIONINFO 需要四段版本号，脚本会把 `1.0.0` 或 `v1.0.0-dirty` 规范化为 `1.0.0.0`。

**控制台窗口**：`p2pdl-engine.exe` 仍是控制台程序（用户可手动运行看日志），
但由 GUI 拉起时通过 `SysProcAttr{HideWindow: true, CreationFlags: CREATE_NO_WINDOW}`
隐藏窗口，因此双击 `p2pdl-gui.exe` 不会闪出黑框。`dl.exe` 需要控制台，保持原样。

---

## 3. WebView2 依赖处理

界面依赖 Microsoft Edge WebView2 Runtime。

**检测**（`CheckWebView2`，两条路径任一命中即视为已安装）：

1. 注册表 `HKLM\SOFTWARE\(WOW6432Node\)Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}` 的 `pv` 值；
2. 目录 `C:\Program Files (x86)\Microsoft\EdgeWebView\Application\<版本号>`。

**引导**（`PromptAndGuideWebView2`）：缺失时弹出原生消息框说明原因，
点「是」用系统默认浏览器打开微软官方下载页（只允许 https 链接，
`OpenURL` 拒绝 `file:`/`javascript:` 等协议，避免变成任意程序启动入口），
安装后重新运行即可。这满足「缺 WebView2 时给出明确提示并引导安装」。

**实测**：本机 WebView2 `153.0.4234.48`，GUI 启动日志首行即输出
`WebView2 Runtime 可用：153.0.4234.48`。

---

## 4. 干净环境验证（T14 验收项 1）

无法获得一台真实的空白 Windows 机器，因此做了**尽可能严格的等价验证**：
把 ZIP 解压到新目录，用 `env -i PATH="/c/Windows/system32:/c/Windows"` 启动，
**PATH 中不含 Go、Node、Git 或任何开发工具**。

结果：

```
gui.log:
  2026/10/12 15:44:49 WebView2 Runtime 可用：153.0.4234.48
  2026/10/12 15:44:49 [WebView2] Environment created successfully
  2026/10/12 15:44:50 桌面壳已启动，下载目录 D:\...\cleanroom\dl
  2026/10/12 15:44:52 gui: 已启动引擎进程 pid=17792 (...\p2pdl-engine.exe)
  2026/10/12 15:44:52 engine> 控制面已启动于 http://127.0.0.1:3637

进程:  17792 p2pdl-engine    21008 p2pdl-gui

随后用包内的 dl.exe 完成一次下载：
  完成: cleaned.bin（768.00 KB）
  源文件 SHA-256 739390784c0bd646… == 下载文件 SHA-256 739390784c0bd646…   MATCH
```

即：**受限 PATH 下双击即用、自动拉起引擎、并通过包内 CLI 完成了一次校验一致的下载**。

Go 二进制是静态链接的，运行时不需要 Go 工具链——这也是「一条命令构建 + 免安装分发」的前提。

### 未能在真实干净机器上验证的部分（诚实记录）

- **操作系统层面缺 WebView2 的表现**：本机已装运行时，无法制造缺失环境。
  检测逻辑（`TestCheckWebView2ReportsVersion`、`TestWebView2DetectionPaths`）有测试覆盖，
  但「真实缺失 → 弹窗 → 安装后恢复」这条路径未在真机演练。
- **杀软误报**：未做多引擎扫描验证。已知风险点是 Go 编写的下载器
  （尤其是内置 BitTorrent 客户端）容易被启发式误判，建议发布时：
  1. 提交到微软 Defender 误报反馈渠道；
  2. 对安装包做代码签名（需要证书，当前未配置）；
  3. 在 README 中说明校验和以便用户自证完整性。

---

## 5. 安装脚本（可选）

```powershell
# 解压后运行（无需管理员权限）
.\install.ps1
```

它把 exe 复制到 `%LOCALAPPDATA%\Programs\p2pdl`，并在开始菜单创建快捷方式。
选择脚本而不是 Inno Setup/NSIS 的理由：脚本可读、可审、可复现，
不引入额外工具链，也不需要管理员权限。

卸载：`uninstall.ps1` 删除快捷方式与 `%APPDATA%\p2pdl` 配置，
**不会删除已下载的文件**（用户数据不应被安装器隐式清理）。

---

## 6. 数据与配置位置

| 内容 | 位置 |
|---|---|
| 下载文件 | `%USERPROFILE%\Downloads\p2pdl`（可在设置中修改） |
| 用户设置 | `%APPDATA%\p2pdl\settings.json` |
| 任务元数据（断点、位图、索引） | `<下载目录>\.p2pdl\` |
| 控制面令牌 | `<下载目录>\.p2pdl\control.token`（权限 0600） |
| 端点文件 | `<下载目录>\.p2pdl\control.endpoint` |

---

## 7. 复现命令

```powershell
# 完整打包（含测试门禁）
.\package.ps1

# 指定版本号
.\package.ps1 -Version 1.2.3

# 快速出包（跳过测试，仅用于本地验证）
.\package.ps1 -SkipTests
```

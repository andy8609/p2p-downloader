// p2pdl 桌面壳前端逻辑。
//
// 与后端交互的唯一入口是 Wails 生成的 window.go.main.Service。
// 前端每秒轮询一次 GetState，整体重绘任务列表；同时按需拉取速率曲线、
// 通知队列与设置。选择轮询而非事件推送的原因见 docs/gui.md。

const POLL_INTERVAL_MS = 1000;
const SPARK_POINTS = 60;

const state = {
  timer: null,
  tasks: [],
  history: new Map(),   // id -> [{at, rate, done}]
  settings: null,
  settingsOpen: false,
  clipboardEnabled: true,
  lastClipboard: "",
  dragDepth: 0,
};

const el = (id) => document.getElementById(id);

// service 返回后端绑定；在浏览器中直接打开时会缺失。
//
// 绑定路径由 Go 侧的「包名.结构体名」决定：本项目的 Service 位于 internal/gui，
// 因此实际挂在 window.go.gui.Service 上（而不是 Wails 模板示例里的 window.go.main）。
// 这里做兼容查找，避免将来移动包位置时界面整体失效——之前正是踩了这个坑：
// 只认 window.go.main.Service，结果所有按钮都无声无息地不工作。
function service() {
  const g = window.go;
  if (!g) return null;
  for (const pkg of Object.keys(g)) {
    const ns = g[pkg];
    if (ns && typeof ns === "object" && ns.Service) return ns.Service;
  }
  return null;
}

// backendDiagnostics 返回后端绑定的可用路径，用于排障提示。
function backendDiagnostics() {
  if (!window.go) return "window.go 未注入（Wails 运行时脚本可能未加载）";
  const paths = [];
  for (const pkg of Object.keys(window.go)) {
    const ns = window.go[pkg];
    if (ns && typeof ns === "object") {
      for (const name of Object.keys(ns)) paths.push(`window.go.${pkg}.${name}`);
    }
  }
  return paths.length ? "可用绑定: " + paths.join(", ") : "window.go 为空（后端未绑定任何结构体）";
}

function toast(message, kind = "") {
  const node = el("toast");
  node.textContent = message;
  node.className = "toast show " + kind;
  clearTimeout(node._timer);
  node._timer = setTimeout(() => { node.className = "toast"; }, 4000);
}

// --------------------------- 格式化 ---------------------------

function formatBytes(n) {
  if (!n || n <= 0) return "未知";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : 2) + " " + units[i];
}

function formatRate(v) {
  if (!v || v <= 0) return "—";
  return formatBytes(v) + "/s";
}

// 剩余时间：用瞬时速率外推。速率过低或进度未知时返回空串，避免显示 "∞"。
function formatETA(task) {
  const remaining = (task.total || 0) - (task.done || 0);
  if (!task.rate || task.rate <= 0 || remaining <= 0) return "";
  const seconds = remaining / task.rate;
  if (!isFinite(seconds) || seconds <= 0) return "";
  if (seconds > 86400 * 7) return "";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `剩余 ${h} 小时 ${m} 分`;
  if (m > 0) return `剩余 ${m} 分 ${s} 秒`;
  return `剩余 ${s} 秒`;
}

const STATE_LABELS = {
  pending: "等待中", running: "下载中", paused: "已暂停",
  verifying: "校验中", completed: "已完成", failed: "失败",
};

function stateLabel(s) { return STATE_LABELS[s] || s; }

function actionsFor(task) {
  const actions = [];
  if (["running", "pending", "verifying"].includes(task.state)) actions.push({ id: "pause", label: "暂停" });
  if (["paused", "failed"].includes(task.state)) actions.push({ id: "resume", label: "继续" });
  if (task.state === "completed") actions.push({ id: "open", label: "打开" });
  actions.push({ id: "recheck", label: "校验" });
  actions.push({ id: "remove", label: "删除" });
  return actions;
}

// --------------------------- 渲染 ---------------------------

function renderEngine(view) {
  const dot = el("engineDot");
  const text = el("engineText");
  if (view.engineRunning) {
    dot.className = "dot ok";
    text.textContent = "引擎运行中 (" + (view.engineAddr || "") + ")";
  } else {
    dot.className = "dot err";
    text.textContent = view.engineError ? ("引擎未运行：" + view.engineError) : "引擎未运行";
  }
}

function renderSummary(view) {
  const stats = view.stats || {};
  el("statTotal").textContent = stats.total ?? state.tasks.length;
  el("statRunning").textContent = stats.running ?? 0;
  el("statCompleted").textContent = stats.completed ?? 0;
  el("statFailed").textContent = stats.failed ?? 0;

  const select = el("rateSelect");
  const current = String(view.rateLimit || 0);
  if (Array.from(select.options).some((o) => o.value === current)) select.value = current;
}

function buildSparkline(samples) {
  const canvas = document.createElement("canvas");
  canvas.className = "spark";
  // 高 DPI 下按设备像素比放大，避免曲线发虚。
  const dpr = window.devicePixelRatio || 1;
  const width = 160, height = 26;
  canvas.width = width * dpr;
  canvas.height = height * dpr;
  canvas.style.width = "100%";
  canvas.style.height = height + "px";

  const ctx = canvas.getContext("2d");
  ctx.scale(dpr, dpr);
  const points = samples.slice(-SPARK_POINTS);
  if (points.length < 2) return canvas;

  const max = Math.max(...points.map((p) => p.rate), 1);
  ctx.beginPath();
  ctx.strokeStyle = "#2563eb";
  ctx.lineWidth = 1.5;
  points.forEach((p, i) => {
    const x = (i / (points.length - 1)) * width;
    const y = height - (p.rate / max) * (height - 3) - 1.5;
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.stroke();

  // 填充区域让曲线更容易看清
  ctx.lineTo(width, height);
  ctx.lineTo(0, height);
  ctx.closePath();
  ctx.fillStyle = "rgba(37, 99, 235, 0.12)";
  ctx.fill();
  return canvas;
}

function buildRow(task) {
  const row = document.createElement("div");
  row.className = "task-row";

  const progress = Math.max(0, Math.min(1, task.progress || 0));
  const pct = (progress * 100).toFixed(1) + "%";
  const fillClass = task.state === "completed" ? "done" : (task.state === "failed" ? "err" : "");

  const name = document.createElement("div");
  name.className = "task-name";
  name.textContent = task.displayName || task.id;
  const sub = document.createElement("small");
  sub.textContent = (task.kind === "bittorrent" ? "BT · " : "HTTP · ") + (task.destPath || "");
  name.appendChild(sub);

  const progressWrap = document.createElement("div");
  progressWrap.className = "progress-wrap";
  const bar = document.createElement("div");
  bar.className = "progress-bar";
  const fill = document.createElement("div");
  fill.className = "progress-fill " + fillClass;
  fill.style.width = pct;
  bar.appendChild(fill);
  const progressText = document.createElement("span");
  progressText.className = "progress-text";
  progressText.textContent = pct;
  progressWrap.append(bar, progressText);

  // 速度 + 剩余时间 + 曲线
  const rateCell = document.createElement("div");
  rateCell.className = "rate-cell";
  const rateText = document.createElement("div");
  rateText.className = "mono";
  rateText.textContent = formatRate(task.rate);
  rateCell.appendChild(rateText);
  const eta = formatETA(task);
  if (eta) {
    const etaNode = document.createElement("div");
    etaNode.className = "eta";
    etaNode.textContent = eta;
    rateCell.appendChild(etaNode);
  }
  const samples = state.history.get(task.id);
  if (samples && samples.length > 1 && ["running", "paused"].includes(task.state)) {
    rateCell.appendChild(buildSparkline(samples));
  }

  const size = document.createElement("div");
  size.className = "mono";
  size.textContent = formatBytes(task.done) + " / " + formatBytes(task.total);

  const badge = document.createElement("div");
  const span = document.createElement("span");
  span.className = "state-badge " + (task.state || "");
  span.textContent = stateLabel(task.state);
  badge.appendChild(span);
  if (task.lastError) {
    const err = document.createElement("small");
    err.style.display = "block";
    err.style.color = "var(--err)";
    err.textContent = task.lastError.slice(0, 60);
    badge.appendChild(err);
  }

  const actions = document.createElement("div");
  actions.className = "row-actions";
  for (const action of actionsFor(task)) {
    const btn = document.createElement("button");
    btn.className = "btn btn-ghost";
    btn.textContent = action.label;
    btn.onclick = () => runAction(action.id, task);
    actions.appendChild(btn);
  }

  row.append(name, progressWrap, rateCell, size, badge, actions);
  return row;
}

function renderTasks(view) {
  const list = el("taskList");
  const empty = el("emptyState");
  const tasks = view.tasks || [];
  state.tasks = tasks;

  if (tasks.length === 0) {
    list.innerHTML = "";
    empty.style.display = "block";
    return;
  }
  empty.style.display = "none";

  const frag = document.createDocumentFragment();
  for (const task of tasks) frag.appendChild(buildRow(task));
  list.innerHTML = "";
  list.appendChild(frag);
}

function renderLogs(logs) {
  const details = document.querySelector(".logs details");
  if (!details || !details.open) return;
  const node = el("logView");
  node.textContent = (logs || []).slice(-80).join("\n");
  node.scrollTop = node.scrollHeight;
}

async function refresh() {
  const svc = service();
  if (!svc) return;
  try {
    const view = await svc.GetState();
    renderEngine(view);
    renderSummary(view);
    renderTasks(view);
    renderLogs(view.logs || []);
    await loadHistory();
    await drainNotifications();
  } catch (err) {
    el("engineDot").className = "dot err";
    el("engineText").textContent = "状态查询失败：" + err;
  }
}

// 只为处于活跃状态的任务拉取曲线，避免任务多时产生大量请求。
async function loadHistory() {
  const svc = service();
  if (!svc) return;
  const active = state.tasks.filter((t) => ["running", "paused"].includes(t.state));
  for (const task of active) {
    try {
      const samples = await svc.GetHistory(task.id);
      if (samples && samples.length) state.history.set(task.id, samples);
    } catch { /* 曲线是装饰性信息，失败不影响主流程 */ }
  }
  const alive = new Set(state.tasks.map((t) => t.id));
  for (const id of Array.from(state.history.keys())) {
    if (!alive.has(id)) state.history.delete(id);
  }
}

async function drainNotifications() {
  const svc = service();
  if (!svc) return;
  try {
    const items = await svc.Notifications();
    for (const n of items || []) {
      toast(`${n.title}：${n.body}`, n.kind === "success" ? "ok" : (n.kind === "error" ? "err" : ""));
    }
  } catch { /* 通知失败无需打扰用户 */ }
}

// --------------------------- 任务操作 ---------------------------

async function runAction(action, task) {
  const svc = service();
  if (!svc) return;
  try {
    switch (action) {
      case "pause": await svc.PauseTask(task.id); break;
      case "resume": await svc.ResumeTask(task.id); break;
      case "recheck":
        await svc.RecheckTask(task.id);
        toast("已触发重新校验：" + task.displayName, "ok");
        break;
      case "open": await svc.OpenContainingFolder(task.destPath); break;
      case "remove":
        if (!window.confirm("确定删除任务「" + (task.displayName || task.id) + "」？\n数据文件会保留。")) return;
        await svc.RemoveTask(task.id, false);
        toast("已删除任务记录（文件保留）", "ok");
        break;
    }
    await refresh();
  } catch (err) {
    toast(String(err), "err");
  }
}

async function addSource(source) {
  const svc = service();
  if (!svc) { toast("未检测到桌面后端，请通过 p2pdl-gui 启动", "err"); return; }
  source = (source || "").trim();
  if (!source) { toast("请填写下载地址、磁力链或种子文件路径", "err"); return; }

  el("btnAdd").disabled = true;
  try {
    const task = await svc.AddTask({
      source,
      name: "",
      paused: el("pausedCheck").checked,
      expectedHash: el("hashInput").value.trim(),
      hashAlgorithm: "sha256",
      peers: "",
    });
    el("sourceInput").value = "";
    el("hashInput").value = "";
    toast("已创建任务：" + (task.displayName || task.id), "ok");
    await refresh();
  } catch (err) {
    toast(String(err), "err");
  } finally {
    el("btnAdd").disabled = false;
  }
}

// --------------------------- 拖拽与剪贴板 ---------------------------

function setupDragAndDrop() {
  const hint = el("dropHint");

  // 用计数器抵消子元素产生的 dragenter/dragleave 抖动。
  window.addEventListener("dragenter", (e) => {
    e.preventDefault();
    state.dragDepth++;
    hint.classList.add("active");
  });
  window.addEventListener("dragover", (e) => { e.preventDefault(); });
  window.addEventListener("dragleave", (e) => {
    e.preventDefault();
    state.dragDepth = Math.max(0, state.dragDepth - 1);
    if (state.dragDepth === 0) hint.classList.remove("active");
  });
  window.addEventListener("drop", async (e) => {
    e.preventDefault();
    state.dragDepth = 0;
    hint.classList.remove("active");

    // 1) 文件拖入：优先取 .torrent 的真实路径（Wails 会填 webkitRelativePath 或 name）
    const files = Array.from(e.dataTransfer.files || []);
    for (const file of files) {
      const path = file.path || file.name;
      if (path && path.toLowerCase().endsWith(".torrent")) {
        await addSource(path);
        return;
      }
    }
    // 2) 文本拖入：链接或磁力链
    const text = (e.dataTransfer.getData("text") || "").trim();
    if (text) {
      el("sourceInput").value = text;
      await addSource(text);
      return;
    }
    if (files.length > 0) {
      toast("只支持 .torrent 文件，或直接拖入链接文本", "err");
    }
  });
}

async function readClipboardAndFill(silent) {
  const svc = service();
  let text = "";
  try {
    text = (await navigator.clipboard.readText()).trim();
  } catch {
    if (!silent) toast("无法读取剪贴板（可通过输入框直接粘贴）", "err");
    return "";
  }
  if (!isLikelySource(text)) {
    if (!silent) toast("剪贴板内容不是链接、磁力链或 .torrent 路径");
    return "";
  }
  el("sourceInput").value = text;
  if (!silent) toast("已从剪贴板填入");
  return text;
}

// isLikelySource 判断文本是否像可下载来源，避免把普通文本误填进输入框。
function isLikelySource(text) {
  if (!text || text.length > 2000) return false;
  if (/\s/.test(text.trim())) return false;
  return /^(https?:\/\/|magnet:\?xt=)/i.test(text) || /\.torrent$/i.test(text);
}

// 剪贴板监听：仅在窗口获得焦点时读取，避免后台偷偷访问剪贴板。
async function watchClipboard() {
  if (!state.clipboardEnabled) return;
  if (document.hasFocus && !document.hasFocus()) return;
  const svc = service();
  if (!svc) return;
  const text = await readClipboardAndFill(true);
  if (!text || text === state.lastClipboard) return;
  state.lastClipboard = text;
  // 只在输入框为空时自动填入，不打断用户正在输入的内容。
  if (!el("sourceInput").value.trim()) {
    toast("已在剪贴板发现下载链接，已填入输入框", "ok");
  }
}

// --------------------------- 设置面板 ---------------------------

async function openSettings() {
  const svc = service();
  if (!svc) return;
  try {
    const view = await svc.GetSettings();
    state.settings = view.settings;
    el("setDataDir").value = view.settings.dataDir || "";
    el("setConcurrency").value = view.settings.concurrency || 8;
    el("setMaxTasks").value = view.settings.maxConcurrentTasks || 3;
    el("setChunkKB").value = Math.round((view.settings.chunkSize || 1048576) / 1024);
    el("setPort").value = view.settings.port || 0;
    el("setRateKB").value = Math.round((view.settings.globalRateLimit || 0) / 1024);
    el("setAutoResume").checked = !!view.settings.autoResume;
    el("setManifest").checked = !!view.settings.buildManifest;
    el("setNotify").checked = !!view.settings.notifyOnComplete;
    el("setClipboard").checked = !!view.settings.clipboardMonitor;
    el("settingsPath").textContent = view.settingsPath || "";
    state.clipboardEnabled = !!view.settings.clipboardMonitor;

    el("saveHint").textContent = (view.pendingRestart || []).length
      ? "有设置已保存但需重启引擎才生效：" + view.pendingRestart.join("、")
      : "";
    el("settingsMask").classList.add("open");
    state.settingsOpen = true;
  } catch (err) {
    toast(String(err), "err");
  }
}

function closeSettings() {
  el("settingsMask").classList.remove("open");
  state.settingsOpen = false;
}

async function saveSettings() {
  const svc = service();
  if (!svc || !state.settings) return;

  const next = Object.assign({}, state.settings, {
    dataDir: el("setDataDir").value.trim(),
    concurrency: parseInt(el("setConcurrency").value, 10) || 8,
    maxConcurrentTasks: parseInt(el("setMaxTasks").value, 10) || 3,
    chunkSize: (parseInt(el("setChunkKB").value, 10) || 1024) * 1024,
    port: parseInt(el("setPort").value, 10) || 0,
    globalRateLimit: (parseInt(el("setRateKB").value, 10) || 0) * 1024,
    autoResume: el("setAutoResume").checked,
    buildManifest: el("setManifest").checked,
    notifyOnComplete: el("setNotify").checked,
    clipboardMonitor: el("setClipboard").checked,
  });

  try {
    const view = await svc.SaveSettings(next);
    state.settings = view.settings;
    state.clipboardEnabled = !!view.settings.clipboardMonitor;
    if ((view.pendingRestart || []).length) {
      toast("已保存。以下项需重启引擎生效：" + view.pendingRestart.join("、"), "ok");
      el("saveHint").textContent = "需重启引擎才生效：" + view.pendingRestart.join("、");
    } else {
      toast("设置已保存并生效", "ok");
      el("saveHint").textContent = "";
    }
    await refresh();
  } catch (err) {
    toast(String(err), "err");
  }
}

async function pickDirectory() {
  const svc = service();
  if (!svc) return;
  try {
    const dir = await svc.PickDataDir();
    if (dir) el("setDataDir").value = dir;
  } catch (err) {
    toast(String(err), "err");
  }
}

// --------------------------- 绑定与启动 ---------------------------

function bind() {
  el("btnAdd").onclick = () => addSource(el("sourceInput").value);
  el("sourceInput").addEventListener("keydown", (e) => {
    if (e.key === "Enter") addSource(el("sourceInput").value);
  });

  el("btnClipboard").onclick = async () => {
    const text = await readClipboardAndFill(false);
    if (text) el("sourceInput").focus();
  };

  el("btnRestart").onclick = async () => {
    const svc = service();
    if (!svc) return;
    const btn = el("btnRestart");
    btn.disabled = true;
    try {
      await svc.RestartEngine();
      toast("引擎已重启", "ok");
      await refresh();
    } catch (err) {
      toast(String(err), "err");
    } finally {
      btn.disabled = false;
    }
  };

  el("btnOpenDir").onclick = async () => {
    const svc = service();
    if (!svc) return;
    try { await svc.OpenPath(""); } catch (err) { toast(String(err), "err"); }
  };

  el("rateSelect").onchange = async (e) => {
    const svc = service();
    if (!svc) return;
    try {
      await svc.SetRateLimit(parseInt(e.target.value, 10));
      toast("限速已更新", "ok");
      await refresh();
    } catch (err) {
      toast(String(err), "err");
    }
  };

  el("btnSettings").onclick = openSettings;
  el("btnCloseSettings").onclick = closeSettings;
  el("btnSaveSettings").onclick = saveSettings;
  el("btnPickDir").onclick = pickDirectory;
  el("settingsMask").addEventListener("click", (e) => {
    if (e.target === el("settingsMask")) closeSettings();
  });

  document.querySelector(".logs details").addEventListener("toggle", () => refresh());
}

function start() {
  bind();
  setupDragAndDrop();
  refresh();
  clearInterval(state.timer);
  // 轮询而非事件推送：状态总量小，且让「引擎崩溃/重连」的异常路径无需额外状态机。
  state.timer = setInterval(refresh, POLL_INTERVAL_MS);
  setInterval(watchClipboard, 1500);
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", start);
} else {
  start();
}

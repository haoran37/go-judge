const app = document.getElementById("app");

const routes = new Set([
  "/",
  "/setup-password",
  "/login",
  "/configure",
  "/configure/formal",
  "/configure/temp",
  "/dashboard",
  "/operations",
  "/logs",
  "/cache",
]);

const routeNames = {
  "/dashboard": "概览",
  "/configure": "配置",
  "/configure/formal": "正式节点配置",
  "/configure/temp": "临时节点配置",
  "/operations": "操作",
  "/logs": "日志",
  "/cache": "缓存",
};

let setup = null;
let runtime = {};
let currentConfig = null;

async function api(path, options = {}) {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  if (!res.ok) {
    throw new Error((await res.text()).trim() || res.statusText);
  }
  return res.json();
}

function navigate(path, replace = false) {
  if (location.pathname !== path) {
    history[replace ? "replaceState" : "pushState"]({}, "", path);
  }
  render().catch(showFatal);
}

window.addEventListener("popstate", () => render().catch(showFatal));

function showFatal(err) {
  app.innerHTML = `
    <main class="auth-shell">
      <section class="auth-card">
        <img src="/hie.svg" class="auth-logo" alt="HnieOJ">
        <h1>控制台加载失败</h1>
        <p class="error">${escapeHTML(err.message)}</p>
      </section>
    </main>`;
}

async function loadSetupStatus() {
  setup = await api("/api/v1/setup/status");
  runtime = setup.runtime || {};
}

async function loadConfig() {
  if (!currentConfig) {
    currentConfig = await api("/api/v1/config");
  }
  return currentConfig;
}

async function loadSystemInfo() {
  try {
    return await api("/api/v1/system/info");
  } catch {
    return {};
  }
}

async function render() {
  await loadSetupStatus();
  let path = normalizePath(location.pathname);
  let notice = "";

  if (!routes.has(path)) {
    path = setup.configured ? "/dashboard" : "/configure";
    history.replaceState({}, "", path);
  }

  if (!setup.adminInitialized) {
    if (path !== "/setup-password") {
      if (path !== "/") {
        notice = `第一次登录需要先设置管理员密码，完成后才能访问“${routeLabel(path)}”。`;
      }
      history.replaceState({}, "", "/setup-password");
    }
    renderSetupPassword(notice);
    return;
  }

  if (!setup.authenticated) {
    if (path !== "/login") {
      if (path !== "/") {
        notice = `请先登录，然后再访问“${routeLabel(path)}”。`;
      }
      history.replaceState({}, "", "/login");
    }
    renderLogin(notice);
    return;
  }

  if (!setup.configured && !path.startsWith("/configure")) {
    if (path !== "/") {
      notice = `判题节点还没有完成配置，先完成配置后才能访问“${routeLabel(path)}”。`;
    }
    history.replaceState({}, "", "/configure");
    await renderAuthed("/configure", notice);
    return;
  }

  if (path === "/" || path === "/setup-password" || path === "/login") {
    path = setup.configured ? "/dashboard" : "/configure";
    history.replaceState({}, "", path);
  }

  await renderAuthed(normalizePath(location.pathname), "");
}

function normalizePath(path) {
  if (!path || path === "/index.html") return "/";
  return path.replace(/\/+$/, "") || "/";
}

function renderSetupPassword(notice) {
  app.innerHTML = `
    <main class="auth-shell">
      <section class="auth-card setup-card">
        <div class="auth-title">
          <img src="/hie.svg" class="auth-logo" alt="HnieOJ">
          <div>
            <h1>第一次登录需要先设置管理员密码</h1>
            <p>这是当前判题机 WebUI 的本地管理员密码，不是 HnieOJ 后端账号密码。</p>
          </div>
        </div>

        <div class="setup-guide">
          <strong>你现在要做什么？</strong>
          <ol>
            <li>创建本机 WebUI 管理员密码。</li>
            <li>登录控制台。</li>
            <li>选择正式节点或临时节点，完成连接配置。</li>
          </ol>
        </div>

        <form id="setup-form" class="auth-form">
          <div class="field">
            <label for="password">新管理员密码</label>
            <input id="password" type="password" autocomplete="new-password" placeholder="至少 8 位" autofocus>
          </div>
          <button class="primary wide" type="submit">设置管理员密码</button>
          <div id="message" class="message" role="status"></div>
        </form>
      </section>
      ${topAlert(notice, "warn")}
    </main>`;
  document.getElementById("setup-form").onsubmit = async (event) => {
    event.preventDefault();
    await submitWithMessage("message", async () => {
      await api("/api/v1/setup/admin", {
        method: "POST",
        body: JSON.stringify({ password: value("password") }),
      });
      currentConfig = null;
      navigate("/configure", true);
    });
  };
}

function renderLogin(notice) {
  app.innerHTML = `
    <main class="auth-shell">
      <section class="auth-card">
        <div class="auth-title">
          <img src="/hie.svg" class="auth-logo" alt="HnieOJ">
          <div>
            <h1>登录判题机控制台</h1>
            <p>输入初始化时创建的本地管理员密码。登录有效期为 2 小时。</p>
          </div>
        </div>
        <form id="login-form" class="auth-form">
          <div class="field">
            <label for="password">管理员密码</label>
            <input id="password" type="password" autocomplete="current-password" autofocus>
          </div>
          <button class="primary wide" type="submit">登录</button>
          <div id="message" class="message" role="status"></div>
        </form>
      </section>
      ${topAlert(notice, "warn")}
    </main>`;
  document.getElementById("login-form").onsubmit = async (event) => {
    event.preventDefault();
    await submitWithMessage("message", async () => {
      await api("/api/v1/auth/login", {
        method: "POST",
        body: JSON.stringify({ password: value("password") }),
      });
      await loadSetupStatus();
      navigate(setup.configured ? "/dashboard" : "/configure", true);
    });
  };
}

async function renderAuthed(path, notice) {
  switch (path) {
    case "/configure":
      renderShell("configure", "节点配置", configureChoiceHTML(), notice);
      bindConfigureChoice();
      break;
    case "/configure/formal":
      renderShell("configure", "正式节点配置", configFormHTML("formal", await loadConfig()), notice);
      bindConfigForm("formal");
      break;
    case "/configure/temp":
      renderShell("configure", "临时节点配置", configFormHTML("temp", await loadConfig()), notice);
      bindConfigForm("temp");
      break;
    case "/operations":
      renderShell("operations", "运行操作", operationsHTML(), notice);
      bindOperations();
      break;
    case "/logs":
      renderShell("logs", "运行日志", logsHTML(), notice);
      await loadLogs();
      break;
    case "/cache":
      renderShell("cache", "测试数据缓存", cacheHTML(), notice);
      await loadCache();
      break;
    case "/dashboard":
    default:
      renderShell("dashboard", "节点概览", dashboardHTML(await loadSystemInfo()), notice);
      break;
  }
}

function renderShell(active, title, content, notice = "") {
  app.innerHTML = `
    <div class="app-shell">
      <aside class="sidebar">
        <div class="brand">
          <img src="/hie.svg" alt="HnieOJ">
          <div>
            <strong>HnieOJ Judge</strong>
            <span>本地控制台</span>
          </div>
        </div>
        <nav class="nav" aria-label="主导航">
          ${navLink("/dashboard", "概览", active === "dashboard")}
          ${navLink("/configure", "配置", active === "configure")}
          ${navLink("/operations", "操作", active === "operations")}
          ${navLink("/logs", "日志", active === "logs")}
          ${navLink("/cache", "缓存", active === "cache")}
        </nav>
        <div class="sidebar-bottom">
          <span class="state-pill ${stateClass(runtime.state)}">${stateText(runtime.state)}</span>
          <button id="logout" class="ghost">退出登录</button>
        </div>
      </aside>
      <main class="content">
        <header class="page-header">
          <div>
            <h1>${escapeHTML(title)}</h1>
            <p>${escapeHTML(headerSubtitle(active))}</p>
          </div>
          <button id="refresh">刷新</button>
        </header>
        ${content}
      </main>
      ${topAlert(notice, "warn")}
    </div>`;

  document.querySelectorAll("[data-link]").forEach((link) => {
    link.addEventListener("click", (event) => {
      event.preventDefault();
      navigate(link.getAttribute("href"));
    });
  });
  document.getElementById("refresh").onclick = () => {
    currentConfig = null;
    render().catch(showFatal);
  };
  document.getElementById("logout").onclick = async () => {
    await api("/api/v1/auth/logout", { method: "POST" });
    currentConfig = null;
    navigate("/login", true);
  };
}

function navLink(path, label, active) {
  return `<a href="${path}" data-link class="${active ? "active" : ""}">${label}</a>`;
}

function configureChoiceHTML() {
  return `
    <section class="panel">
      <div class="choice-grid">
        <button id="choose-formal" class="choice">
          <strong>正式节点</strong>
          <span>长期运行的生产判题节点。输入管理员签发的一次性 Bootstrap，节点生成独立 Ed25519 身份并完成挑战注册。</span>
        </button>
        <button id="choose-temp" class="choice">
          <strong>临时节点</strong>
          <span>临时扩容节点。同样使用独立 Ed25519 身份 + 一次性 Bootstrap，授权到期后凭密钥重新认证，无需重新注册。</span>
        </button>
      </div>
    </section>`;
}

function bindConfigureChoice() {
  document.getElementById("choose-formal").onclick = () => navigate("/configure/formal");
  document.getElementById("choose-temp").onclick = () => navigate("/configure/temp");
}

function dashboardHTML(system) {
  const metrics = runtime.metrics || {};
  const recent = runtime.recentMetrics || [];
  return `
    <section class="metric-grid">
      ${metric("运行状态", stateText(runtime.state), stateClass(runtime.state))}
      ${metric("运行任务", runtime.runningTasks || 0)}
      ${metric("已完成", metrics.finishedTasks || 0)}
      ${metric("失败任务", metrics.failedTasks || 0, metrics.failedTasks ? "error" : "")}
    </section>
    <section class="panel">
      <h2>近期任务量</h2>
      ${lineChart(recent)}
    </section>
    <section class="dashboard-grid">
      <section class="panel">
        <h2>判题结果占比</h2>
        ${pieChart(metrics)}
      </section>
      <section class="panel">
        <h2>系统信息</h2>
        <div class="kv-grid">
          ${kv("CPU 核数", system.cpuCore || "-")}
          ${kv("Go 协程", system.goRoutines || "-")}
          ${kv("进程内存", formatBytes(system.processAllocBytes || 0))}
          ${kv("系统内存", memoryText(system.memoryTotalBytes, system.memoryFreeBytes))}
          ${kv("磁盘可用", diskText(system.diskTotalBytes, system.diskFreeBytes))}
        </div>
      </section>
    </section>
    <section class="panel">
      <h2>节点信息</h2>
      <div class="kv-grid">
        ${kv("节点名称", runtime.nodeName || "未配置")}
        ${kv("节点类型", nodeTypeText(runtime.nodeType))}
        ${kv("节点 ID", runtime.nodeId || "未登记")}
        ${kv("当前 Key ID", runtime.keyId || "未登记")}
        ${kv("Session Epoch", runtime.sessionEpoch || 0)}
        ${kv("待确认结果", runtime.pendingResults || 0)}
        ${kv("是否排空", runtime.draining ? "是" : "否")}
        ${kv("启动时间", formatTime(runtime.startedAt))}
        ${kv("最近错误", runtime.lastError || "无", runtime.lastError ? "error" : "ok")}
      </div>
    </section>`;
}

function operationsHTML() {
  return `
    <section class="panel">
      <h2>判题服务</h2>
      <div class="button-row">
        <button id="start" class="primary">启动</button>
        <button id="restart">重启</button>
        <button id="stop" class="danger">停止</button>
      </div>
      <div id="operation-message" class="message" role="status"></div>
    </section>
    <section class="panel">
      <h2>当前状态</h2>
      <div class="kv-grid">
        ${kv("状态", stateText(runtime.state))}
        ${kv("运行任务", runtime.runningTasks || 0)}
        ${kv("最近错误", runtime.lastError || "无", runtime.lastError ? "error" : "ok")}
      </div>
    </section>`;
}

function bindOperations() {
  document.getElementById("start").onclick = () => runtimeAction("/api/v1/runtime/start");
  document.getElementById("stop").onclick = () => runtimeAction("/api/v1/runtime/stop");
  document.getElementById("restart").onclick = () => runtimeAction("/api/v1/runtime/restart");
}

async function runtimeAction(path) {
  await submitWithMessage("operation-message", async () => {
    runtime = await api(path, { method: "POST" });
    await render();
  }, "操作已提交");
}

function configFormHTML(mode, cfg) {
  const c = normalizedConfig(cfg);
  return `
    <form id="config-form" class="config-form">
      <section class="panel">
        <h2>基础配置</h2>
        <div class="form-grid two">
          ${field("节点名称", "node-name", c.node.name)}
          ${field("最大并发", "max-concurrency", c.node.maxConcurrency, "number")}
          ${field("HnieOJ 后端地址", "base-url", c.hnieoj.baseUrl, "text", "https://oj.example.com")}
          ${field("WSS 任务通道（留空自动推导）", "wss-url", c.hnieoj.wssUrl, "text", "wss://oj.example.com/ws/judge/node")}
          ${field("签名 audience（留空取 host）", "audience", c.hnieoj.audience)}
          ${judgeModeCheckboxes(c.node.supportedJudgeModes)}
        </div>
      </section>
      ${bootstrapHTML(c, mode)}
      <section class="panel">
        <h2>身份与自动轮换</h2>
        <div class="form-grid two">
          ${field("身份文件", "identity-file", c.identity.file)}
          ${field("状态目录", "identity-state-dir", c.identity.stateDir)}
          ${field("自动轮换周期", "rotation-interval", c.rotation.interval || "720h")}
          ${field("旧密钥 grace", "rotation-grace", c.rotation.grace || "5m")}
        </div>
        <p class="hint">身份文件含本地私钥，POSIX 权限 0700/0600，Windows 使用 current-user-only ACL；绝不进入沙箱或对外返回。</p>
      </section>
      <section class="panel">
        <h2>任务执行</h2>
        <div class="form-grid two">
          ${field("空队列最小退避", "worker-empty-min-backoff", c.worker.emptyMinBackoff || "200ms")}
          ${field("空队列最大退避", "worker-empty-max-backoff", c.worker.emptyMaxBackoff || "5s")}
          ${field("排空超时", "worker-drain-timeout", c.worker.drainTimeout || "5m")}
        </div>
      </section>
      ${cacheConfigHTML(c)}
      <section class="form-footer">
        <button class="primary" type="submit">保存并完成入网配置</button>
        <button id="back-config" type="button">返回</button>
        <div id="config-message" class="message" role="status"></div>
      </section>
    </form>`;
}

function bootstrapHTML(cfg, mode) {
  const identity = cfg.identity || {};
  const configured = identity.bootstrapConfigured
    ? "本地已保存一次性 Bootstrap（注册成功后自动删除）"
    : "尚未提供 Bootstrap";
  const publicIdentity = runtime.identity || {};
  return `
    <section class="panel">
      <h2>${mode === "temp" ? "临时节点入网" : "正式节点入网"}</h2>
      <div class="form-grid">
        <div class="field">
          <label for="bootstrap-token">一次性 Bootstrap</label>
          <input id="bootstrap-token" type="password" placeholder="仅首次入网需要；不会回显，也不会写入 config.yaml">
        </div>
      </div>
      <div class="token-summary">
        ${kv("Bootstrap 状态", configured)}
        ${kv("节点 ID", publicIdentity.nodeId || "未登记")}
        ${kv("Key ID", publicIdentity.keyId || "未登记")}
        ${kv("轮换状态", publicIdentity.rotationStatus || "无待处理轮换")}
      </div>
      <p class="hint">节点首次启动即生成真实随机 Ed25519 keypair 与 enrollmentId 并原子持久化；重启复用同一身份，不消耗新 Bootstrap。</p>
    </section>`;
}

function cacheConfigHTML(cfg) {
  const testdata = cfg.testdata || {};
  return `
    <section class="panel">
      <h2>测试数据缓存</h2>
      <div class="form-grid two">
        ${field("缓存目录", "cache-root", testdata.cacheRoot || "/data/oj/judge-cache")}
        ${field("最大缓存大小 GiB", "cache-max-gib", bytesToGiB(testdata.maxCacheBytes || 0), "number")}
        ${field("最大未使用时间", "cache-max-unused", testdata.maxUnusedDuration || "72h")}
        ${field("清理间隔", "cache-cleanup-interval", testdata.cleanupInterval || "1h")}
        ${field("统计刷新间隔", "cache-stats-interval", testdata.statsInterval || "5m")}
      </div>
    </section>`;
}

function judgeModeCheckboxes(selectedModes = []) {
  const selected = new Set(selectedModes.length ? selectedModes : ["default"]);
  const options = [
    ["default", "普通题"],
    ["spj", "SPJ"],
    ["interactive", "交互题"],
  ];
  return `
    <div class="field">
      <label>判题模式</label>
      <div class="checkbox-group">
        ${options.map(([value, label]) => `
          <label class="checkbox">
            <input type="checkbox" name="judge-mode" value="${value}" ${selected.has(value) ? "checked" : ""}>
            <span>${label}</span>
          </label>`).join("")}
      </div>
    </div>`;
}

function bindConfigForm(mode) {
  document.getElementById("back-config").onclick = () => navigate("/configure");
  document.getElementById("config-form").onsubmit = async (event) => {
    event.preventDefault();
    await submitWithMessage("config-message", async () => {
      const bootstrapToken = value("bootstrap-token");
      const result = await api("/api/v1/setup/bootstrap", {
        method: "POST",
        body: JSON.stringify({ config: formConfig(mode, bootstrapToken) }),
      });
      currentConfig = result.config || null;
      await loadSetupStatus();
      navigate("/dashboard", true);
    }, "配置已保存；启动判题服务后将用本地 Ed25519 身份完成挑战注册");
  };
}

function formConfig(mode, bootstrapToken = "") {
  const maxConcurrency = Number(value("max-concurrency") || 1);
  return {
    node: {
      name: value("node-name"),
      type: mode,
      maxConcurrency,
      supportedJudgeModes: selectedJudgeModes(),
    },
    hnieoj: {
      baseUrl: value("base-url"),
      wssUrl: value("wss-url"),
      audience: value("audience"),
      requestTimeout: "30s",
    },
    identity: {
      file: value("identity-file"),
      stateDir: value("identity-state-dir"),
      bootstrapToken,
    },
    rotation: {
      enabled: true,
      interval: value("rotation-interval") || "720h",
      grace: value("rotation-grace") || "5m",
      confirmTimeout: "30s",
    },
    testdata: {
      cacheRoot: value("cache-root") || "/data/oj/judge-cache",
      maxCacheBytes: giBToBytes(value("cache-max-gib") || "20"),
      maxUnusedDuration: value("cache-max-unused") || "72h",
      cleanupInterval: value("cache-cleanup-interval") || "1h",
      statsInterval: value("cache-stats-interval") || "5m",
    },
    gojudge: { endpoint: "http://127.0.0.1:5050" },
    worker: {
      emptyMinBackoff: value("worker-empty-min-backoff") || "200ms",
      emptyMaxBackoff: value("worker-empty-max-backoff") || "5s",
      drainTimeout: value("worker-drain-timeout") || "5m",
    },
  };
}

function logsHTML() {
  return `<section class="panel"><h2>最近日志</h2><div id="logs" class="logs">正在加载日志</div></section>`;
}

async function loadLogs() {
  const logs = await api("/api/v1/logs/recent");
  const target = document.getElementById("logs");
  target.innerHTML = Array.isArray(logs) && logs.length
    ? logs.map((item) => `
      <div class="log-row">
        <span>${escapeHTML(formatTime(item.time))}</span>
        <strong class="${levelClass(item.level)}">${escapeHTML(item.level || "-")}</strong>
        <span>${escapeHTML(item.message || "")}</span>
      </div>`).join("")
    : `<p>暂无日志</p>`;
}

function cacheHTML() {
  return `
    <section class="panel">
      <div class="button-row">
        <button id="refresh-cache">刷新缓存</button>
        <button id="cleanup-cache" class="danger">按策略清理</button>
      </div>
      <div id="cache-message" class="message" role="status"></div>
      <div id="cache-list" class="cache-list">正在加载缓存</div>
    </section>`;
}

async function loadCache() {
  const data = await api("/api/v1/testdata/cache");
  const items = data.items || [];
  document.getElementById("cache-list").innerHTML = items.length ? `
    <table class="cache-table">
      <thead><tr><th>题目 ID</th><th>版本</th><th>大小</th><th>最近使用</th><th>操作</th></tr></thead>
      <tbody>
        ${items.map((item) => `
          <tr>
            <td>${item.problemId}</td>
            <td>${item.version || "-"}</td>
            <td>${formatBytes(item.sizeBytes || 0)}</td>
            <td>${formatTime(item.lastUsed)}</td>
            <td><button data-delete-cache="${item.problemId}" class="danger">删除</button></td>
          </tr>`).join("")}
      </tbody>
    </table>` : `<p>暂无已缓存测试数据</p>`;
  document.getElementById("refresh-cache").onclick = () => loadCache().catch(showFatal);
  document.getElementById("cleanup-cache").onclick = async () => {
    await submitWithMessage("cache-message", async () => {
      const result = await api("/api/v1/testdata/cache/cleanup", { method: "POST" });
      await loadCache();
      return result;
    }, "缓存清理完成");
  };
  document.querySelectorAll("[data-delete-cache]").forEach((button) => {
    button.onclick = async () => {
      await submitWithMessage("cache-message", async () => {
        await api(`/api/v1/testdata/cache/${button.dataset.deleteCache}`, { method: "DELETE" });
        await loadCache();
      }, "缓存已删除");
    };
  });
}

function normalizedConfig(cfg = {}) {
  const fallback = {
    node: { name: "judge-node-01", maxConcurrency: 2, supportedJudgeModes: ["default"] },
    hnieoj: { baseUrl: "", wssUrl: "", audience: "" },
    identity: { file: "/var/lib/hnieoj-judge-node/identity.json", stateDir: "/var/lib/hnieoj-judge-node", bootstrapConfigured: false },
    rotation: { enabled: true, interval: "720h", grace: "5m", confirmTimeout: "30s" },
    worker: { emptyMinBackoff: "200ms", emptyMaxBackoff: "5s", drainTimeout: "5m" },
    testdata: { cacheRoot: "/data/oj/judge-cache", maxCacheBytes: 21474836480, maxUnusedDuration: "72h", cleanupInterval: "1h", statsInterval: "5m" },
  };
  return {
    ...fallback,
    ...cfg,
    node: { ...fallback.node, ...(cfg.node || {}) },
    hnieoj: { ...fallback.hnieoj, ...(cfg.hnieoj || {}) },
    identity: { ...fallback.identity, ...(cfg.identity || {}) },
    rotation: { ...fallback.rotation, ...(cfg.rotation || {}) },
    worker: { ...fallback.worker, ...(cfg.worker || {}) },
    testdata: { ...fallback.testdata, ...(cfg.testdata || {}) },
  };
}

function lineChart(points = []) {
  const width = 720;
  const height = 220;
  const padding = 24;
  const values = points.map((point) => ({
    started: Number(point.startedTasks || 0),
    finished: Number(point.finishedTasks || 0),
    failed: Number(point.failedTasks || 0),
  }));
  const max = Math.max(1, ...values.flatMap((item) => [item.started, item.finished, item.failed]));
  const x = (index) => padding + (index * (width - padding * 2)) / Math.max(1, values.length - 1);
  const y = (value) => height - padding - (value * (height - padding * 2)) / max;
  const path = (key) => values.map((item, index) => `${index === 0 ? "M" : "L"}${x(index).toFixed(1)},${y(item[key]).toFixed(1)}`).join(" ");
  return `
    <div class="chart-wrap">
      <svg viewBox="0 0 ${width} ${height}" class="line-chart" role="img" aria-label="近期任务量">
        <line x1="${padding}" y1="${height - padding}" x2="${width - padding}" y2="${height - padding}" />
        <path class="started" d="${path("started")}"></path>
        <path class="finished" d="${path("finished")}"></path>
        <path class="failed" d="${path("failed")}"></path>
      </svg>
      <div class="chart-legend">
        <span class="started">进入</span><span class="finished">完成</span><span class="failed">失败</span>
      </div>
    </div>`;
}

function pieChart(metrics = {}) {
  const finished = Number(metrics.finishedTasks || 0);
  const failed = Number(metrics.failedTasks || 0);
  const retryable = Number(metrics.retryableTasks || 0);
  const total = Math.max(1, finished + failed + retryable);
  const successDeg = (finished / total) * 360;
  const failedDeg = (failed / total) * 360;
  return `
    <div class="pie-row">
      <div class="pie" style="background: conic-gradient(var(--ok) 0 ${successDeg}deg, var(--danger) ${successDeg}deg ${successDeg + failedDeg}deg, var(--warning) ${successDeg + failedDeg}deg 360deg)"></div>
      <div class="pie-legend">
        ${kv("成功", finished)}
        ${kv("失败", failed)}
        ${kv("可重试", retryable)}
      </div>
    </div>`;
}

function metric(label, metricValue, className = "") {
  return `<article class="metric ${className}"><span>${escapeHTML(label)}</span><strong>${escapeHTML(String(metricValue))}</strong></article>`;
}

function kv(label, kvValue, className = "") {
  return `<div class="kv"><span>${escapeHTML(label)}</span><strong class="${className}">${escapeHTML(String(kvValue))}</strong></div>`;
}

function field(label, id, fieldValue = "", type = "text", placeholder = "") {
  return `
    <div class="field">
      <label for="${id}">${escapeHTML(label)}</label>
      <input id="${id}" type="${type}" value="${escapeAttr(String(fieldValue ?? ""))}" placeholder="${escapeAttr(placeholder)}">
    </div>`;
}

function value(id) {
  return document.getElementById(id)?.value.trim() || "";
}

function formatBytes(bytes) {
  const value = Number(bytes || 0);
  if (value < 1024) return `${value} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let size = value / 1024;
  for (const unit of units) {
    if (size < 1024) return `${Math.round(size * 10) / 10} ${unit}`;
    size /= 1024;
  }
  return `${Math.round(size * 10) / 10} PiB`;
}

function memoryText(total, free) {
  if (!total) return "不可用";
  return `${formatBytes(total - free)} / ${formatBytes(total)}`;
}

function diskText(total, free) {
  if (!total) return "不可用";
  return `${formatBytes(free)} 可用 / ${formatBytes(total)}`;
}

function selectedJudgeModes() {
  const modes = Array.from(document.querySelectorAll('input[name="judge-mode"]:checked')).map((item) => item.value);
  return modes.length ? modes : ["default"];
}

function bytesToGiB(bytes) {
  if (!bytes) return 20;
  return Math.round((Number(bytes) / 1024 / 1024 / 1024) * 10) / 10;
}

function giBToBytes(value) {
  const number = Number(value);
  if (!Number.isFinite(number) || number <= 0) return 0;
  return Math.round(number * 1024 * 1024 * 1024);
}

async function submitWithMessage(messageID, action, success = "") {
  const message = document.getElementById(messageID);
  if (message) {
    message.textContent = "处理中...";
    message.classList.remove("error", "ok");
  }
  try {
    await action();
    if (message && success) {
      message.textContent = success;
      message.classList.add("ok");
    }
  } catch (err) {
    showRuntimeAlert(err.message, "error");
    if (message) {
      message.textContent = err.message;
      message.classList.add("error");
    }
  }
}

function topAlert(message, type = "warn", extraClass = "") {
  if (!message) return "";
  return `
    <div class="top-alert ${type} ${extraClass}" role="status">
      <strong>${type === "error" ? "错误" : "提示"}</strong>
      <span>${escapeHTML(message)}</span>
    </div>`;
}

function showRuntimeAlert(message, type = "error") {
  document.querySelectorAll(".top-alert.runtime").forEach((item) => item.remove());
  document.body.insertAdjacentHTML("beforeend", topAlert(message, type, "runtime"));
  const alert = document.querySelector(".top-alert.runtime");
  if (alert) {
    setTimeout(() => alert.remove(), 5000);
  }
}

function routeLabel(path) {
  return routeNames[path] || "控制台页面";
}

function headerSubtitle(active) {
  const map = {
    dashboard: "查看判题节点当前运行状态和任务统计。",
    configure: "配置正式节点或临时节点的连接信息。",
    operations: "启动、停止或重启容器内判题服务。",
    logs: "查看 WebUI 记录的最近运行日志。",
    cache: "查看、清理和配置本地测试数据缓存。",
  };
  return map[active] || "";
}

function stateText(state) {
  const map = {
    stopped: "已停止",
    starting: "启动中",
    running: "运行中",
    stopping: "停止中",
    failed: "异常",
  };
  return map[state] || state || "未知";
}

function stateClass(state) {
  if (state === "running") return "ok";
  if (state === "failed") return "error";
  if (state === "starting" || state === "stopping") return "warn";
  return "";
}

function nodeTypeText(type) {
  if (type === "formal") return "正式节点";
  if (type === "temp") return "临时节点";
  return type || "-";
}

function levelClass(level = "") {
  return level.toLowerCase() === "warn" ? "warn" : "ok";
}

function formatTime(input) {
  if (!input) return "-";
  const date = new Date(input);
  if (Number.isNaN(date.getTime())) return "-";
  return date.toLocaleString();
}

function escapeHTML(input) {
  return String(input).replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[ch]));
}

function escapeAttr(input) {
  return escapeHTML(input).replace(/`/g, "&#96;");
}

render().catch(showFatal);

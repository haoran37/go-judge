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
]);

const routeNames = {
  "/dashboard": "概览",
  "/configure": "配置",
  "/configure/formal": "正式节点配置",
  "/configure/temp": "临时节点配置",
  "/operations": "操作",
  "/logs": "日志",
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
      notice = `第一次登录需要先设置管理员密码，完成后才能访问“${routeLabel(path)}”。`;
      history.replaceState({}, "", "/setup-password");
    }
    renderSetupPassword(notice);
    return;
  }

  if (!setup.authenticated) {
    if (path !== "/login") {
      notice = `请先登录，然后再访问“${routeLabel(path)}”。`;
      history.replaceState({}, "", "/login");
    }
    renderLogin(notice);
    return;
  }

  if (!setup.configured && !path.startsWith("/configure")) {
    notice = `判题节点还没有完成配置，先完成配置后才能访问“${routeLabel(path)}”。`;
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
      ${noticeDialog(notice)}
    </main>`;
  bindNoticeDialog();
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
      ${noticeDialog(notice)}
    </main>`;
  bindNoticeDialog();
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
    case "/dashboard":
    default:
      renderShell("dashboard", "节点概览", dashboardHTML(), notice);
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
      ${noticeDialog(notice)}
    </div>`;

  bindNoticeDialog();
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
          <span>用于长期运行的生产判题节点。需要上传正式节点私钥。</span>
        </button>
        <button id="choose-temp" class="choice">
          <strong>临时节点</strong>
          <span>用于临时扩容。需要输入后端发放的临时授权码并兑换 JWT。</span>
        </button>
      </div>
    </section>`;
}

function bindConfigureChoice() {
  document.getElementById("choose-formal").onclick = () => navigate("/configure/formal");
  document.getElementById("choose-temp").onclick = () => navigate("/configure/temp");
}

function dashboardHTML() {
  const metrics = runtime.metrics || {};
  return `
    <section class="metric-grid">
      ${metric("运行状态", stateText(runtime.state), stateClass(runtime.state))}
      ${metric("运行任务", runtime.runningTasks || 0)}
      ${metric("已完成", metrics.finishedTasks || 0)}
      ${metric("失败任务", metrics.failedTasks || 0, metrics.failedTasks ? "error" : "")}
    </section>
    <section class="panel">
      <h2>节点信息</h2>
      <div class="kv-grid">
        ${kv("节点名称", runtime.nodeName || "未配置")}
        ${kv("节点类型", nodeTypeText(runtime.nodeType))}
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
          ${field("HnieOJ 后端地址", "base-url", c.hnieoj.baseUrl, "text", "http://gateway:8800")}
          ${field("判题模式", "judge-modes", c.node.supportedJudgeModes.join(","))}
          ${field("心跳间隔", "heartbeat-interval", c.heartbeat.interval || "30s")}
        </div>
      </section>
      <section class="panel">
        <h2>RabbitMQ</h2>
        <div class="form-grid two">
          ${field("主机", "rabbit-host", c.rabbitmq.host)}
          ${field("端口", "rabbit-port", c.rabbitmq.port, "number")}
          ${field("用户", "rabbit-user", c.rabbitmq.username)}
          ${field("密码", "rabbit-password", "", "password", c.rabbitmq.passwordConfigured ? "留空保持原密码" : "")}
          ${field("vhost", "rabbit-vhost", c.rabbitmq.virtualHost)}
        </div>
      </section>
      ${mode === "formal" ? formalKeyHTML(c) : tempAuthHTML(c)}
      <section class="form-footer">
        ${mode === "formal" ? `<button class="primary" type="submit">保存正式节点配置</button>` : tempButtonsHTML()}
        <button id="back-config" type="button">返回</button>
        <div id="config-message" class="message" role="status"></div>
      </section>
    </form>`;
}

function formalKeyHTML(cfg) {
  const placeholder = cfg.hnieoj.formalToken.privateKeyConfigured ? "已配置；留空保持原私钥" : "-----BEGIN PRIVATE KEY-----";
  return `
    <section class="panel">
      <h2>正式节点私钥</h2>
      <div class="form-grid">
        <div class="field">
          <label for="formal-private-key-file">上传 PEM 文件</label>
          <input id="formal-private-key-file" type="file" accept=".pem,.key,text/plain">
        </div>
        <div class="field">
          <label for="formal-private-key">PEM 内容</label>
          <textarea id="formal-private-key" placeholder="${escapeAttr(placeholder)}"></textarea>
        </div>
      </div>
    </section>`;
}

function tempAuthHTML(cfg) {
  const token = cfg.hnieoj.tempToken || {};
  return `
    <section class="panel">
      <h2>临时授权码兑换</h2>
      <div class="form-grid">
        <div class="field">
          <label for="temp-auth-code">临时授权码</label>
          <input id="temp-auth-code" type="password" placeholder="输入后点击下方兑换按钮">
        </div>
      </div>
      <div class="token-summary">
        ${kv("节点 ID", token.nodeId || "未兑换")}
        ${kv("Token ID", token.tokenId || "未兑换")}
        ${kv("过期时间", token.expireTime || "未兑换")}
      </div>
    </section>`;
}

function tempButtonsHTML() {
  const saveButton = setup.configured ? `<button id="save-temp-config" type="button">保存基础配置</button>` : "";
  return `${saveButton}<button id="exchange-token" class="primary" type="button">兑换临时授权码</button>`;
}

function bindConfigForm(mode) {
  document.getElementById("back-config").onclick = () => navigate("/configure");
  const keyFile = document.getElementById("formal-private-key-file");
  if (keyFile) {
    keyFile.onchange = async () => {
      const file = keyFile.files && keyFile.files[0];
      if (file) {
        document.getElementById("formal-private-key").value = await file.text();
      }
    };
  }
  if (mode === "formal") {
    document.getElementById("config-form").onsubmit = async (event) => {
      event.preventDefault();
      await submitWithMessage("config-message", async () => {
        await api("/api/v1/setup/formal", {
          method: "POST",
          body: JSON.stringify({ config: formConfig(mode), privateKeyPem: value("formal-private-key") }),
        });
        currentConfig = null;
        navigate("/dashboard", true);
      }, "配置已保存");
    };
    return;
  }

  document.getElementById("config-form").onsubmit = (event) => event.preventDefault();

  const saveButton = document.getElementById("save-temp-config");
  if (saveButton) {
    saveButton.onclick = async () => {
      await submitWithMessage("config-message", async () => {
        await api("/api/v1/config", {
          method: "PUT",
          body: JSON.stringify(formConfig("temp")),
        });
        currentConfig = null;
      }, "基础配置已保存，重启判题服务后生效");
    };
  }

  document.getElementById("exchange-token").onclick = async () => {
    await submitWithMessage("config-message", async () => {
      const authCode = value("temp-auth-code");
      if (!authCode) {
        throw new Error("请输入临时授权码");
      }
      const result = await api("/api/v1/setup/temp/exchange", {
        method: "POST",
        body: JSON.stringify({ config: formConfig("temp"), authCode }),
      });
      setup.configured = true;
      currentConfig = result.config || null;
      await loadSetupStatus();
      await renderAuthed("/configure/temp", "");
      const message = document.getElementById("config-message");
      if (message) {
        message.textContent = "临时授权码兑换成功，JWT 已写入本机配置";
        message.classList.add("ok");
      }
    });
  };
}

function formConfig(mode) {
  const maxConcurrency = Number(value("max-concurrency") || 1);
  return {
    node: {
      name: value("node-name"),
      type: mode,
      maxConcurrency,
      supportedJudgeModes: value("judge-modes").split(",").map((item) => item.trim()).filter(Boolean),
    },
    hnieoj: {
      baseUrl: value("base-url"),
      requestTimeout: "30s",
      formalToken: {
        cipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding",
        refreshInterval: "30s",
        nacos: {},
      },
      tempToken: { proofType: "hmac-sha256" },
    },
    rabbitmq: {
      host: value("rabbit-host"),
      port: Number(value("rabbit-port") || 5672),
      username: value("rabbit-user"),
      password: value("rabbit-password"),
      virtualHost: value("rabbit-vhost"),
      exchange: "hnieoj.judge.exchange",
      queue: "hnieoj.judge.task",
      routingKey: "judge.submission.created",
      deadLetterExchange: "hnieoj.judge.dlx",
      deadLetterQueue: "hnieoj.judge.task.dlq",
      deadLetterRoutingKey: "judge.submission.created.dlq",
      prefetch: maxConcurrency,
      maxRetries: 3,
      retryBackoff: "10s",
    },
    testdata: {
      cacheRoot: "/data/oj/judge-cache",
      maxCacheBytes: 21474836480,
      maxUnusedDuration: "72h",
      cleanupInterval: "1h",
      statsInterval: "5m",
    },
    gojudge: { endpoint: "http://127.0.0.1:5050" },
    reporter: { mode: "http", endpoint: "/judge/submissions/{submissionId}/events" },
    heartbeat: { enabled: true, endpoint: "/judge/nodes/heartbeat", interval: value("heartbeat-interval") || "30s" },
    remoteConfig: { enabled: false, nacos: {} },
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

function normalizedConfig(cfg = {}) {
  const fallback = {
    node: { name: "judge-node-01", maxConcurrency: 2, supportedJudgeModes: ["default"] },
    hnieoj: { baseUrl: "", formalToken: {}, tempToken: {} },
    rabbitmq: { host: "rabbitmq", port: 5672, username: "hnieoj_judge", virtualHost: "hnieoj" },
    heartbeat: { interval: "30s" },
  };
  return {
    ...fallback,
    ...cfg,
    node: { ...fallback.node, ...(cfg.node || {}) },
    hnieoj: {
      ...fallback.hnieoj,
      ...(cfg.hnieoj || {}),
      formalToken: { ...(cfg.hnieoj?.formalToken || {}) },
      tempToken: { ...(cfg.hnieoj?.tempToken || {}) },
    },
    rabbitmq: { ...fallback.rabbitmq, ...(cfg.rabbitmq || {}) },
    heartbeat: { ...fallback.heartbeat, ...(cfg.heartbeat || {}) },
  };
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
    if (message) {
      message.textContent = err.message;
      message.classList.add("error");
    }
  }
}

function noticeDialog(message) {
  if (!message) return "";
  return `
    <div class="notice-backdrop">
      <section class="notice-dialog" role="alertdialog" aria-modal="true">
        <h2>需要先完成当前步骤</h2>
        <p>${escapeHTML(message)}</p>
        <button id="notice-close" class="primary">知道了</button>
      </section>
    </div>`;
}

function bindNoticeDialog() {
  const close = document.getElementById("notice-close");
  if (close) {
    close.onclick = () => document.querySelector(".notice-backdrop")?.remove();
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

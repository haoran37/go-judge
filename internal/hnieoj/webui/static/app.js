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
    <main class="auth-page">
      <section class="login-panel">
        <img src="/hie.svg" class="logo" alt="HnieOJ">
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
  const path = normalizePath(location.pathname);

  if (!routes.has(path)) {
    navigate(setup.configured ? "/dashboard" : "/configure", true);
    return;
  }

  if (!setup.adminInitialized) {
    if (path !== "/setup-password") {
      history.replaceState({}, "", "/setup-password");
    }
    renderSetupPassword();
    return;
  }

  if (!setup.authenticated) {
    if (path !== "/login") {
      history.replaceState({}, "", "/login");
    }
    renderLogin();
    return;
  }

  if (!setup.configured && !path.startsWith("/configure")) {
    history.replaceState({}, "", "/configure");
    await renderAuthed("/configure");
    return;
  }

  if (path === "/" || path === "/setup-password" || path === "/login") {
    history.replaceState({}, "", setup.configured ? "/dashboard" : "/configure");
  }

  await renderAuthed(normalizePath(location.pathname));
}

function normalizePath(path) {
  if (!path || path === "/index.html") return "/";
  return path.replace(/\/+$/, "") || "/";
}

function renderSetupPassword() {
  app.innerHTML = `
    <main class="auth-page">
      <section class="login-panel">
        <div class="auth-head">
          <img src="/hie.svg" class="logo" alt="HnieOJ">
          <div>
            <h1>首次访问：创建本地管理员密码</h1>
            <p>这个密码只用于当前判题机 WebUI，不会写入 HnieOJ 后端。</p>
          </div>
        </div>
        <div class="guide">
          <h2>初始化流程</h2>
          <ol>
            <li>设置本地管理员密码。</li>
            <li>登录后选择正式节点或临时节点。</li>
            <li>填写 HnieOJ 后端、RabbitMQ 和节点参数，然后启动判题服务。</li>
          </ol>
        </div>
        <form id="setup-form" class="form-grid">
          <div class="field">
            <label for="password">管理员密码</label>
            <input id="password" type="password" autocomplete="new-password" placeholder="至少 8 位" autofocus>
            <span class="hint">建议使用只在本机保存的独立密码。</span>
          </div>
          <button class="primary" type="submit">创建密码并继续</button>
          <div id="message" class="message" role="status"></div>
        </form>
      </section>
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

function renderLogin() {
  app.innerHTML = `
    <main class="auth-page">
      <section class="login-panel compact">
        <div class="auth-head">
          <img src="/hie.svg" class="logo" alt="HnieOJ">
          <div>
            <h1>登录 HnieOJ 判题机控制台</h1>
            <p>登录有效期：2 小时。</p>
          </div>
        </div>
        <form id="login-form" class="form-grid">
          <div class="field">
            <label for="password">管理员密码</label>
            <input id="password" type="password" autocomplete="current-password" autofocus>
          </div>
          <button class="primary" type="submit">登录</button>
          <div id="message" class="message" role="status"></div>
        </form>
      </section>
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

async function renderAuthed(path) {
  switch (path) {
    case "/configure":
      renderShell("configure", "配置", configureChoiceHTML());
      bindConfigureChoice();
      break;
    case "/configure/formal":
      renderShell("configure", "正式节点配置", configFormHTML("formal", await loadConfig()));
      bindConfigForm("formal");
      break;
    case "/configure/temp":
      renderShell("configure", "临时节点配置", configFormHTML("temp", await loadConfig()));
      bindConfigForm("temp");
      break;
    case "/operations":
      renderShell("operations", "操作", operationsHTML());
      bindOperations();
      break;
    case "/logs":
      renderShell("logs", "日志", logsHTML());
      await loadLogs();
      break;
    case "/dashboard":
    default:
      renderShell("dashboard", "概览", dashboardHTML());
      break;
  }
}

function renderShell(active, title, content) {
  app.innerHTML = `
    <div class="console">
      <header class="topbar">
        <div class="brand">
          <img src="/hie.svg" alt="HnieOJ">
          <div>
            <strong>HnieOJ Judge</strong>
            <span>本地管理控制台</span>
          </div>
        </div>
        <div class="top-actions">
          <span class="status ${stateClass(runtime.state)}">状态：${stateText(runtime.state)}</span>
          <button id="refresh">刷新</button>
          <button id="logout">退出</button>
        </div>
      </header>
      <nav class="tabs" aria-label="主导航">
        ${tab("/dashboard", "概览", active === "dashboard")}
        ${tab("/configure", "配置", active === "configure")}
        ${tab("/operations", "操作", active === "operations")}
        ${tab("/logs", "日志", active === "logs")}
      </nav>
      <main class="content">
        <h1>${escapeHTML(title)}</h1>
        ${content}
      </main>
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

function tab(path, label, active) {
  return `<a href="${path}" data-link class="${active ? "active" : ""}">${label}</a>`;
}

function configureChoiceHTML() {
  return `
    <section class="panel">
      <h2>节点身份</h2>
      <table class="info-table">
        <tbody>
          <tr>
            <th>正式节点</th>
            <td>长期运行，使用正式节点私钥完成认证。</td>
            <td><button id="choose-formal" class="primary">配置正式节点</button></td>
          </tr>
          <tr>
            <th>临时节点</th>
            <td>临时扩容，使用后端发放的授权码兑换 JWT。</td>
            <td><button id="choose-temp">配置临时节点</button></td>
          </tr>
        </tbody>
      </table>
    </section>`;
}

function bindConfigureChoice() {
  document.getElementById("choose-formal").onclick = () => navigate("/configure/formal");
  document.getElementById("choose-temp").onclick = () => navigate("/configure/temp");
}

function dashboardHTML() {
  const metrics = runtime.metrics || {};
  return `
    <section class="panel">
      <h2>节点状态</h2>
      <table class="info-table">
        <tbody>
          ${row("运行状态", stateText(runtime.state))}
          ${row("节点名称", runtime.nodeName || "未配置")}
          ${row("节点类型", nodeTypeText(runtime.nodeType))}
          ${row("运行任务", runtime.runningTasks || 0)}
          ${row("启动时间", formatTime(runtime.startedAt))}
          ${row("停止时间", formatTime(runtime.stoppedAt))}
          ${row("最近错误", runtime.lastError || "无", runtime.lastError ? "error" : "ok")}
        </tbody>
      </table>
    </section>
    <section class="panel">
      <h2>任务统计</h2>
      <table class="info-table metrics">
        <tbody>
          ${row("已开始", metrics.startedTasks || 0)}
          ${row("已完成", metrics.finishedTasks || 0)}
          ${row("失败", metrics.failedTasks || 0, metrics.failedTasks ? "error" : "")}
          ${row("可重试错误", metrics.retryableTasks || 0)}
        </tbody>
      </table>
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
      <table class="info-table">
        <tbody>
          ${row("状态", stateText(runtime.state))}
          ${row("运行任务", runtime.runningTasks || 0)}
          ${row("最近错误", runtime.lastError || "无", runtime.lastError ? "error" : "ok")}
        </tbody>
      </table>
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
    <form id="config-form">
      <section class="panel">
        <h2>基本信息</h2>
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
      ${mode === "formal" ? formalKeyHTML(c) : tempAuthHTML()}
      <section class="form-footer">
        <button class="primary" type="submit">${mode === "formal" ? "保存正式节点配置" : "兑换并保存临时节点配置"}</button>
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

function tempAuthHTML() {
  return `
    <section class="panel">
      <h2>临时授权</h2>
      <div class="field">
        <label for="temp-auth-code">临时授权码</label>
        <input id="temp-auth-code" type="password" placeholder="输入后会立即兑换 JWT">
      </div>
    </section>`;
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
  document.getElementById("config-form").onsubmit = async (event) => {
    event.preventDefault();
    await submitWithMessage("config-message", async () => {
      const cfg = formConfig(mode);
      if (mode === "formal") {
        await api("/api/v1/setup/formal", {
          method: "POST",
          body: JSON.stringify({ config: cfg, privateKeyPem: value("formal-private-key") }),
        });
      } else {
        await api("/api/v1/setup/temp/exchange", {
          method: "POST",
          body: JSON.stringify({ config: cfg, authCode: value("temp-auth-code") }),
        });
      }
      currentConfig = null;
      navigate("/dashboard", true);
    }, "配置已保存");
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

function row(label, rowValue, className = "") {
  return `<tr><th>${escapeHTML(label)}</th><td class="${className}">${escapeHTML(String(rowValue))}</td></tr>`;
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

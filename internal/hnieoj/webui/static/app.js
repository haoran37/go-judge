const app = document.getElementById("app");

let setup = null;
let runtime = null;
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
    const method = replace ? "replaceState" : "pushState";
    history[method]({}, "", path);
  }
  render().catch(showFatal);
}

window.addEventListener("popstate", () => render().catch(showFatal));

function showFatal(err) {
  app.innerHTML = `
    <main class="center-shell">
      <section class="auth-panel">
        <img src="/hie.svg" class="auth-logo" alt="HnieOJ">
        <h1>控制台加载失败</h1>
        <p>${escapeHTML(err.message)}</p>
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
    renderConfigureChoice();
    return;
  }

  if (path === "/" || path === "/setup-password" || path === "/login") {
    history.replaceState({}, "", setup.configured ? "/dashboard" : "/configure");
  }

  await renderAuthed(normalizePath(location.pathname));
}

function normalizePath(path) {
  if (!path || path === "/index.html") {
    return "/";
  }
  return path.replace(/\/+$/, "") || "/";
}

function renderSetupPassword() {
  app.innerHTML = `
    <main class="center-shell">
      <section class="auth-panel">
        <img src="/hie.svg" alt="HnieOJ" class="auth-logo">
        <p class="eyebrow">首次初始化</p>
        <h1>设置管理密码</h1>
        <p>完成前不会展示节点配置、运行状态或日志页面。</p>
        <div class="form-grid">
          <div class="field">
            <label for="password">管理密码</label>
            <input id="password" type="password" autocomplete="new-password" placeholder="至少 8 位">
          </div>
          <button id="submit" class="primary">创建管理密码</button>
          <div id="message" class="message" role="status"></div>
        </div>
      </section>
    </main>`;
  document.getElementById("submit").onclick = async () => {
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
    <main class="center-shell">
      <section class="auth-panel">
        <img src="/hie.svg" alt="HnieOJ" class="auth-logo">
        <p class="eyebrow">HnieOJ Judge WebUI</p>
        <h1>登录控制台</h1>
        <p>登录有效期为 2 小时。</p>
        <div class="form-grid">
          <div class="field">
            <label for="password">管理密码</label>
            <input id="password" type="password" autocomplete="current-password">
          </div>
          <button id="submit" class="primary">登录</button>
          <div id="message" class="message" role="status"></div>
        </div>
      </section>
    </main>`;
  document.getElementById("submit").onclick = async () => {
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
      renderShell("configure", "配置判题节点", "选择节点身份后填写连接信息。", configureChoiceHTML());
      bindConfigureChoice();
      break;
    case "/configure/formal":
      renderShell("configure", "配置正式节点", "上传正式节点私钥，并填写 HnieOJ 后端和 RabbitMQ 信息。", configFormHTML("formal", await loadConfig()));
      bindConfigForm("formal");
      break;
    case "/configure/temp":
      renderShell("configure", "配置临时节点", "输入临时授权码，控制台会实时兑换 JWT。", configFormHTML("temp", await loadConfig()));
      bindConfigForm("temp");
      break;
    case "/operations":
      renderShell("operations", "操作中心", "控制容器内判题服务的启动、停止和重启。", operationsHTML());
      bindOperations();
      break;
    case "/logs":
      renderShell("logs", "运行日志", "查看控制台记录的最近运行日志。", logsHTML());
      await loadLogs();
      break;
    case "/dashboard":
    default:
      renderShell("dashboard", "仪表盘", "查看节点状态、任务统计和最近错误。", dashboardHTML());
      break;
  }
}

function renderShell(active, title, subtitle, content) {
  app.innerHTML = `
    <div class="app-shell">
      <aside class="sidebar">
        <div class="brand">
          <img src="/hie.svg" alt="HnieOJ">
          <div>
            <strong>HnieOJ Judge</strong>
            <span>判题机控制台</span>
          </div>
        </div>
        <nav class="nav" aria-label="主导航">
          ${navLink("/configure", "配置", active === "configure")}
          ${navLink("/dashboard", "仪表盘", active === "dashboard")}
          ${navLink("/operations", "操作", active === "operations")}
          ${navLink("/logs", "日志", active === "logs")}
        </nav>
        <div class="sidebar-footer">
          <span class="badge ${stateClass(runtime.state)}">${stateText(runtime.state)}</span>
          <button id="logout" class="ghost">退出登录</button>
        </div>
      </aside>
      <main class="content">
        <header class="page-header">
          <div>
            <h1>${escapeHTML(title)}</h1>
            <p class="page-subtitle">${escapeHTML(subtitle)}</p>
          </div>
          <div class="actions">
            <button id="refresh">刷新</button>
          </div>
        </header>
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

function navLink(path, label, active) {
  return `<a href="${path}" data-link class="${active ? "active" : ""}">${label}</a>`;
}

function configureChoiceHTML() {
  return `
    <section class="grid two">
      <button id="choose-formal" class="choice">
        <span class="choice-index">01</span>
        <strong>正式节点</strong>
        <span>用于长期运行的生产判题节点，适合固定服务器和稳定网络环境。</span>
      </button>
      <button id="choose-temp" class="choice">
        <span class="choice-index">02</span>
        <strong>临时节点</strong>
        <span>用于短期扩容或临时接入，通过授权码完成首次认证。</span>
      </button>
    </section>`;
}

function bindConfigureChoice() {
  document.getElementById("choose-formal").onclick = () => navigate("/configure/formal");
  document.getElementById("choose-temp").onclick = () => navigate("/configure/temp");
}

function dashboardHTML() {
  const metrics = runtime.metrics || {};
  return `
    <section class="grid three">
      ${statHTML("运行状态", stateText(runtime.state), stateClass(runtime.state))}
      ${statHTML("节点名称", runtime.nodeName || "未配置")}
      ${statHTML("节点类型", nodeTypeText(runtime.nodeType))}
      ${statHTML("运行任务", runtime.runningTasks || 0)}
      ${statHTML("已完成任务", metrics.finishedTasks || 0)}
      ${statHTML("失败任务", metrics.failedTasks || 0, metrics.failedTasks ? "danger-text" : "")}
    </section>
    <section class="panel compact-panel">
      <div>
        <h2 class="section-title">最近错误</h2>
        <p class="${runtime.lastError ? "error" : "ok"}">${escapeHTML(runtime.lastError || "暂无错误")}</p>
      </div>
    </section>`;
}

function operationsHTML() {
  return `
    <section class="split">
      <div class="panel">
        <h2 class="section-title">判题服务</h2>
        <div class="actions">
          <button id="start" class="primary">启动</button>
          <button id="restart">重启</button>
          <button id="stop" class="danger">停止</button>
        </div>
        <div id="operation-message" class="message" role="status"></div>
      </div>
      <div class="panel status-list">
        <h2 class="section-title">当前状态</h2>
        ${kvHTML("状态", stateText(runtime.state))}
        ${kvHTML("运行任务", runtime.runningTasks || 0)}
        ${kvHTML("最近错误", runtime.lastError || "无")}
      </div>
    </section>`;
}

function bindOperations() {
  const start = document.getElementById("start");
  const stop = document.getElementById("stop");
  const restart = document.getElementById("restart");
  if (start) start.onclick = () => runtimeAction("/api/v1/runtime/start");
  if (stop) stop.onclick = () => runtimeAction("/api/v1/runtime/stop");
  if (restart) restart.onclick = () => runtimeAction("/api/v1/runtime/restart");
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
    <section class="panel">
      <div class="form-grid two">
        ${field("节点名称", "node-name", c.node.name)}
        ${field("最大并发", "max-concurrency", c.node.maxConcurrency, "number")}
        ${field("HnieOJ 后端地址", "base-url", c.hnieoj.baseUrl, "text", "http://gateway:8800")}
        ${field("判题模式", "judge-modes", c.node.supportedJudgeModes.join(","))}
        ${field("RabbitMQ 主机", "rabbit-host", c.rabbitmq.host)}
        ${field("RabbitMQ 端口", "rabbit-port", c.rabbitmq.port, "number")}
        ${field("RabbitMQ 用户", "rabbit-user", c.rabbitmq.username)}
        ${field("RabbitMQ 密码", "rabbit-password", "", "password", c.rabbitmq.passwordConfigured ? "留空保持原密码" : "")}
        ${field("RabbitMQ vhost", "rabbit-vhost", c.rabbitmq.virtualHost)}
        ${field("心跳间隔", "heartbeat-interval", c.heartbeat.interval || "30s")}
      </div>
      ${mode === "formal" ? formalKeyHTML(c) : tempAuthHTML()}
      <div class="actions submit-row">
        <button id="submit-config" class="primary">${mode === "formal" ? "保存正式节点配置" : "兑换并保存临时节点配置"}</button>
        <button id="back-config">返回配置入口</button>
      </div>
      <div id="config-message" class="message" role="status"></div>
    </section>`;
}

function formalKeyHTML(cfg) {
  const placeholder = cfg.hnieoj.formalToken.privateKeyConfigured ? "已配置；留空保持原私钥" : "-----BEGIN PRIVATE KEY-----";
  return `
    <div class="key-area">
      <div class="field">
        <label for="formal-private-key-file">上传 formal 私钥 PEM</label>
        <input id="formal-private-key-file" type="file" accept=".pem,.key,text/plain">
      </div>
      <div class="field">
        <label for="formal-private-key">或粘贴 PEM 内容</label>
        <textarea id="formal-private-key" placeholder="${escapeAttr(placeholder)}"></textarea>
      </div>
    </div>`;
}

function tempAuthHTML() {
  return `
    <div class="field key-area">
      <label for="temp-auth-code">临时授权码</label>
      <input id="temp-auth-code" type="password" placeholder="输入后会实时兑换 JWT">
    </div>`;
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
  document.getElementById("submit-config").onclick = async () => {
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
  return `<section class="panel"><div id="logs" class="logs">正在加载日志</div></section>`;
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

function statHTML(label, statValue, className = "") {
  return `
    <article class="stat ${className}">
      <span>${escapeHTML(label)}</span>
      <strong>${escapeHTML(String(statValue))}</strong>
    </article>`;
}

function kvHTML(label, statValue) {
  return `<p><span>${escapeHTML(label)}</span><strong>${escapeHTML(String(statValue))}</strong></p>`;
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

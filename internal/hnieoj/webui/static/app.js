const $ = (id) => document.getElementById(id);
let status = null;
let config = null;
let setupMode = "formal";

async function api(path, options = {}) {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

function show(view) {
  document.querySelectorAll(".view").forEach((el) => el.classList.add("hidden"));
  document.querySelectorAll("nav button").forEach((el) => el.classList.remove("active"));
  $(view).classList.remove("hidden");
  document.querySelector(`[data-view="${view}"]`)?.classList.add("active");
}

function modesFromInput(id) {
  return $(id).value.split(",").map((x) => x.trim()).filter(Boolean);
}

function baseConfig(prefix = "") {
  return {
    node: {
      name: $(`${prefix}node-name`)?.value || $("node-name").value,
      type: prefix ? $("cfg-node-type").value : setupMode,
      maxConcurrency: Number($(`${prefix}max-concurrency`)?.value || $("max-concurrency").value || 1),
      supportedJudgeModes: modesFromInput(prefix ? "cfg-judge-modes" : "judge-modes"),
    },
    hnieoj: {
      baseUrl: $(`${prefix}base-url`)?.value || $("base-url").value,
      requestTimeout: "30s",
      formalToken: { cipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding", refreshInterval: "30s", nacos: {} },
      tempToken: { proofType: "hmac-sha256" },
    },
    rabbitmq: {
      host: $(`${prefix}rabbit-host`)?.value || $("rabbit-host").value,
      port: Number($(`${prefix}rabbit-port`)?.value || $("rabbit-port").value || 5672),
      username: $(`${prefix}rabbit-user`)?.value || $("rabbit-user").value,
      password: $(`${prefix}rabbit-password`)?.value || $("rabbit-password").value,
      virtualHost: $(`${prefix}rabbit-vhost`)?.value || $("rabbit-vhost").value,
      exchange: "hnieoj.judge.exchange",
      queue: "hnieoj.judge.task",
      routingKey: "judge.submission.created",
      deadLetterExchange: "hnieoj.judge.dlx",
      deadLetterQueue: "hnieoj.judge.task.dlq",
      deadLetterRoutingKey: "judge.submission.created.dlq",
      prefetch: Number($(`${prefix}max-concurrency`)?.value || $("max-concurrency").value || 1),
      maxRetries: 3,
      retryBackoff: "10s",
    },
    testdata: { cacheRoot: "/data/oj/judge-cache", maxCacheBytes: 21474836480, maxUnusedDuration: "72h", cleanupInterval: "1h", statsInterval: "5m" },
    gojudge: { endpoint: "http://127.0.0.1:5050" },
    reporter: { mode: "http", endpoint: "/judge/submissions/{submissionId}/events" },
    heartbeat: { enabled: true, endpoint: "/judge/nodes/heartbeat", interval: $("cfg-heartbeat-interval")?.value || "30s" },
    remoteConfig: { enabled: false, nacos: {} },
  };
}

async function refresh() {
  const setup = await api("/api/v1/setup/status");
  status = setup.runtime;
  $("subtitle").textContent = setup.adminInitialized ? "本地管理已启用" : "请先创建管理员密码";
  $("login-view").classList.toggle("hidden", setup.adminInitialized && setup.authenticated);
  $("stat-state").textContent = status.state;
  $("stat-type").textContent = status.nodeType || "-";
  $("stat-running").textContent = status.runningTasks;
  $("stat-finished").textContent = status.metrics?.finishedTasks || 0;
  $("stat-failed").textContent = status.metrics?.failedTasks || 0;
  $("stat-error").textContent = status.lastError || "无";
  try {
    config = await api("/api/v1/config");
    fillConfig(config);
  } catch {}
}

function fillConfig(c) {
  $("cfg-node-name").value = c.node?.name || "";
  $("cfg-node-type").value = c.node?.type || "formal";
  $("cfg-max-concurrency").value = c.node?.maxConcurrency || 1;
  $("cfg-judge-modes").value = (c.node?.supportedJudgeModes || ["default"]).join(",");
  $("cfg-base-url").value = c.hnieoj?.baseUrl || "";
  $("cfg-rabbit-host").value = c.rabbitmq?.host || "";
  $("cfg-rabbit-port").value = c.rabbitmq?.port || 5672;
  $("cfg-rabbit-user").value = c.rabbitmq?.username || "";
  $("cfg-rabbit-vhost").value = c.rabbitmq?.virtualHost || "";
  $("cfg-heartbeat-interval").value = c.heartbeat?.interval || "30s";
}

async function loadLogs() {
  const logs = await api("/api/v1/logs/recent");
  $("log-list").innerHTML = logs.map((x) => `<div class="log"><span>${new Date(x.time).toLocaleTimeString()}</span><b>${x.level}</b><span>${x.message}</span></div>`).join("");
}

document.querySelectorAll("nav button").forEach((btn) => btn.addEventListener("click", () => {
  show(btn.dataset.view);
  if (btn.dataset.view === "logs") loadLogs().catch(console.error);
}));

$("mode-formal").onclick = () => {
  setupMode = "formal";
  $("mode-formal").classList.add("active");
  $("mode-temp").classList.remove("active");
  document.querySelectorAll(".formal-only").forEach((x) => x.classList.remove("hidden"));
  document.querySelectorAll(".temp-only").forEach((x) => x.classList.add("hidden"));
};
$("mode-temp").onclick = () => {
  setupMode = "temp";
  $("mode-temp").classList.add("active");
  $("mode-formal").classList.remove("active");
  document.querySelectorAll(".temp-only").forEach((x) => x.classList.remove("hidden"));
  document.querySelectorAll(".formal-only").forEach((x) => x.classList.add("hidden"));
};

$("login-submit").onclick = async () => {
  $("login-message").textContent = "";
  try {
    const path = (await api("/api/v1/setup/status")).adminInitialized ? "/api/v1/auth/login" : "/api/v1/setup/admin";
    await api(path, { method: "POST", body: JSON.stringify({ password: $("login-password").value }) });
    await refresh();
  } catch (e) { $("login-message").textContent = e.message; }
};

$("setup-submit").onclick = async () => {
  $("setup-message").textContent = "正在提交...";
  try {
    const cfg = baseConfig();
    if (setupMode === "formal") {
      await api("/api/v1/setup/formal", { method: "POST", body: JSON.stringify({ config: cfg, privateKeyPem: $("formal-private-key").value }) });
    } else {
      await api("/api/v1/setup/temp/exchange", { method: "POST", body: JSON.stringify({ config: cfg, authCode: $("temp-auth-code").value }) });
    }
    $("setup-message").textContent = "保存成功，可以启动判题服务。";
    await refresh();
  } catch (e) { $("setup-message").textContent = e.message; }
};

$("config-save").onclick = async () => {
  $("config-message").textContent = "保存中...";
  try {
    const cfg = baseConfig("cfg-");
    await api("/api/v1/config", { method: "PUT", body: JSON.stringify(cfg) });
    $("config-message").textContent = "已保存。重启判题服务后生效。";
    await refresh();
  } catch (e) { $("config-message").textContent = e.message; }
};

$("start").onclick = () => api("/api/v1/runtime/start", { method: "POST" }).then(refresh).catch((e) => alert(e.message));
$("stop").onclick = () => api("/api/v1/runtime/stop", { method: "POST" }).then(refresh).catch((e) => alert(e.message));
$("restart").onclick = () => api("/api/v1/runtime/restart", { method: "POST" }).then(refresh).catch((e) => alert(e.message));
$("refresh").onclick = refresh;
$("logout").onclick = () => api("/api/v1/auth/logout", { method: "POST" }).then(() => location.reload());

refresh().catch(() => {});
setInterval(refresh, 5000);

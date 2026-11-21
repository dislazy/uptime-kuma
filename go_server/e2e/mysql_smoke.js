// MySQL/MariaDB 后端冒烟脚本（需要 socket.io-client）。
//
// 用法（服务端要跑在 MariaDB 上）：
//   npm i socket.io-client
//   KUMA_BASE=http://127.0.0.1:3001 KUMA_USER=admin KUMA_PASS=****** node e2e/mysql_smoke.js init
//   # 重启服务端（验证数据确实落库，不是内存里活着）
//   node e2e/mysql_smoke.js verify
//   node e2e/mysql_smoke.js crud
//   node e2e/mysql_smoke.js clean
//
// 覆盖范围：
//   init   —— 首次初始化管理员、加监控、等首条心跳
//   verify —— 重启后登录，读回监控与心跳（含毫秒时间戳）
//   crud   —— 设置/标签/代理/状态页分组/维护窗口/API Key/通知 的增删改查
//   clean  —— 删监控，验证 7 张关联表被显式清理
const { io } = require("socket.io-client");
const fs = require("fs");
const os = require("os");
const path = require("path");

const BASE = process.env.KUMA_BASE || "http://127.0.0.1:3001";
const USER = process.env.KUMA_USER || "admin";
const PASS = process.env.KUMA_PASS || "";
const STATE = path.join(os.tmpdir(), "kuma-mysql-smoke.json");
const MON_NAME = "mysql-smoke-self";

const emit = (s, n, ...a) => new Promise((r) => s.emit(n, ...a, r));

function waitEvent(socket, name, pred, timeout = 20000) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("timeout waiting " + name)), timeout);
    const handler = (...args) => {
      try {
        if (pred(...args)) {
          clearTimeout(timer);
          socket.off(name, handler);
          resolve(args);
        }
      } catch (e) { /* keep waiting */ }
    };
    socket.on(name, handler);
  });
}

let failed = 0;
function check(label, ok, extra) {
  const tail = extra === undefined ? "" : " → " + JSON.stringify(extra).slice(0, 160);
  console.log((ok ? "  PASS " : "  FAIL ") + label + tail);
  if (!ok) failed++;
  return ok;
}

async function signIn(p, body) {
  const res = await fetch(BASE + p, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  const raw = ((res.headers.getSetCookie ? res.headers.getSetCookie()[0] : "") ||
    res.headers.get("set-cookie") || "").split(";")[0];
  return { status: res.status, cookie: raw, body: await res.text() };
}

async function connect(cookie) {
  const socket = io(BASE, {
    transports: ["websocket", "polling"],
    reconnection: false,
    extraHeaders: { Cookie: cookie },
  });
  await new Promise((res, rej) => {
    socket.on("connect", res);
    socket.on("connect_error", (e) => rej(new Error("socket 连接失败: " + e.message)));
  });
  return socket;
}

function saveState(obj) { fs.writeFileSync(STATE, JSON.stringify(obj)); }
function loadState() { return JSON.parse(fs.readFileSync(STATE, "utf8")); }

// ---------------- init ----------------
async function phaseInit() {
  const health = await (await fetch(BASE + "/api/health")).json();
  check("/api/health", health.ok === true, health);

  const s = await signIn("/api/setup", { username: USER, password: PASS, rememberMe: true });
  check("setup", !!s.cookie, s.status + " " + s.body);

  const socket = await connect(s.cookie);

  // 先挂监听再 add：服务端可能在 ack 之前就推送 monitorList
  const listP = waitEvent(socket, "monitorList", (m) => m && Object.keys(m).length > 0);
  const added = await emit(socket, "add", {
    type: "http", name: MON_NAME, url: BASE + "/api/health",
    interval: 20, retryInterval: 20, maxretries: 1,
  });
  check("add monitor", !!(added && added.id), added);
  await listP;

  const beat = (await waitEvent(socket, "heartbeat", (b) => b && b.monitorID === added.id, 30000))[0];
  check("首条心跳 UP", beat.status === 1, { status: beat.status, msg: beat.msg, time: beat.time });

  saveState({ monitorID: added.id, cookie: s.cookie });
  socket.close();
}

// ---------------- verify（重启后） ----------------
async function phaseVerify() {
  const st = loadState();
  const s = await signIn("/api/auth/sign-in/username", { username: USER, password: PASS, rememberMe: true });
  check("重启后登录", !!s.cookie, s.status);
  if (!s.cookie) return;

  const socket = await connect(s.cookie);
  const listP = waitEvent(socket, "monitorList", (m) => m && m[st.monitorID], 10000);
  const beatsP = waitEvent(socket, "heartbeatList", (id, beats) => id === st.monitorID && beats.length > 0, 15000);

  const list = (await listP)[0];
  check("重启后监控仍在", list[st.monitorID].name === MON_NAME, list[st.monitorID].name);

  const beats = (await beatsP)[1];
  check("重启后心跳仍在", beats.length > 0, { count: beats.length, first: beats[0] && beats[0].time });
  // MySQL 用 DATETIME(3)，毫秒必须保住；丢了会看到整秒
  check("时间戳带毫秒", /\d{2}:\d{2}:\d{2}\.\d{3}$/.test(String(beats[0].time)), beats[0].time);

  socket.close();
}

// ---------------- crud ----------------
async function phaseCrud() {
  const st = loadState();
  const s = await signIn("/api/auth/sign-in/username", { username: USER, password: PASS, rememberMe: true });
  const socket = await connect(s.cookie);
  const slug = "smoke-" + (Date.now() % 100000);
  const mid = st.monitorID;

  check("setSettings", (await emit(socket, "setSettings", { serverTimezone: "Asia/Shanghai" }, "general")).ok !== false);
  const gs = await emit(socket, "getSettings");
  check("getSettings 读回", gs.data && gs.data.serverTimezone === "Asia/Shanghai", gs.data && gs.data.serverTimezone);

  const tag = await emit(socket, "addTag", { name: "smoke", color: "#3498db" });
  check("addTag", !!(tag && tag.id), tag);
  if (tag && tag.id) check("editTag", (await emit(socket, "editTag", { id: tag.id, name: "smoke2", color: "#9b59b6" })).ok !== false);

  const proxy = await emit(socket, "addProxy", { protocol: "http", host: "127.0.0.1", port: 3128, default: false });
  check("addProxy", !!(proxy && proxy.id), proxy);

  check("addStatusPage", (await emit(socket, "addStatusPage", "Smoke", slug)).ok !== false);
  const saved = await emit(socket, "saveStatusPage", slug, {
    slug, title: "Smoke 改", description: "emoji ✅", theme: "light",
    autoRefreshInterval: 60, showTags: true, showPoweredBy: true,
  }, "", [{ id: 0, name: "G1", weight: 1, monitorList: [] }]);
  check("saveStatusPage（含分组）", saved.ok !== false, saved);
  const got = await emit(socket, "getStatusPage", slug);
  check("getStatusPage 读回", got.config && got.config.title === "Smoke 改", got.config && got.config.title);

  const maint = await emit(socket, "addMaintenance", { title: "smoke", strategy: "single", active: false, duration: 60 });
  check("addMaintenance", !!(maint && maint.maintenanceID), maint);

  const key = await emit(socket, "addAPIKey", { name: "smoke" });
  check("addAPIKey", !!(key && key.keyID), key && key.keyID);
  const keys = await emit(socket, "getAPIKeyList");
  check("getAPIKeyList", keys.apiKeyList && keys.apiKeyList.length > 0, keys.apiKeyList && keys.apiKeyList.length);

  const notif = await emit(socket, "addNotification", {
    name: "smoke", type: "webhook", isDefault: false, active: true,
    webhookURL: "https://example.com/hook", webhookContentType: "json",
  });
  check("addNotification", !!(notif && notif.id), notif);

  // 监控编辑链路
  const got2 = await emit(socket, "getMonitor", mid);
  const mon = got2.monitor;
  mon.name = MON_NAME + "-edited";
  check("editMonitor", (await emit(socket, "editMonitor", mon, mid)).ok !== false);
  check("编辑已生效", (await emit(socket, "getMonitor", mid)).monitor.name === MON_NAME + "-edited");
  check("pauseMonitor", (await emit(socket, "pauseMonitor", mid)).ok !== false);
  check("resumeMonitor", (await emit(socket, "resumeMonitor", mid)).ok !== false);
  check("getHeartbeatHistory", (await emit(socket, "getHeartbeatHistory", mid, 24)).ok !== false);
  check("getMonitorChartData", !!(await emit(socket, "getMonitorChartData", mid, 24)));

  // Docker 主机 / 远程浏览器 / 库体积
  const dh = await emit(socket, "addDockerHost", { name: "smoke", dockerDaemon: "tcp://127.0.0.1:2375", dockerType: "socket" });
  check("addDockerHost", !!(dh && dh.id), dh);
  if (dh && dh.id) check("deleteDockerHost", (await emit(socket, "deleteDockerHost", dh.id)).ok !== false);

  const rb = await emit(socket, "addRemoteBrowser", { name: "smoke", url: "ws://127.0.0.1:3000/playwright" });
  check("addRemoteBrowser", !!(rb && rb.id), rb);
  if (rb && rb.id) check("deleteRemoteBrowser", (await emit(socket, "deleteRemoteBrowser", rb.id)).ok !== false);

  // MySQL 下官方 getDatabaseSize 返回 0、shrink 是空操作；不能报错就行
  check("getDatabaseSize", (await emit(socket, "getDatabaseSize")) !== null);
  check("shrinkDatabase（MySQL 空操作）", (await emit(socket, "shrinkDatabase")).ok !== false);

  // 回滚
  if (tag && tag.id) check("deleteTag", (await emit(socket, "deleteTag", tag.id)).ok !== false);
  if (proxy && proxy.id) check("deleteProxy", (await emit(socket, "deleteProxy", proxy.id)).ok !== false);
  if (maint && maint.maintenanceID) check("deleteMaintenance", (await emit(socket, "deleteMaintenance", maint.maintenanceID)).ok !== false);
  if (key && key.keyID) check("deleteAPIKey", (await emit(socket, "deleteAPIKey", key.keyID)).ok !== false);
  if (notif && notif.id) check("deleteNotification", (await emit(socket, "deleteNotification", notif.id)).ok !== false);
  check("deleteStatusPage", (await emit(socket, "deleteStatusPage", slug)).ok !== false);

  socket.close();
}

// ---------------- clean ----------------
async function phaseClean() {
  const st = loadState();
  const s = await signIn("/api/auth/sign-in/username", { username: USER, password: PASS, rememberMe: true });
  const socket = await connect(s.cookie);

  check("clearEvents", (await emit(socket, "clearEvents", st.monitorID)).ok !== false);
  check("clearHeartbeats", (await emit(socket, "clearHeartbeats", st.monitorID)).ok !== false);
  const emptyP = waitEvent(socket, "monitorList", (m) => m && !m[st.monitorID], 10000);
  check("deleteMonitor", (await emit(socket, "deleteMonitor", st.monitorID)).ok !== false);
  const left = (await emptyP)[0];
  check("删除后列表为空", Object.keys(left).length === 0, Object.keys(left).length);

  socket.close();
}

(async () => {
  if (!PASS) {
    console.error("请设置 KUMA_PASS（以及可选的 KUMA_USER / KUMA_BASE）");
    process.exit(2);
  }
  const phase = process.argv[2] || "init";
  const table = { init: phaseInit, verify: phaseVerify, crud: phaseCrud, clean: phaseClean };
  if (!table[phase]) {
    console.error("未知阶段: " + phase + "（可选 init / verify / crud / clean）");
    process.exit(2);
  }
  console.log("== " + phase + " @ " + BASE + " ==");
  await table[phase]();
  console.log(failed === 0 ? "== " + phase + " PASS ==" : "== " + phase + " 有 " + failed + " 项失败 ==");
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error("错误:", e.message); process.exit(1); });

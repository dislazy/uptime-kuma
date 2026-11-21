# API 对齐审计（Node 官方后端 vs Go 后端）

> 基线：官方 `upstream/master` = `b0cd0f1d`（2026-11-21 同步），Go 侧 `main` = `2b168fe6`。
> 本文逐条核对两侧全部对外 API：REST 路由、better-auth 端点、Socket.IO 双向事件。
> 核对方法见文末附录，可用相同命令复现。

## 结论速览

| 类别 | 官方总数 | Go 直接对齐 | 等价覆盖 | 缺失 | 说明 |
| --- | --- | --- | --- | --- | --- |
| REST 路由（生产挂载） | 40 | 33 | — | 7 | `/metrics` 鉴权已对齐官方（2026-11-21 修复），见 §1.1 |
| better-auth 端点（前端实际调用） | 10 | 10 | — | 0 | 其余官方端点前端不调用，Go 统一 501 兜底，见 §2 |
| Socket 事件（客户端→服务端） | 76 | 66 | — | 10 | 缺失集中在 proxy / remote-browser / cloudflared，见 §3 |
| Socket 推送（服务端→客户端） | 23 | 16 | 3 | 4 | 前端 UI 实际受影响的只剩 domainInfo（域名 whois），见 §4 |

> 计数口径：REST 按"官方生产模式挂载的独立端点"逐条计（dev/e2e 专属路由除外）；
> better-auth 只计前端 `authClient.*` 实际调用的端点；socket 按注册名逐条计。

---

## 1. REST 路由全表

### 1.1 已对齐（32 条）

| 官方路由 | 官方位置 | Go 实现 | 备注 |
| --- | --- | --- | --- |
| `GET /`（dist 静态） | server.js:346 | `internal/web/web.go` | SPA 回退、hash 资源长缓存，行为一致 |
| `GET *`（SPA 回退） | server.js:377 | `internal/web/web.go` | ✓ |
| `POST /api/setup` | better-auth-router.ts:41 | `httpapi.handleSetup` | 首次初始化 |
| `GET /api/auth/ok` | better-auth 健康检查 | `httpapi.handleOK` | ✓ |
| `POST /api/auth/sign-in/username` | better-auth | `httpapi.handleSignIn` | 登录主链路 |
| `GET /api/auth/get-session` | better-auth | `httpapi.handleGetSession` | 含 2FA 状态 |
| `POST /api/auth/sign-out` | better-auth | `httpapi.handleSignOut` | ✓ |
| `POST /api/auth/change-password` | better-auth | `httpapi.handleChangePassword` | 2026-11-21 补齐；旧哈希自动升级 scrypt |
| `POST /api/auth/two-factor/enable` | better-auth 插件 | `httpapi.handleTwoFAEnable` | ✓ |
| `POST /api/auth/two-factor/verify-totp` | better-auth 插件 | `httpapi.handleTwoFAVerifyTOTP` | ✓ |
| `POST /api/auth/two-factor/verify-backup-code` | better-auth 插件 | `httpapi.handleTwoFAVerifyBackupCode` | ✓ |
| `POST /api/auth/two-factor/disable` | better-auth 插件 | `httpapi.handleTwoFADisable` | ✓ |
| `POST /api/auth/two-factor/generate-backup-codes` | better-auth 插件 | `httpapi.handleTwoFAGenerateBackupCodes` | 兼容 `/api/auth/generate-backup-codes` 旧路径 |
| `GET /api/entry-page` | api-router.js:353 | `httpapi.handleEntryPage` | ✓ |
| `ALL /api/push/:pushToken` | api-router.js:372 | `httpapi.handlePush` | 被动推送监控 |
| `GET /api/badge/:id/status` | api-router.js:495 | `httpapi.handleBadge` | ✓ |
| `GET /api/badge/:id/uptime/:duration?` | api-router.js:568 | 同上 | ✓ |
| `GET /api/badge/:id/ping/:duration?` | api-router.js:633 | 同上 | ✓ |
| `GET /api/badge/:id/avg-response/:duration?` | api-router.js:697 | 同上 | ✓ |
| `GET /api/badge/:id/cert-exp` | api-router.js:762 | 同上 | ✅ 真实数据（2026-11-21 补齐）：官方四分支全对齐——非公开 N/A / 无记录 No/Bad Cert / 无效 Bad Cert / 有效则天数+颜色（>14 绿 >7 黄），date 模式显到期日 |
| `GET /api/badge/:id/response` | api-router.js:845 | 同上 | ✓ |
| `GET /api/info` | api-router.js:204 | `httpapi.handleAPIInfo` | requireApiKey，2026-11-21 补齐 |
| `GET /api/monitors` | api-router.js:222 | `httpapi.handleAPIMonitors` | requireApiKey，toCompactMonitor 脱敏形状 |
| `GET /api/status-page/:slug` | status-page-router.js:39 | `httpapi.handleStatusPageData` | 前缀分发 |
| `GET /api/status-page/heartbeat/:slug` | status-page-router.js:64 | `httpapi.handleStatusPageHeartbeat` | ✓ |
| `GET /api/status-page/:slug/manifest.json` | status-page-router.js:114 | `httpapi.handleStatusPageManifest` | PWA |
| `GET /api/status-page/:slug/incident-history` | status-page-router.js:146 | `httpapi.handleStatusPageIncidentHistory` | ✓ |
| `GET /status/:slug`（HTML） | status-page-router.js:16 | SPA 回退 index.html | 官方同源逻辑（也回 index.html） |
| `GET /status` / `GET /status-page` | status-page-router.js:28/33 | SPA 回退 | ✓ |
| `GET /status/:slug/rss` | status-page-router.js:22 | `httpapi.handleStatusPageRSS` | ✓ |
| `GET /metrics` | server.js:329 | `httpapi.handleMetrics` | ✅ 官方鉴权全链路（prometheusApiEnabled 门控 404 → disableAuth → apiKeysEnabled 时 Basic 密码字段=API key，否则用户名密码；E2E 验证 404/401/200/分支切换） |
| `GET /upload/*`（静态） | server.js:354 | FileServer | ✓ |

### 1.2 缺失（7 条）

| 官方路由 | 官方位置 | 影响 | 补齐成本 |
| --- | --- | --- | --- |
| `GET /robots.txt` | server.js:317 | 搜索引擎抓取控制（跟随 `searchEngineIndex` 设置） | 极小 |
| `GET /.well-known/change-password` | server.js:356 | 浏览器密码管理器跳转官方 wiki | 极小 |
| `GET /api/status-page/:slug/badge` | status-page-router.js:171 | 状态页整体状态徽章（外部引用场景） | 小 |
| `GET /api/openapi.json` | api-router.js:347 | OpenAPI 描述文件 | 中 |
| `GET /api/docs` | api-router.js:351 | API 文档页，**前端 Settings→General 有链接**（General.vue:233），当前点击 404 | 中（依赖上一条） |
| `ALL /mcp` | mcp-router.ts:74 | 上游新增的 MCP（AI 工具接入）端点，requireApiKey | 大（独立协议栈） |
| `GET /screenshots/*`（静态） | uptime-kuma-server.ts:230 | real-browser 截图外链 | n/a：Go 的 real-browser 走本地 Chromium，无截图产物 |

### 1.3 不适用（官方仅在 dev/e2e 模式挂载）

`GET /setup-database-info`、`POST /test-webhook`、`POST /test-x-www-form-urlencoded`（server.js:283-313，仅 `NODE_ENV===development` 或 e2e 模式生效）。生产镜像不暴露，Go 无需实现。

---

## 2. better-auth 端点核对

官方 `better-auth.ts` 挂载 username / admin / twoFactor / apiKey / haveIBeenPwned 五插件。
前端实际调用全集（`git grep authClient. src/`）：

| 前端调用 | 对应端点 | Go 状态 |
| --- | --- | --- |
| `signIn.username` | `POST /api/auth/sign-in/username` | ✅ |
| `signOut` | `POST /api/auth/sign-out` | ✅ |
| `getSession` | `GET /api/auth/get-session` | ✅ |
| `changePassword` | `POST /api/auth/change-password` | ✅（2026-11-21 补齐） |
| `twoFactor.enable` | `POST /api/auth/two-factor/enable` | ✅ |
| `twoFactor.disable` | `POST /api/auth/two-factor/disable` | ✅ |
| `twoFactor.verifyTotp` | `POST /api/auth/two-factor/verify-totp` | ✅ |
| （登录页探活） | `GET /api/auth/ok` | ✅ |

API Key 的 socket 管理事件（addAPIKey 等）与 REST 鉴权（verifyApiKey，requireApiKey 中间件）均已在 `internal/httpapi/api.go` 实现。
**未实现但前端也不调用**（统一 501 兜底，不影响 UI）：admin 插件（多用户管理）、haveIBeenPwned（弱密码检查，`POST /api/auth/hibp`）、forget/reset-password、update-user 等。

---

## 3. Socket.IO 客户端→服务端事件全表（76 条）

### 3.1 server.js 核心（29 条）

| 事件 | Go | 备注 |
| --- | --- | --- |
| `add` | ✅ | 新建监控 |
| `editMonitor` | ✅ | |
| `getMonitorList` | ✅ | |
| `getMonitor` | ✅ | |
| `deleteMonitor` | ✅ | 含 deleteChildren 参数 |
| `pauseMonitor` / `resumeMonitor` | ✅ | |
| `getHeartbeatHistory` | ✅ | |
| `getMonitorBeats` | ✅ | |
| `monitorImportantHeartbeatListCount` | ✅ | |
| `monitorImportantHeartbeatListPaged` | ✅ | |
| `getTags` / `addTag` / `editTag` / `deleteTag` | ✅ | |
| `addMonitorTag` | ✅ | 2026-11-21 补齐；INSERT + 全量 monitorList 广播（官方为增量推送，等价），回包 `successAdded` |
| `deleteMonitorTag` | ✅ | 2026-11-21 补齐；value 参与匹配，回包 `successDeleted` |
| `editMonitorTag` | ✅ | 2026-11-21 补齐（官方有注册、前端暂不调用） |
| `getSettings` / `setSettings` | ✅ | |
| `addNotification` / `deleteNotification` / `testNotification` | ✅ | |
| `checkApprise` | ✅ | |
| `clearEvents` / `clearHeartbeats` / `clearStatistics` | ✅ | |
| `checkDomain` | ✅ | 占位实现（返回空域名），RDAP 未接 |
| `getWebpushVapidPublicKey` | ✅ | |

### 3.2 各 socket-handler 文件（47 条）

| 文件 | 事件 | Go 状态 |
| --- | --- | --- |
| api-key（3） | `addAPIKey` / `getAPIKeyList` / `deleteAPIKey` | ✅ 全部（2026-11-21 补齐，含 legacy key 兼容） |
| better-auth（1） | `needSetup` | ✅ |
| chart（1） | `getMonitorChartData` | ✅ |
| cloudflared（5） | `cloudflared join` / `leave` / `start` / `stop` / `removeToken` | ❌ 全缺（Cloudflared Tunnel 设置页不可用；登录时也不会推 `cloudflared` 相关状态） |
| database（2） | `getDatabaseSize` / `shrinkDatabase` | ✅ |
| docker（3） | `addDockerHost` / `deleteDockerHost` / `testDockerHost` | ✅ |
| general（6） | `initServerTimezone` / `getGameList` / `getPM2ProcessList` / `testChrome` / `getPushExample` / `disconnectOtherSocketClients` | ✅ 全注册；其中 getGameList / PM2 / testChrome 为占位返回 |
| maintenance（11） | add/edit/delete/pause/resume + getMonitorMaintenance 等全套 | ✅ 全部 |
| proxy（2） | `addProxy` / `deleteProxy` | ❌ 全缺（**Proxies 设置页不可用**；且 Go 的 HTTP checker 不消费 `proxy_id`——即使接管官方库，带代理的监控也直连目标） |
| remote-browser（3） | `addRemoteBrowser` / `deleteRemoteBrowser` / `testRemoteBrowser` | ❌ 全缺（Remote Browsers 设置页不可用；Go 的 real-browser 监控走本地 Chromium，不走远程浏览器） |
| status-page（10） | incident 增删改 + saveStatusPage 等 | ✅ 全部 |

### 3.3 Go 多出的事件（官方没有）

| 事件 | 说明 |
| --- | --- |
| `login` / `loginByToken` | 旧版 socket 登录兼容（v3 前端已走 better-auth HTTP，不会触发），无害 |

---

## 4. Socket.IO 服务端→客户端推送全表（23 条）

前端监听全集（`git grep socket.on( src/mixins/socket.js` 等）：

| 事件 | Go 状态 | 备注 |
| --- | --- | --- |
| `setup` / `session` / `loginRequired` / `info` | ✅ | 登录握手链路 |
| `monitorList` | ✅ | **且 add/edit/delete 后全量广播**，等价覆盖 `updateMonitorIntoList` / `deleteMonitorFromList`（前端 monitorList 处理器本就是全量替换，socket.js:185） |
| `updateMonitorIntoList` / `deleteMonitorFromList` | ≈ 等价 | 见上；官方为增量推送，Go 为全量，前端行为一致 |
| `heartbeat` | ✅ | 单次心跳推送 |
| `heartbeatList` | ✅ | 单监控近期心跳 |
| `heartbeatLists` | ≈ 等价 | 官方登录后聚合一包；Go 逐监控发 `heartbeatList`，前端两个监听器效果相同 |
| `importantHeartbeatList` | ❌ | 官方会推但**前端不监听**（socket.js 无注册），仅移动端/API 消费；无 UI 影响 |
| `certInfo` | ✅ | 2026-11-21 补齐：https 握手后采集证书链（对齐 readTlsInfo/parseCertificateInfo 形状，含嵌套 issuerCertificate 与 certType），落库 monitor_tls_info 并推送。E2E 与 openssl 逐秒一致 |
| `domainInfo` | ❌ | 域名到期信息未采集（依赖 checkDomain 的 RDAP，Go 未接） |
| `proxyList` | ✅ | 登录推送空列表（因 proxy CRUD 未实现，见 §3.2） |
| `remoteBrowserList` | ✅ | 同上，空列表 |
| `dockerHostList` / `maintenanceList` / `notificationList` / `statusPageList` / `monitorTypeList` / `stats` | ✅ | |
| `refresh` | ❌ | 官方踢其它会话前先发 refresh（uptime-kuma-server.ts:594），Go 直接断开；被踢端少一次"强制刷新"体验 |
| `initServerTimezone`（推送） | ❌ | 官方登录后若 `initServerTimezone` 设置未置位则推送该事件触发前端时区初始化弹窗（server.js:1562）；Go 实现了回包 handler 但从不主动推送，弹窗永不出现 |
| `avgPing` / `uptime` | n/a | 前端监听的遗留事件，**官方服务端同样从不推送** |
| `tags`（Go 多出） | 额外 | Go 在标签变更后广播，前端无监听，无害 |

---

## 5. 已知偏差分级汇总

### A. 安全/正确性 —— 2026-11-21 已全部修复 ✅

| # | 偏差 | 状态 |
| --- | --- | --- |
| A1 | `/metrics` 鉴权：`prometheusApiEnabled !== true` → 404；disableAuth 放行；apiKeysEnabled 时 Basic Auth 密码字段=API key（走 apiRateLimiter 60/min），否则用户名密码（走 loginRateLimiter 10/60s 按 IP），完全对齐 server.js:329 + auth.ts:182 | ✅ E2E 验证 404→401→200→key 分支切换 |
| A2 | `addMonitorTag` / `editMonitorTag` / `deleteMonitorTag` 事件注册，回包 successAdded/Edited/Deleted | ✅ E2E 验证 |

### B. 功能缺失（用户可感，按影响排序）

| # | 功能 | 影响面 |
| --- | --- | --- |
| B1 | ~~TLS 证书信息采集 + `certInfo` 推送 + `cert-exp` 徽章~~ | ✅ 2026-11-21 已补齐（含证书到期通知：tlsExpiryNotifyDays [7,14,21]、expiry_notification 开关、sent_history 去重、换证书清历史，官方 util.ts:843 全语义） |
| B2 | Proxy 管理（socket 事件 + checker 消费 `proxy_id`） | Proxies 设置页不可用；接管官方库后带代理监控直连 |
| B3 | Remote Browser 管理（3 事件） | Remote Browsers 设置页不可用（real-browser 本地 Chromium 仍可用） |
| B4 | Cloudflared Tunnel（5 事件） | Cloudflared 设置页不可用 |
| B5 | `/api/docs` + `/api/openapi.json` | Settings→General 的 API 文档链接 404 |
| B6 | `/api/status-page/:slug/badge` | 状态页整体徽章不可用 |
| B7 | `domainInfo` + checkDomain（RDAP） | 域名到期信息缺失（TLS 证书到期已由 B1 覆盖，此项是域名 whois，另一条线） |
| B8 | `/robots.txt`、`/.well-known/change-password` | 低影响 |
| B9 | `refresh` / `initServerTimezone` 推送 | 体验细节（被踢端不自动刷新；时区初始化弹窗不出现） |
| B10 | `/mcp` | 上游新功能（AI 工具接入），独立协议栈，暂缓 |
| B11 | `importantHeartbeatList` 推送 | 前端不消费，仅影响潜在移动端/API 消费者 |

### C. 等价实现（行为一致，机制不同）

- monitor 列表增量更新 → 全量广播；heartbeatLists 聚合 → 逐监控 heartbeatList；前端处理结果相同。
- badge 缓存：官方 apicache 5 分钟，Go 发 `no-store`（实时性更高，QPS 高时开销更大）。
- 静态资源：官方 express-static-gzip（brotli 预压缩），Go net/http 原生（无 br，有 gzip 需反代开启）。
- `/metrics` 指标集：官方完整 Prometheus 指标，Go 仅 3 个自订 gauge（`gokuma_*`）。

### D. 占位（有意不实现，返回合法空结果）

`checkDomain`（RDAP）、`getGameList`、`getPM2ProcessList`（官方 PM2 进程管理，Node 专属）、`testChrome`、better-auth admin/HIBP/forget-password 端点（501）。

### E. 不适用

- dev/e2e 专用路由（§1.3）；`avgPing`/`uptime` 遗留推送（官方也不发）；`/screenshots`（无截图产物）。

---

## 6. 与前端联动验证过的链路

以下链路在本地真机 E2E 走通（非静态比对）：

setup → sign-in → get-session → change-password → 重登录；
`/metrics` 官方鉴权全链路（prometheusApiEnabled 门控 → Basic Auth 两分支 → apiKey 切换）；
monitorTag 三事件（add/edit/delete + 回包文案）；
TLS 证书全链路（https 监控 → certInfo 推送 → monitor_tls_info 落库 → cert-exp 徽章 34 days/绿/date 模式，validTo 与 openssl 逐秒一致）；
2FA enable/verify/disable；API Key add/list/delete（含 legacy key）；
REST `/api/info` `/api/monitors`（200/401/403/429）；badge 六 metric；状态页四端点；push 监控；`/metrics` 全鉴权链（404/401/200/apiKey 分支，A1 修复验证）。

---

## 7. 附录：核对命令（可复现）

```bash
# 官方 REST 路由
git grep -nE 'router\.(get|post|all|put|delete)\(' upstream/master -- server/routers/
git grep -nE 'app\.(get|post|use|all)\(' upstream/master -- server/server.js server/uptime-kuma-server.ts

# Go REST 路由
grep -rn 'mux\.Handle' go_server/internal/httpapi/router.go go_server/internal/httpapi/api.go

# 官方 socket 注册（客户端→服务端）
git grep -ohE 'socket\.on\("[a-zA-Z]*"' upstream/master -- server/
# Go socket 注册
grep -rhoE 'sock\.On\("[^"]*"' go_server/internal/handlers/

# 官方推送（服务端→客户端，含跨行 emit）
git grep -hE '\.emit\("' upstream/master -- server/ | grep -oE '"[a-zA-Z]*"'
git grep -A1 -hE '\.emit\($' upstream/master -- server/   # 跨行形式

# 前端消费面（权威清单）
git grep -ohE 'socket\.on\("[a-zA-Z]*"' upstream/master -- src/
git grep -ohE 'authClient\.[a-zA-Z.]*' upstream/master -- src/
```

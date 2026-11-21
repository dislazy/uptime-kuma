# API 对齐审计（Node 官方后端 vs Go 后端）

> 基线：官方 `upstream/master` = `b0cd0f1d`（2026-11-21 同步）；Go 侧 `main` 持续演进，各缺口完成状态以行内标注为准。
> 本文逐条核对两侧全部对外 API：REST 路由、better-auth 端点、Socket.IO 双向事件。
> 核对方法见文末附录，可用相同命令复现。

## 结论速览

| 类别 | 官方总数 | Go 直接对齐 | 等价覆盖 | 缺失 | 说明 |
| --- | --- | --- | --- | --- | --- |
| REST 路由（生产挂载） | 40 | 39 | — | 1 | `/metrics` 鉴权、状态页徽章、robots.txt、change-password、openapi.json、/api/docs、`/mcp` 已对齐官方（2026-11-21），见 §1.1 |
| better-auth 端点（前端实际调用） | 10 | 10 | — | 0 | 其余官方端点前端不调用，Go 统一 501 兜底，见 §2 |
| Socket 事件（客户端→服务端） | 76 | 71 | — | 5 | proxy/remote-browser 已补齐（2026-11-21）；缺失只剩 cloudflared×5（明确不做），见 §3 |
| Socket 推送（服务端→客户端） | 23 | 19 | 3 | 1 | domainInfo/refresh/initServerTimezone 已补齐（2026-11-21）；仅剩 importantHeartbeatList（前端不监听），见 §4 |

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

### 1.2 缺失（1 条）

| 官方路由 | 官方位置 | 影响 | 补齐成本 |
| --- | --- | --- | --- |
| `GET /screenshots/*`（静态） | uptime-kuma-server.ts:230 | real-browser 截图外链 | n/a：Go 的 real-browser 走本地 Chromium / 远程 Playwright，均不落截图产物（已知等价偏差） |

> `ALL /mcp` ✅ 2026-11-21 已补齐（`httpapi/mcp.go`：手写 JSON-RPC 2.0 over streamable HTTP）。
> 协议行为按官方 SDK 实测对齐：SSE 单条 `event: message` 帧、initialize 版本协商（支持集合内回显，
> 否则回落 2025-11-25）、`tools/{listChanged}` 能力、notifications 回 202、未知方法 -32601、
> 未知工具 -32602；门禁沿用 requireApiKey（先鉴权再校验方法，故 GET /mcp 无 key 回 401 而非 405）。
> 工具严格照官方"无路径参数的 GET 操作"桥接规则，目前只有 `get_info` / `get_monitors`。
> 已知偏差：坏 JSON 官方是 express.json() 的 400 空 body，Go 给规范化 -32700 "Parse error"。

> `GET /api/status-page/:slug/badge` ✅ 2026-11-21 已补齐（`httpapi/badge.go handleStatusPageBadge`：
> 聚合 public 分组最新心跳，Maintenance > Degraded > Up > Down 官方同序，PENDING 忽略，
> 无心跳/无此 slug 渲染无 label 的灰色 N/A 单段徽章；upColor/downColor/partialColor/
> maintenanceColor/label 参数全支持）。

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
| `checkDomain` | ✅ | 2026-11-21 补齐真实现：四层校验（类型清单/空目标/非 ICANN/无 RDAP 端点）对齐官方 checkSupport，错误回 i18n key + msgi18n + meta，见 §3.2 domain_expiry |
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
| proxy（2） | `addProxy` / `deleteProxy` | ✅ 2026-11-21 补齐（对齐 proxy-socket-handler.js：Saved./successDeleted 回包、default 置换、applyExisting 批量指派 + 广播 monitorList、删除级联置空 monitor.proxy_id）；HTTP 家族 checker 按 `proxy_id` 走代理（http/https CONNECT 隧道、socks5/socks5h/socks 走 SOCKS5、socks4 自实现，官方每次探测 R.load 的语义 → Go 每次 attachProxy 现查） |
| remote-browser（3） | `addRemoteBrowser` / `deleteRemoteBrowser` / `testRemoteBrowser` | ✅ 2026-11-21 补齐（对齐 remote-browser-socket-handler.js：Saved./successDeleted 回包、删除级联置空 monitor.remote_browser、testRemoteBrowser 走真实 Playwright 握手）；real-browser 监控按 `remote_browser` 连远程端点——Go 自带 Playwright 浏览器服务器协议极小客户端（initialize/newContext/newPage/goto，gorilla/websocket 实现，协议形状按官方 1.5x 客户端实测报文校准），探测语义与官方 real-browser-monitor-type.js:264 一致（200-399 → UP、msg=状态码；其余 → DOWN） |
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
| `domainInfo` | ✅ | 2026-11-21 补齐：心跳后 RDAP 查询域名到期日（一天缓存、IANA 映射表周缓存+落库+内嵌兜底），60 秒节流推送 `(monitorID, daysRemaining, expiresOn)`；domain_expiry_notification 开启时按 domainExpiryNotifyDays 阈值发通知。E2E 与 Verisign RDAP 逐秒一致 |
| `proxyList` | ✅ | 登录推送 + 增删后推送真表（2026-11-21 起，proxy CRUD 已补齐，见 §3.2） |
| `remoteBrowserList` | ✅ | 登录推送 + 增删后推送真表（2026-11-21 起，remote-browser CRUD 已补齐，见 §3.2） |
| `dockerHostList` / `maintenanceList` / `notificationList` / `statusPageList` / `monitorTypeList` / `stats` | ✅ | |
| `refresh` | ✅ | 2026-11-21 补齐：disconnectOtherSocketClients 与「启用鉴权」踢人前先发 refresh（前端 location.reload），对齐官方 disconnectAllSocketClients |
| `initServerTimezone`（推送） | ✅ | 2026-11-21 补齐：登录后设置未置位则推送，前端回传浏览器时区并落库（server.js:1562 同语义） |
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
| B2 | ~~Proxy 管理（socket 事件 + checker 消费 `proxy_id`）~~ | ✅ 2026-11-21 已补齐：`addProxy`/`deleteProxy` + 真实 `proxyList`；HTTP 家族探测按 `proxy_id` 走 http/https/socks5/socks5h/socks/socks4 六协议（socks-proxy-agent 的 lookup 语义：socks5 本地解析、socks5h/socks 远程解析、socks4 仅 IPv4）；官方 `active \|\| true` 恒真语义保持一致（前端"启用"开关官方本就不生效）。E2E 验证：建代理→监控心跳走代理（代理计数增长）→删代理→心跳回退直连→applyExisting 批量指派 |
| B3 | ~~Remote Browser 管理（3 事件）~~ | ✅ 2026-11-21 已补齐：`addRemoteBrowser`/`deleteRemoteBrowser`/`testRemoteBrowser` + 真实 `remoteBrowserList`；real-browser 监控可连远程 Playwright 端点（browserless / launchServer），Go 自实现协议极小客户端，E2E 走真 Chromium 验证 msg=200 |
| B4 | Cloudflared Tunnel（5 事件） | **明确不做**（2026-11-21 决策）：部署机有公网直连，隧道无使用场景；官方实现还要管理 cloudflared 二进制进程，收益/成本比最低。Cloudflared 设置页在 Go 后端下不可用 |
| B5 | ~~`/api/docs` + `/api/openapi.json`~~ | ✅ 2026-11-21 已补齐：OpenAPI 3.1.0 文档（get_info/get_monitors + Error/Info/Tag/Monitor schema + bearerAuth，对齐官方 zod-openapi 产物）、Scalar 文档页（CDN 挂载 + data-url）；门禁照官方 requireApiAccess/requireDocsAccess（apiEnabled 403、无凭证 401，JSON/纯文本两种错误形态）；顺带对齐 /api/info 的 runtime 与 `+08:00` 时区偏移、tags 补 monitor_id |
| B6 | ~~`/api/status-page/:slug/badge`~~ | ✅ 2026-11-21 已补齐：整体徽章四态（Up/Degraded/Maintenance/N-A）官方同序判定，E2E 验证混合→黄 Degraded、全 UP→绿、坏 slug→灰 N/A |
| B7 | ~~`domainInfo` + checkDomain（RDAP）~~ | ✅ 2026-11-21 已补齐：`internal/domainexpiry` 包（IANA rdap/dns.json 周缓存+settings 落库+官方 extra/rdap-dns.json 内嵌兜底）、checkDomain 四层校验错误 i18n、checkExpiry 一天缓存、domainExpiryNotifyDays 阈值通知、60 秒节流 domainInfo 推送。E2E 真打 Verisign RDAP，example.com 到期日逐秒一致；`.gov.cn` 无 RDAP 端点按官方报 no-rdap |
| B8 | ~~`/robots.txt`、`/.well-known/change-password`~~ | ✅ 2026-11-21 已补齐：robots 跟随 searchEngineIndex（未开启全站禁抓、text/plain）、change-password 302 到官方 wiki |
| B9 | ~~`refresh` / `initServerTimezone` 推送~~ | ✅ 2026-11-21 已补齐：首登推时区初始化、踢人前 refresh 让被踢端自动刷新；E2E 验证首登收到、二次登录不再推、被踢端收 refresh 后断开 |
| B10 | ~~`/mcp`~~ | ✅ 2026-11-21 已补齐：JSON-RPC 2.0 over streamable HTTP，桥接 get_info/get_monitors；官方 MCP SDK 客户端交叉验证通过（connect/listTools/callTool/未知工具报错 5/5） |
| B11 | `importantHeartbeatList` 推送 | 前端不消费，仅影响潜在移动端/API 消费者 |
| B12 | `/metrics` 指标集（**归类修正**：原写在 C 级"等价实现"，实际是功能缺失） | 官方输出 7 个标准指标 `monitor_status` / `monitor_response_time` / `monitor_response_time_seconds` / `monitor_uptime_ratio`（1d/30d/365d 三档 window）/ `monitor_cert_days_remaining` / `monitor_cert_is_valid`，标签为 `monitor_id/name/type/url/hostname/port` + 每个 tag 名的动态 label；Go 只有 3 个自订 `gokuma_*` gauge。**后果**：照官方文档配的 Grafana 面板与告警规则在 Go 后端下全部取不到数据 |
| B13 | ~~MariaDB / MySQL 存储后端~~ | ✅ 2026-11-21 已补齐（外部库）：`UPTIME_KUMA_DB_TYPE=mariadb` + `_HOSTNAME/_PORT/_NAME/_USERNAME/_PASSWORD/_SOCKET/_SSL/_CA/_POOL_MAX_CONNECTIONS`，四个敏感项支持 `_FILE`（docker secrets，同时设置报错）；启动先 `CREATE DATABASE IF NOT EXISTS ... CHARACTER SET utf8mb4`；连接串强制 `time_zone='+00:00'` + `parseTime=false`，对齐官方 mysql2 `timezone:"Z"` 与 DATETIME 原样返回；`schema_mysql.sql` 与 SQLite 逐表逐列一致（`TestSchemaParity`），时间列 `DATETIME(3)/TIMESTAMP(3)` 保毫秒，TEXT 不做主键、保留字全加反引号、索引内联；方言分支仅 6 处（information_schema 查表/列、两份 schema、`ON DUPLICATE KEY UPDATE`、`GREATEST`、`CAST AS SIGNED`、`[ident]`→反引号改写）。**真库联调已完成（2026-11-21，MariaDB 10.x 外部库）**：`TestMariaDBIntegration` 跑通建库→34 张表→列/索引比对→毫秒精度→`+00:00` 会话时区→utf8mb4→分钟聚合 UPSERT（GREATEST 算法）→小时滚动→事务批量写→重启接管；`e2e/mysql_smoke.js` 跑通真实服务的安装/心跳/CRUD/删除全链路。真库抓出并修掉 5 个 SQLite 才宽容的问题：`TIMESTAMP` 隐式默认值（1067，时间列全改 `DATETIME(3)`）、`expiresAt != ''` 触发 1292（改 `LENGTH()`）、字符串 userID 写入 `INT` 列（1265，统一 `numericUserID`）、`VACUUM` 在 MySQL 是语法错误、事件处理器异步注册导致首个事件被判 "Unsupported event"。**未做**：`embedded-mariadb`（镜像内置 mariadbd 那套）；SQLite↔MariaDB 之间没有数据迁移工具（官方也没有） |

### C. 等价实现（行为一致，机制不同）

- monitor 列表增量更新 → 全量广播；heartbeatLists 聚合 → 逐监控 heartbeatList；前端处理结果相同。
- badge 缓存：官方 apicache 5 分钟，Go 发 `no-store`（实时性更高，QPS 高时开销更大）。
- 静态资源：官方 express-static-gzip（brotli 预压缩），Go net/http 原生（无 br，有 gzip 需反代开启）。
- badge SVG 渲染：官方 `badge-maker` 用 Verdana 精确字宽表 + 支持 `style=flat|flat-square|plastic|for-the-badge`；
  Go 自实现按 6.6px/字符估算宽度（`renderBadge`），**忽略 `style` 参数**。图形语义（颜色/文案/两段式）一致，尺寸与边距有像素级差异。
- `/api/info`：官方 `latestVersion` 是启动时查 GitHub Releases 的结果、`isContainer` 读 `UPTIME_KUMA_IS_CONTAINER` 环境变量；
  Go 分别取自身版本号、恒 `true`。字段形状一致，值语义不同（Go 不做外网版本探测）。
- MCP 传输层：坏 JSON 官方是 `express.json()` 的 400 空 body，Go 给规范化 `-32700 "Parse error"`；
  请求体上限 Go 4MB vs express 默认 100KB。JSON-RPC 语义与工具结果完全一致。

### D. 占位（有意不实现，返回合法空结果）

`getGameList`、`getPM2ProcessList`（官方 PM2 进程管理，Node 专属）、`testChrome`、better-auth admin/HIBP/forget-password 端点（501）。

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

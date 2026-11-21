# go_server — Uptime Kuma 的 Go 后端

用 Go 重写 Uptime Kuma 的后端，**前端零改动复用**：直接托管原有 `dist/`，
通过自研的 Socket.IO v4 协议层与前端通信。

目标不是"另一个监控系统"，而是"同一个系统的低开销实现"：
单静态二进制、常驻内存几十 MB、不依赖 Node 运行时。

```
uptime-kuma/
├── src/ public/ index.html   # 原前端（不改）
├── server/                   # 原 Node 后端（保留作对照，不再维护）
└── go_server/                # ← 本目录
```

---

## 为什么可以"前端零改动"

前端通过 socket.io 交换约 80 个事件。只要 Go 侧实现了 Engine.IO v4 + Socket.IO v4
的协议子集（默认命名空间、JSON 事件、ack 回调），再加上 better-auth 的 8 个 HTTP
端点，前端就完全感知不到后端换了语言。

相关实现：

| 文件 | 作用 |
|---|---|
| `internal/socketio/packet.go` | 协议编解码 |
| `internal/socketio/server.go` | polling / websocket 传输、升级、心跳 |
| `internal/socketio/socket.go` | 事件注册、emit、ack |
| `internal/httpapi/auth.go` | better-auth 兼容层 |
| `internal/handlers/` | 80 个事件的业务实现 |

---

## 快速开始

### 1. 构建前端

```bash
cd ..            # 仓库根目录
npm ci
npm run build    # 产出 dist/
```

### 2. 编译并运行

```bash
cd go_server
go mod tidy      # 首次需要联网拉依赖
make build
./go_server -web-dir ../dist -debug
```

打开 http://localhost:3001 ，首次进入会引导创建管理员账号。

### 3. Docker

构建上下文必须是**仓库根目录**：

```bash
docker build -f go_server/Dockerfile -t uptime-kuma-go:dev .
docker run -d --name uptime-kuma-go \
  -p 3001:3001 \
  -v $(pwd)/data:/app/data \
  uptime-kuma-go:dev
```

要连 MariaDB（或者直接用现成镜像）就用示例编排，数据库是**独立容器**：

```bash
cd go_server
cp .env.example .env      # 改掉里面两个 CHANGE_ME 密码
docker compose -f docker-compose.example.yml up -d
```

`docker-compose.example.yml` 里带有 `healthcheck` + `depends_on: service_healthy`，
应用会等数据库真正可连接后才启动；应用账号只有 `kuma` 库的权限（实测这种授权
足以通过启动时的 `CREATE DATABASE IF NOT EXISTS`，因为库已由镜像初始化好）。

---

## 配置

环境变量与命令行参数同时支持，命名与官方版本保持一致，现有 `docker-compose.yml` 可直接复用。

| 环境变量 | 命令行 | 默认值 | 说明 |
|---|---|---|---|
| `UPTIME_KUMA_PORT` / `PORT` | `-port` | 3001 | 监听端口 |
| `UPTIME_KUMA_HOST` / `HOST` | `-host` | 空（双栈） | 监听地址 |
| `DATA_DIR` | `-data-dir` | `./data` | 数据目录，库文件为 `kuma.db` |
| `UPTIME_KUMA_WEB_DIR` | `-web-dir` | `./web/dist` | 前端静态资源 |
| `UPTIME_KUMA_SSL_KEY` / `SSL_KEY` | `-ssl-key` | — | TLS 私钥 |
| `UPTIME_KUMA_SSL_CERT` / `SSL_CERT` | `-ssl-cert` | — | TLS 证书 |
| `UPTIME_KUMA_AUTH_SECRET` | — | 自动生成并入库 | 会话签名密钥 |
| `UPTIME_KUMA_TRUST_PROXY` | `-trust-proxy` | false | 信任 `X-Forwarded-For` |
| `UPTIME_KUMA_DISABLE_AUTH` | — | false | 关闭鉴权 |
| `UPTIME_KUMA_WS_ORIGIN_CHECK` | — | cors-like | 设为 `bypass` 关闭 Origin 校验 |
| `UPTIME_KUMA_MAX_CONCURRENT_CHECKS` | `-max-concurrent-checks` | 20 | 检查并发上限 |
| `UPTIME_KUMA_KEEP_HEARTBEAT_DAYS` | `-keep-heartbeat-days` | 365 | 数据保留天数（心跳 + 聚合；默认跟随 UI 设置 keepDataPeriodDays，显式配置时覆盖，<1 禁用清理） |
| `UPTIME_KUMA_DEBUG` | `-debug` | false | debug 日志 |
| `UPTIME_KUMA_DB_TYPE` | — | `sqlite` | 存储后端：`sqlite` / `mariadb`（也接受 `mysql` 别名） |
| `UPTIME_KUMA_DB_HOSTNAME` | — | 127.0.0.1 | 数据库主机（`mariadb` 模式） |
| `UPTIME_KUMA_DB_PORT` | — | 3306 | 数据库端口 |
| `UPTIME_KUMA_DB_NAME` | — | kuma | 库名，不存在时自动 `CREATE DATABASE ... utf8mb4` |
| `UPTIME_KUMA_DB_USERNAME` | — | 必填 | 用户名，支持 `_FILE` 后缀 |
| `UPTIME_KUMA_DB_PASSWORD` | — | — | 密码，支持 `_FILE` 后缀 |
| `UPTIME_KUMA_DB_SOCKET` | — | — | unix socket 路径，设置后忽略 hostname/port |
| `UPTIME_KUMA_DB_SSL` | — | false | 启用 TLS，支持 `_FILE` 后缀 |
| `UPTIME_KUMA_DB_CA` | — | — | 自定义 CA（PEM），支持 `_FILE` 后缀 |
| `UPTIME_KUMA_DB_POOL_MAX_CONNECTIONS` | — | 10 | 连接池上限，1–100（超界按官方规则回落/截断） |

`_FILE` 后缀用于 docker secrets：`UPTIME_KUMA_DB_PASSWORD_FILE=/run/secrets/db_pass`。
同一项和它的 `_FILE` 版本同时设置会直接报错（与官方 `getEnvOrFile` 一致）。

---

## 存储后端

默认 SQLite（`<DATA_DIR>/kuma.db`），也可以接外部 MySQL / MariaDB：

```yaml
environment:
  - UPTIME_KUMA_DB_TYPE=mariadb
  - UPTIME_KUMA_DB_HOSTNAME=db
  - UPTIME_KUMA_DB_PORT=3306
  - UPTIME_KUMA_DB_NAME=kuma
  - UPTIME_KUMA_DB_USERNAME=kuma
  - UPTIME_KUMA_DB_PASSWORD_FILE=/run/secrets/kuma_db_pass
```

- 最低版本 **MySQL 8.0.13 / MariaDB 10.2**（用到 TEXT 列的表达式默认值）
- 账号需要 `CREATE` 权限（首次启动要建库）；库已存在时可只给该库的读写权限
- 连接串强制 `time_zone='+00:00'`、`parseTime=false`：库里存的是 UTC 字符串，
  驱动不做二次时区换算，与官方 mysql2 的 `timezone:"Z"` + DATETIME 原样返回等价
- 时间列统一 `DATETIME(3)`（不用 `TIMESTAMP`）：MariaDB 在
  `explicit_defaults_for_timestamp=OFF` 下会给同表第二个 `TIMESTAMP NOT NULL` 隐式补
  `'0000-00-00 00:00:00'`，严格模式直接 1067；而且 `TIMESTAMP` 只到 2038 年、还会按会话时区换算
- 表名、列名与 SQLite 完全一致（`internal/store/schema_mysql.sql` 由 `TestSchemaParity` 逐表逐列校验）
- **不依赖外键级联**：删除路径显式清理 7 张关联表（`store.MonitorStore.Delete`），
  因此 MySQL 侧不建外键，避免插入顺序与约束名冲突
- `proxy.user_id` / `remote_browser.user_id` 是 `INT NOT NULL`，而 better-auth 的用户 ID 是随机字符串，
  非纯数字时记 0（`handlers.numericUserID`）；直接写字符串在 MySQL 严格模式下报 1265
- `getDatabaseSize` / `shrinkDatabase` 与官方一致：MySQL 下返回 0、收缩是空操作
  （官方 `Database.shrink()` 只对 SQLite 执行 `VACUUM`）
- 账号只需该库的权限即可启动：`CREATE DATABASE IF NOT EXISTS` 对**已存在**的库
  不校验全局 `CREATE`（实测确认），所以 compose 里镜像预建库 + db 级授权就够了

外部库的完整编排见 [`docker-compose.example.yml`](docker-compose.example.yml)
（应用 + `mariadb:11.4` 两个容器，含健康检查、字符集、超时窗口）。

要留在默认 SQLite，把 `UPTIME_KUMA_DB_*` 全部删掉即可，compose 里只剩一个服务：

```yaml
services:
  uptime-kuma:
    image: ghcr.io/dislazy/uptime-kuma-go:latest
    restart: unless-stopped
    ports: ["3001:3001"]
    volumes: ["./data:/app/data"]   # kuma.db 就在这里面
```

**关于 `embedded-mariadb`**：不做，也不打算做。官方镜像把 `mariadbd` 打进同一个
容器、在 `/app/data/mariadb` 起实例，代价是镜像 +400MB，并且数据库的备份、升级、
资源限制、故障排查全和应用绑死（数据库崩溃会带着应用一起重建）。需要"开箱即用"
就在 compose 里加一个 `db` 服务，见上面的示例文件；不需要就继续用 SQLite。

真库验证方式见 [`e2e/README.md`](e2e/README.md)：`TestMariaDBIntegration`（进程内直连，
未设环境变量自动 skip）+ `e2e/mysql_smoke.js`（真实服务 + socket.io 全链路）。

方言差异只在 6 处，都在 `internal/store`：
`information_schema` 查表/列、两份建表脚本、`ON DUPLICATE KEY UPDATE`（替代 `ON CONFLICT`）、
`GREATEST`（替代 `MAX(a,b)`）、`CAST(... AS SIGNED)`（替代 `AS INTEGER`）、
以及执行入口把 SQLite 的 `[ident]` 统一改写成反引号。
`TestNoSQLiteOnlySQLInSources` 会扫描全部源码，防止以后在新代码里写出只在 SQLite 下能跑的 SQL。

---

## 数据兼容

**可以直接把官方 `data/kuma.db` 拷过来用**，反之亦然。

- 表名、字段、状态值（`DOWN=0 / UP=1 / PENDING=2 / MAINTENANCE=3`）完全一致
- 官方 v3 的列名是 camelCase 与 snake_case 混用（redbean 会按属性名自动建列），
  所以 `internal/store/columns.go` 在运行期做列名自适应：写入 `upsideDown` 时，
  如果库里只有 `upside_down`，会自动改名，反之亦然
- 已有库启动时进入"接管模式"，不执行建表脚本，只补建 `_gokuma_meta`
- 密码哈希三种格式全部兼容：better-auth 的 scrypt、v2 的 bcrypt、v1 的 sha1，
  老用户登录成功后自动升级为 scrypt

---

## 已实现

**协议与鉴权**
- Socket.IO v4（polling + 直接 websocket 两种起步方式、升级握手、ack 回调、心跳保活、背压丢弃）
- better-auth 兼容：`sign-in/username`、`get-session`、`sign-out`、`/api/auth/ok`、`POST /api/setup`
- Cookie `<token>.<HMAC-SHA256>`、scrypt 哈希、旧账号自动迁移、登录限流
- 2FA：TOTP 绑定/校验/关闭、10 个备用码（一次性消费）、连错 5 次锁 5 分钟；
  密钥按 better-auth 的对称加密方案（`SHA-256(secret)` → XChaCha20-Poly1305，
  `hex(nonce||ciphertext)`，兼容新版本 `$ba$<ver>$` 信封）落库，能读官方库里的记录

**监控（34 / 34，前端下拉框全量）**
- HTTP / keyword / json-query（状态码区间、关键字取反、JSONata 完整表达式、自定义头、
  Basic Auth、mTLS、忽略证书、重定向次数、cache bust、响应体长度限制）
- TCP / port、Ping（系统 ping 命令）、DNS（A/AAAA/CNAME/MX/TXT/NS/PTR/SRV/PTR，可指定上游）
- MySQL / PostgreSQL / SQL Server / Oracle（内置单值查询与条件判定，
  SQL Server 支持前端预填的 ADO 连接串，Oracle 支持 `host:port/service` 裸地址）
- Redis（自写 RESP，支持 `redis://` `rediss://` `redis+unix://`）
- MQTT（自写 3.1.1，支持 TCP 与 WebSocket 传输，keyword / json-query / conditions 三种判定）
- MongoDB（runCommand + JSON 路径断言）、RabbitMQ（Management API 健康检查）
- WebSocket（升级握手 + 关闭码判定）、SMTP（secure / starttls / nostarttls）
- gRPC（`protoparse` 动态解析用户 .proto，动态 message 做 JSON↔protobuf，不需要预编译 stub）
- NTP（自写 SNTP，校验 stratum / 偏移 / root dispersion 三个阈值）、SIP OPTIONS（先 UDP 后 TCP）
- SFTP（密码 / 私钥 / 私钥+口令）、SNMP（v1/v2c/v3）
- Steam（Web API + A2S 过滤）、GameDig（Minecraft Java/Bedrock、Source A2S_INFO，其余降级 TCP）
- Globalping（公共 API 的 ping/http/dns 三种测量）
- Docker（Engine API：unix socket / tcp / mTLS，按 Running/Paused/Restarting/Health 判定；
  配套 `docker_host` 表与 `dockerHostList`/`addDockerHost`/`deleteDockerHost`/`testDockerHost`）
- RADIUS（自写 Access-Request，含 User-Password 混淆与 Response Authenticator 校验）
- Kafka Producer（franz-go，支持 plain / scram-sha-256 / scram-sha-512 与 TLS）
- real-browser（`-tags realbrowser` 时直调 Chromium `--headless=new --dump-dom`，零额外依赖）
- system-service（systemctl is-active）、tailscale-ping；pm2 明确返回不支持
- Push（被动推送）、Group（按子监控最差状态聚合）、Manual
- 调度器：单 goroutine 时间轮 + worker 池，重试窗口（PENDING）、`upside_down`、`downCount`

**数据**
- schema 与官方 knex 迁移对齐；启动时按 `schema.sql` 声明自动补列，
  接管旧库（含官方库）无需手工迁移
- 列名自适应：官方 v3 库里 camelCase 与 snake_case 并存，运行期按 `PRAGMA table_info` 解析
- `monitor.config` JSON 列的读写（2026-10 起 `systemServiceName` 这类字段都收进来了）
- 心跳批量写入（500ms / 100 条合并一次事务，WAL 模式）
- `stat_minutely → stat_hourly → stat_daily` 三级聚合与自动降采样
- uptime / avgPing 实时统计并随心跳下发

**通知（111 / 111，与前端 `NotificationFormList` 逐键对应）**
- 引擎：并行投递、单渠道超时、状态变化才触发、测试消息不渲染成 Down 告警
- 表驱动：一个渠道一行 `registerFunc`，`ContextProvider` 接口给需要区分 UP/DOWN
  标题或 trigger/resolve 的渠道用，纯文本渠道零改动
- 覆盖 Telegram、Slack、Discord、钉钉、飞书、企业微信、阿里云短信、短信类全家桶、
  Matrix、Mattermost、PagerDuty、Opsgenie、Splunk、Twilio、Signal、Nostr（NIP-17/59 gift wrap）、
  Web Push（VAPID）等，清单见 `internal/notify/registry_test.go`

**监控条件（conditions）**
- 14 个操作符（equals / contains / starts_with / num_* / lt / gt …）
- 无限层级分组与 `and` / `or` 组合，`andOr` 挂在子节点上，语义与官方一致
- 前端是否显示条件面板由 `monitorTypeList[type].supportsConditions` 决定

**维护窗口**
- 六种策略全支持：`manual` / `single` / `cron` / `recurring-interval` /
  `recurring-weekday` / `recurring-day-of-month`
- 自研 5 字段 cron 求值器（`internal/cron`，支持 `L` 月末、`*/n`、`a-b/n`、`@daily` 等宏）
- 状态机 `inactive / scheduled / under-maintenance / ended`，每秒比对并在变化时广播
- 维护中监控不发起真实检查，直接写 `status=3, msg="Monitor under maintenance"`

**状态页**
- `status_page` / `status_page_cname` / `incident` / `group` / `monitor_group` 全套 CRUD
- 10 个 socket 事件：增删改查、公告的发布/编辑/解决/取消置顶/历史分页
- 公开接口：`/api/status-page/:slug`、`/heartbeat/:slug`、`/incident-history`、`/manifest.json`
- `/status/:slug/rss`（RSS 2.0，状态文案与官方 `overallStatus` 一致）、
  `/api/badge/:id/:type`（支持 label/upLabel/downLabel/pendingLabel/maintenanceLabel/*Color/value）
- 自定义域名绑定、PNG logo 落盘到 `/upload/`、按 Host 解析 domain → slug

**接口**
- `/api/push/:token`、SVG 徽章、`/api/health`、`/metrics`
- 监控、通知、标签、设置、维护窗口、状态页、Docker 主机的增删改查与实时广播

---

## 已知差异与未实现

| 项 | 说明 |
|---|---|
| `pm2` 监控 | 不实现。pm2 是 Node 专属进程管理器，Go 侧无法管理它的进程表；入口保留并返回明确提示 |
| JSONata 沙箱 | 官方用 `node:vm` 跑 jsonata.js；这里用纯 Go 的 `jsonata-go`，个别边缘语法可能不一致 |
| real-browser | 需要 `-tags realbrowser` 且机器上有 Chromium；默认构建返回"需重建"提示 |
| Web Push | 代码完成、VAPID 密钥自动生成并落库，但**未做浏览器端实测** |
| embedded-mariadb | 明确不做（不把 `mariadbd` 塞进应用镜像）。要"一个文件起全栈"就用 [`docker-compose.example.yml`](docker-compose.example.yml) 里的独立 `db` 服务 |
| `/metrics` | 指标集未对齐官方 7 个标准指标（见 API-PARITY 的 B12） |

## 代码结构

```
cmd/go_server/main.go      装配与优雅退出
internal/config            环境变量 / 参数
internal/store             SQLite / MySQL 双后端：列名自适应、批量写、聚合、方言改写
internal/model             跨包共享类型与状态常量
internal/auth              scrypt / bcrypt / sha1、会话 Cookie、TOTP
internal/socketio          Engine.IO v4 + Socket.IO v4
internal/hub               连接注册与广播
internal/handlers          100+ 个 socket 事件
internal/httpapi           better-auth、setup、push、badge、health、metrics
internal/monitor           调度器、执行、聚合任务、推送处理
internal/checkers          各监控类型探测器（注册表模式）
internal/notify            投递引擎与渠道实现
internal/web               SPA 静态托管
```

---

## 开发约定

- 新增监控类型：在 `internal/checkers` 写一个实现 `Checker` 的文件，`init()` 里 `Register`，无需改调度器；
  同时在 `internal/checkers/types_test.go` 的 `frontendMonitorTypes` 里补一行，防止漏注册
- 新增通知渠道：在 `internal/notify` 写一个实现 `Provider` / `ContextProvider` 的结构体，
  按渠道名 `registerFunc`／`Register`；`internal/notify/registry_test.go` 会校验前端键名
- 新增 socket 事件：在 `internal/handlers` 写 `func(*Ctx) (any, error)`，然后在 `registerEvents` 挂一行
- 字段名一律照抄官方 JS 里的写法，不要"顺手改成 Go 风格"，否则前端读不到
- 协议层改动必须用真实 `socket.io-client` 验一遍（`socket.io-client@4`），curl 只能验 REST
- 写 SQL 的硬规则（两种存储后端都要能跑）：
  - 表名/列名用 `[ident]` 包裹（执行入口会按方言转成反引号），**不要裸写** `interval` /
    `key` / `default` / `group` 这类 MySQL 保留字
  - 不要用 SQLite 专属语法：`PRAGMA`、`sqlite_master`、`ON CONFLICT`、`excluded.`、
    `AUTOINCREMENT`、`CAST(... AS INTEGER)`、`||` 拼接
  - 派生表必须带别名（`FROM (SELECT ...) AS t`），MySQL 没别名直接报 1248
  - 新增表/列要**同时**改 `schema.sql` 和 `schema_mysql.sql`，`TestSchemaParity` 会拦住漏改

## 已知限制

- `ping` 与 `gamedig` 依赖容器内的 `ping` 命令（镜像已装 `iputils`）
- ICMP 在某些受限环境需要 `NET_RAW` 能力
- 前端构建仍需 Node（只在构建期，运行期不需要）
- `kafka-producer` 用 franz-go，二进制因此变大（约 +12MB）；`docker` 需要宿主可访问
  docker socket 或已配置 `data/docker-tls/<host>/{ca,cert,key}.pem` 三件套

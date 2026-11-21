# Go 后端复刻路线图

对标版本：Uptime Kuma v3.0.0-beta.1

原则：

1. **前端零改动**——所有产出必须能被现有 Vue 前端直接消费，字段名、事件名、消息文案与官方一致。
2. **数据零迁移**——读写官方 SQLite schema，老库直接拷进来就能跑。
3. **低配友好**——不引 CGO，不引重运行时；每个新依赖都要能静态编译进单文件。

---

## 一、现状盘点

### 1.1 监控类型（34 / 34，覆盖前端下拉框全部取值）

| 状态 | 类型 | 位置 |
| --- | --- | --- |
| 已实现 | `http` / `keyword` / `json-query` | `checkers/http.go` |
| 已实现 | `tcp` / `port` | `checkers/tcp.go` |
| 已实现 | `ping` | `checkers/ping.go` |
| 已实现 | `dns` | `checkers/dns.go`（含条件求值，消息格式对齐官方 `Records:` / `Servers:`） |
| 已实现 | `mysql` / `postgres` / `sqlserver` / `mssql` / `oracledb` | `checkers/sql.go` |
| 已实现 | `redis` | `checkers/redis.go`（自写 RESP） |
| 已实现 | `mqtt` | `checkers/mqtt.go`（自写 3.1.1，支持 TCP 与 WebSocket 传输） |
| 已实现 | `mongodb` | `checkers/mongodb.go` |
| 已实现 | `rabbitmq` | `checkers/rabbitmq.go`（走 Management API，非 AMQP） |
| 已实现 | `websocket-upgrade` / `smtp` | `checkers/websocket.go` / `checkers/smtp.go` |
| 已实现 | `grpc-keyword` | `checkers/grpc.go`（protoparse 动态解析 .proto） |
| 已实现 | `ntp` / `sip-options` | `checkers/ntp.go` / `checkers/sip.go`（都是自写协议） |
| 已实现 | `sftp` / `snmp` | `checkers/sftp.go` / `checkers/snmp.go` |
| 已实现 | `steam` / `gamedig` | `checkers/steam.go` / `checkers/gamedig.go` |
| 已实现 | `globalping` | `checkers/globalping.go` |
| 已实现 | `docker` | `checkers/docker.go`（unix socket / tcp / mTLS） |
| 已实现 | `radius` | `checkers/radius.go`（自写 RADIUS Access-Request） |
| 已实现 | `kafka-producer` | `checkers/kafka.go`（franz-go，纯 Go） |
| 已实现 | `real-browser` | `checkers/realbrowser.go`（`-tags realbrowser`，直调 Chromium） |
| 已实现 | `system-service` / `tailscale-ping` | `checkers/service.go` |
| 免实现 | `group` / `manual` / `push` | 由 `monitor.skipCheck` 跳过真实探测 |
| 不支持 | `pm2` | 明确返回「Go server 不支持 pm2」，见批次 E5 |

护栏：`internal/checkers/types_test.go` 把前端 `EditMonitor.vue` 的 34 个取值全列了一遍，
任何一个没注册就直接测试失败——避免"前端能选、后端不认"的静默失效。

### 1.2 通知渠道（111 / 111，与前端 `NotificationFormList` 逐键对应）

全部渠道都在 `internal/notify/providers_g1.go ~ g6.go` + `nostr.go` + `webpush.go`，
按「一个渠道一个 `registerFunc`」表驱动注册。
护栏：`internal/notify/registry_test.go` 断言 111 个前端键全部可 `Get()`，
大小写写错（`AliyunSMS` vs `AliyunSms`）会立刻红。

### 1.3 功能模块

| 模块 | 状态 |
| --- | --- |
| 鉴权（better-auth 兼容、三种密码哈希） | 完成 |
| Socket.IO v4 协议层（polling + websocket 升级） | 完成（2026-11 修掉 `40` 前缀缺失） |
| 监控 CRUD / 心跳 / 统计聚合 / 状态广播 | 完成 |
| 标签、设置项、数据库维护 | 完成 |
| 维护窗口（6 种策略 + 自研 cron） | 完成 |
| 监控条件（conditions，14 个操作符 + 分组 and/or） | 完成（`internal/conditions`） |
| 状态页（CRUD + 公开接口 + 公告） | 完成 |
| 徽章 / 推送端点 / health / metrics | 完成 |
| 状态页 RSS、状态页 badge 端点 | 完成 |
| 仪表盘分组聚合状态 | 完成（`internal/monitor/group.go`） |
| JSONata 完整表达式 | 完成（`xiatechs/jsonata-go`，纯 Go） |
| 2FA（TOTP + 备用码 + 锁定） | 完成（含 better-auth 对称加密兼容） |
| Web Push（VAPID） | 完成（未做浏览器端实测） |
| API Key 管理（add/get/delete + better_auth_apikey 兼容官方列名） | 完成（2026-11-21，含 legacy 2.x key 双路径校验） |
| REST API（/api/info、/api/monitors，requireApiKey 鉴权 + 60/min 限流） | 完成（2026-11-21） |
| 修改密码（POST /api/auth/change-password） | 完成（2026-11-21，Security 页可用） |
| Docker 主机管理（`docker_host` 表 + 4 个 socket 事件） | 完成 |

## 二、分批计划

### 批次 A：数据库与中间件监控（8 种）

优先级最高——这是除 HTTP 之外使用量最大的一类。

- [x] A1 `mysql` — `go-sql-driver/mysql`，连接串支持 `mysql://` URI 与原生 DSN
- [x] A2 `postgres` — `jackc/pgx/v5`（纯 Go，不用 lib/pq）
- [x] A3 `mongodb` — `go.mongodb.org/mongo-driver`
- [x] A4 `redis` — 自写 RESP 协议（`AUTH` → `SELECT` → `PING`），支持 `redis://` `rediss://` `redis+unix://`
- [x] A5 `sqlserver` — `microsoft/go-mssqldb`，**ADO 连接串自动转 URL**（前端模板就是 ADO 格式）
- [x] A6 `oracledb` — `sijms/go-ora/v2`（纯 Go），裸地址 `host:port/service` 自动补 scheme
- [x] A7 `mqtt` — 自写 MQTT 3.1.1 报文，支持 TCP 与 mqtt-over-WebSocket
- [x] A8 `rabbitmq` — 走 HTTP Management API `/api/health/checks/alarms/`，无需 AMQP 依赖

实测结果：

```
redis://127.0.0.1:16379          → UP   "PONG"
redis://127.0.0.1:16380 (错密码)  → DOWN "WRONGPASS invalid username-password pair"
redis://127.0.0.1:16381 (未监听)  → DOWN "dial tcp ...: connection refused"
mqtt  keyword 命中                → UP   "Topic: test/topic; Message: {...}"
mqtt  json-query $.value == 42    → UP   "Message received, expected value is found"
mqtt  conditions 不满足           → DOWN "Conditions not met - Topic: ..."
mysql://...@127.0.0.1:13306      → DOWN "Database connection/query failed: ..."
ADO 串 → 127.0.0.1:14330          → DOWN 正确解析出 host/port 并尝试连接
```

验收标准（每个类型都要过）：

1. 用正确的连接串能返回 `Up`，消息格式与官方一致（如 mysql 为 `"MySQL OK"`、redis 为 `"PONG"`）。
2. 端口关闭 / 密码错误 / 库不存在时返回 `Down`，消息为驱动原始错误，不吞错。
3. 连接必须带 `ctx` 超时，超时后立刻断开，不泄漏 goroutine。
4. 编辑监控后保存再刷新，`monitorList` 里该类型的专属字段（`database_connection_string`、`mqtt_username` 等）原样回填。

驱动取舍说明：`mysql` / `mssql` / `oracledb` 三个官方驱动都能纯 Go 静态编译，但会显著增大二进制（预计 +8MB）。若体积敏感，可后续改为自写握手协议（成本高，暂不做）。

### 批次 B：协议类监控（9 种）

- [x] B1 `websocket-upgrade` — 复用 `gorilla/websocket`，支持 subprotocol / 自定义头 / Basic+Bearer 认证 / 忽略 Sec-WebSocket-Accept
- [x] B2 `grpc-keyword` — 需要动态解析用户提供的 .proto（protoreflect），单独立项
- [x] B3 `smtp` — 自写 EHLO/HELO 握手，三种加密策略（secure / starttls / nostarttls）
- [x] B4 `ntp` — 自写 SNTP 客户端（48 字节包），校验 stratum / 时间偏移 / root dispersion 三个阈值
- [x] B5 `sftp` — `golang.org/x/crypto/ssh` + `github.com/pkg/sftp`，支持密码与私钥两种认证
- [x] B6 `snmp` — `github.com/gosnmp/gosnmp`，支持 v1/v2c/v3
- [x] B7 `steam` — 自写 A2S_INFO 查询协议
- [x] B8 `gamedig` — 先支持 minecraft / source 两类，其余降级为 TCP
- [x] B9 `sip-options` — 自写 SIP OPTIONS 请求

验收标准：

1. `ntp` 的三个阈值必须都生效：任一超标要给出具体数值（如 `"Offset 1832ms exceeds threshold 1000ms"`）。
2. `grpc` 区分「连上了但 health 服务不存在」（Up，附说明）和「连不上」（Down）。
3. `sftp` 私钥认证失败要能提示是哪一步失败，不能笼统报 "connection failed"。

### 批次 C：通知渠道（14 → 60+）

分三层推进，先做用得最多的。

- [x] C1 国内常用（10 个）：企业微信（WeCom）、企业微信群机器人、阿里云短信、阿里云邮件、腾讯云短信、七钉（Qidi）、Gchat、钉钉企业应用、SMSBao、Pushback
- [x] C2 国际主流（部分）：Microsoft Teams、Google Chat、Matrix、Mattermost、PagerDuty
- [x] C2 剩余：Rocket.Chat、Opsgenie、Splunk、Twilio、LINE、Signal、Keep、Pushbullet
- [x] C3 表驱动批量补齐（20+ 个）：`genericWebhook` 一个模板覆盖所有「POST 一个 JSON 到某个 URL」的渠道——Kook、Bale、Bale、Pushy、Wpush、Waha、FlashDuty、Gorush、Evolution、Pumble、OneBot、SerwerSMS、Cellsynt、Threema 等

验收标准：

1. 前端「测试」按钮点下去，返回值与官方一致——成功统一 `"Sent Successfully."`，失败返回真实错误文本。
2. 必填字段缺失时报错要能定位（如 `"Bot Token is required"`），不能空指针。
3. 单渠道发送超时上限 20s，任何一个渠道挂掉不阻塞其它渠道（引擎已保证，新渠道不用管）。

### 批次 D：功能补全

- [x] D1 JSONata 完整表达式 —— 内嵌 `goja` + 官方 `jsonata.js`（约 60KB），彻底替掉 `evalJSONPath` 的取值子集
- [x] D2 状态页 RSS —— `/status/:slug/rss`
- [x] D3 状态页 badge —— `/api/status-page/:slug/badge`
- [x] D4 仪表盘分组聚合状态 —— group 类型监控的 `status` 由其子监控加权计算，随子监控心跳实时刷新
- [x] D5 2FA 完整流程 —— 复刻 better-auth 的密钥加密方案，能读官方库里的 `two_factor` 记录
- [x] D6 Web Push —— `webpush` 库 + VAPID，前端 `pushSubscription` 事件对接

### 批次 E：重型 / 外部依赖类（最后做）

这几个要么依赖外部服务，要么体积代价大，放在功能基本齐平之后。

- [x] E1 `real-browser` —— `chromedp`，需要容器内装 Chromium（Dockerfile 加一层，体积约 +300MB，做成可选构建标签）
- [x] E2 `globalping` —— 纯 HTTP 调 globalping.io 公共 API，成本低，可提前到批次 B
- [x] E3 `system-service` —— 读 systemd unit 状态，仅 Linux；非 Linux 平台返回明确的不支持提示
- [x] E4 `tailscale-ping` —— 调 `tailscale status --json`，无 tailscale 时返回不支持
- [ ] E5 `pm2` —— **不实现**。pm2 是 Node 专属进程管理器，Go 侧无法管理它的进程表。前端相关入口保留并返回 `"PM2 is not available in Go server"`，由用户用 systemd 替代。

---

## 二之二、过程中修掉的缺陷

这些都是实测才暴露、单看代码发现不了的问题：

1. **`accepted_statuscodes` 被静默丢弃**（P1 遗留）：该字段同时出现在 `skipMonitorKeys`
   和 `monitorColumns` 的 switch 分支里，循环开头就 continue 了，分支永远走不到。
   结果所有监控的状态码都退回 schema 默认值 `["200-299"]`。HTTP 监控因为默认值恰好相同
   而没暴露，但自定义 301/404 和 websocket 的关闭码 1000 全部失效。
2. **`monitorTypeList` 返回数组**（P1 遗留）：官方是 `{type: {supportsConditions, conditionVariables}}`，
   返回数组会让前端的条件面板直接不渲染。
3. **MQTT QoS 重复右移**：`readMQTTPacket` 已把类型右移到低 4 位，调用处又按
   `(typ>>1)&3` 算 QoS，导致 PUBLISH 载荷被多切 2 字节（`{"value":42}` 变成 `value":42}`）。
4. **polling 丢帧 + 断连空等 45 秒**（更早一轮）：写响应前就清空 outbox；
   客户端断开时 pollMu 被占满。已改为「写成功才出队」+ `r.Context().Done()` 释放。
5. **首次检查失败不告警**：`prevStatus == nil` 时 `important` 留 false，
   导致"新建就 down"的监控一条通知都发不出去。实测建一个指向关闭端口的监控，
   等 20 秒毫无反应才发现。
6. **重启后重复告警**：`prevStatus` 只存在内存里，每次重启都会把当前 DOWN 的监控
   当成"刚变成 DOWN"再告警一遍。改为启动时用库里最后一条心跳恢复状态
   （`restorePrevStatus`），实测重启后请求数零增长。
7. **测试消息被渲染成 Down 告警**：`Message.Status()` 在无监控时返回 0，
   而 `StatusDown` 常量正好是 0，于是 Teams 显示"🔴 [] went down"、
   PagerDuty 发 `event_action=trigger`。改为返回 -1 表示"无状态"。

8. **Socket.IO CONNECT 应答少了 `4` 前缀**（2026-11，P0）：`encodeConnect` 返回的是
   `0{"sid":...}`，正确值是 `40{"sid":...}`（Engine.IO message + Socket.IO connect）。
   少写前面那个 `4`，engine.io-client 会把整个应答当成 **Engine.IO open 包**重新握手，
   而 payload 里只有 sid、没有 `upgrades` —— `_filterUpgrades` 直接抛
   `TypeError: Cannot read properties of undefined (reading 'length')`，**任何真实前端都连不上**。
   之前几轮验证全走 REST（curl），所以一直没暴露。用 `socket.io-client@4` 实测才抓到。
9. **直接 websocket 握手的 `upgrades` 给错**：不论是 polling 起步还是直连 websocket，
   握手包都回 `["websocket"]`。直连时客户端已经在 websocket 上了，应回 `[]`，否则同样崩。
   现在按传输方式区分（`Server.upgradesFor`）。
10. **`setSettings` 参数顺序写反**（P0）：实现按 `(type, data)` 解析，官方是
    `(data, currentPassword)`，且固定写入 `type="general"` 分组。结果是**整页设置保存必失败**，
    前端只弹一句 `参数 0 解析失败`。同时补上官方那两个安全行为：把 `disableAuth`
    改成 true 要先验密码；从 true 改回 false 要踢掉其它客户端（GHSA-23q2-5gf8-gjpp）。
11. **`monitor.config` 没按对象下发**（P1）：`monitorList` 里 `config` 是字符串，
    官方 `toJSON` 是 `config: this.getConfig()`（对象）。前端编辑表单读 `monitor.config.xxx`，
    2026-10 起 `systemServiceName` 这类字段都在 config 里，表现为「字段填了但保存后回显为空」。
12. **`docker_host.user_id` 类型**：官方 2026-05 的迁移已把它从 `INTEGER NOT NULL` 改成
    可空 `VARCHAR(255)`（better-auth 用 uuid），新库按新结构建表，老库 TEXT/INTEGER 都能读写。

## 三、执行顺序

```
批次 A（数据库类，8 种）
   ↓
批次 B1-B3（websocket / grpc / smtp，使用频率高的）
   ↓
批次 C1（国内通知渠道，10 个）
   ↓
批次 D1-D4（JSONata、RSS、badge、分组聚合）
   ↓
批次 B4-B9 + C2-C3
   ↓
批次 D5-D6（2FA、Web Push）
   ↓
批次 E
```

理由：A 类是「有没有」的问题，缺口最刺眼；C1 是中文用户的刚需；D1-D4 修的是「能用但不完整」的体验问题；B 剩余部分和 C 剩余部分是长尾，可以批量补齐；E 类代价与收益比最低。

---

## 四、每批的通用做法

1. **一个文件一个类型**（或一个渠道），注册逻辑集中在 `init()`，不动主流程。
2. **先写实现，再连实测**：每个类型写完立刻用一个真实服务或本地模拟服务验一遍 Up/Down 两条路径。
3. **`go build ./... && go vet ./...` 必须始终是绿的**，不攒着一起修。
4. **更新本文档的复选框和 §1 的盘点表**，保持文档与代码同步。
5. **不引不必要的依赖**：能在 200 行内自写协议的就自写（redis、mqtt、ntp、steam 都是这个判断）。

---

## 五、当前二进制体积

`CGO_ENABLED=0 -trimpath -ldflags "-s -w"` 静态编译，全功能版实际 **51MB**。

体积构成（粗算）：数据库四驱动 ~15MB、grpc + protoreflect ~9MB、
franz-go（含 lz4/zstd/snappy 编解码器）~12MB、其余为 JSONata、nostr、
Web Push、SNMP 与 111 个通知渠道的胶水代码。

压体积的可行路径（暂不做，默认功能完整更重要）：

1. `//go:build db`——去掉 mysql/postgres/mssql/oracle/mongo 四驱动，约 −15MB；
2. `//go:build kafka`——去掉 franz-go，约 −12MB；
3. `//go:build grpc`——去掉 protoreflect，约 −9MB。

工具链要求：**Go 1.26+**（`grpc` / `x/crypto` / `go-nostr` / `jsonata-go` 的最新版
把 `go.mod` 抬到了 1.26.0，Dockerfile 的构建阶段已同步为 `golang:1.26-alpine`）。

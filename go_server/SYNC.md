# 同步基线（Sync Baseline）

> 这个文件回答一个问题：**官方上游又更新了，我要同步哪些改动到 go_server？**
> 每次同步官方新改动后，更新本文件的基线 commit。

## 当前基线

| 项 | commit | 说明 |
| --- | --- | --- |
| 官方上游基线 | `b0cd0f1d` | `master` 分支，2026-11-20，*fix(settings): use the server's 365-day default for keepDataPeriodDays (#8213)* |
| go_server 基线 | `ab5bd237` | `main` 分支，go_server 全量首发（110 文件 / 29,389 行） |

前端（Vue，仓库根目录的 `src/`）**零改动**，跟随官方 `master` 即可；
需要同步的只有官方 `server/`、`db/`、`src/` 里的**协议与数据结构约定**。

## 怎么找出官方有哪些新改动

```bash
git fetch upstream master        # upstream = louislam/uptime-kuma
# 官方新提交一览
git log b0cd0f1d..upstream/master --oneline
# 只看后端相关文件（前端 UI 改动无需同步）
git diff --stat b0cd0f1d..upstream/master -- server/ db/ src/mixins/socket.js src/components/notifications/ src/pages/EditMonitor.vue
# 看具体 diff
git diff b0cd0f1d..upstream/master -- server/model/monitor.js
```

## 拿到 diff 后，按这张映射表找 go_server 里对应位置

| 官方改动位置 | go_server 对应位置 | 说明 |
| --- | --- | --- |
| `db/patch*.sql`、`db/knex_migrations/*.js` | `internal/store/schema.sql` | 新列加进 schema，`ensureColumns` 会自动给老库补列 |
| `server/model/monitor.js` 内联检查分支 | `internal/checkers/` | docker/radius/kafka 这类不在 `monitor-types/` 目录里的类型也写在这里 |
| `server/monitor-types/*.js` | `internal/checkers/<type>.go` | 常规监控类型 |
| `server/notification-providers/*.js` | `internal/notify/providers_*.go` | 同时要在 `internal/notify/registry_test.go` 补前端键 |
| `server/socket-handlers/`、`server/server.js` 的 `io.on` | `internal/handlers/` + `registerEvents` | 注意官方的消息文案要照抄 |
| `src/pages/EditMonitor.vue` 的 `monitorTypeList` 取值 | `internal/checkers/types_test.go` | 新类型必须进护栏清单 |
| `src/components/notifications/index.js` 的 `NotificationFormList` | `internal/notify/registry_test.go` | 新渠道必须进护栏清单 |
| `src/mixins/socket.js` | `internal/handlers/` | 事件名/参数顺序以这里为准 |
| `src/lang/en.json` | 各 checker/notify 里的消息字符串 | 官方文案保持一致 |
| `db/` schema 里 JSON 列新增 | `internal/handlers/monitor.go` 的 `monitorJSON()` | JSON 字符串字段要 parse 成对象再下发 |

## 同步流程建议

1. `git log b0cd0f1d..upstream/master --oneline` 圈出范围，按映射表分类；
2. 逐条改 go_server，跑 `go build ./... && go vet ./... && go test ./...`；
3. 有 socket 协议改动的，务必用 `socket.io-client@4` 实连验证（curl 只能验 REST）；
4. 更新本文件两个基线 commit 到最新值，一起提交。

## 历史同步记录

| 日期 | 官方基线 | go_server 提交 | 备注 |
| --- | --- | --- | --- |
| 2026-11-19 | `160acac6` | `ab5bd237` | 首次基线 |
| 2026-11-21 | `b0cd0f1d` | 见 main | 3 个上游提交：加泰罗尼亚语言、依赖更新（纯前端，合并即得）；keepDataPeriodDays 修复 → go_server 同步：settings 种子 180→365、prune 改为跟随 UI 设置（缺失/解析失败回落 365，<1 禁用清理，官方 clear-old-data.js 语义），并补上此前缺失的 heartbeat 过期清理（Go 侧原本只清聚合表，heartbeat 无限增长） |

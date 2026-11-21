# e2e

联调脚本。默认不参与 `go test`，都需要真实环境，按需手动跑。

## MySQL / MariaDB 后端

两层验证，互补，建议都跑：

### 1. Go 集成测试（进程内，直连数据库）

```bash
export KUMA_TEST_MARIADB_HOST=mysql.example.com
export KUMA_TEST_MARIADB_PORT=3306
export KUMA_TEST_MARIADB_NAME=uptime_test      # 会被清空，务必用一次性库
export KUMA_TEST_MARIADB_USER=uptime_test_user
export KUMA_TEST_MARIADB_PASS=******

go test ./internal/store/ -run TestMariaDBIntegration -v -count=1
```

覆盖：建库 → 建 34 张表 → 与 `schema_mysql.sql` 逐表逐列逐索引比对 → 毫秒精度
→ 会话时区 `+00:00` → utf8mb4 → 分钟聚合 UPSERT（GREATEST 算法）→ 小时滚动
→ 事务批量写 → 关连接重开走"接管既有库"分支且数据不丢。

未设置 `KUMA_TEST_MARIADB_HOST` 时整条测试 skip，所以 CI 与本机默认 `go test ./...` 都不会连库。

### 2. 端到端脚本（真实服务 + socket.io）

需要 `socket.io-client`（仓库根 `npm i` 里没有，单独装到任意目录后用 `NODE_PATH` 指过去）：

```bash
npm i socket.io-client --prefix /tmp/sio
export NODE_PATH=/tmp/sio/node_modules

export KUMA_BASE=http://127.0.0.1:3001
export KUMA_USER=admin
export KUMA_PASS=******

node go_server/e2e/mysql_smoke.js init     # 初始化管理员 + 加监控 + 等首条心跳
# 重启服务端，验证数据确实在数据库里
node go_server/e2e/mysql_smoke.js verify   # 读回监控与心跳，检查毫秒时间戳
node go_server/e2e/mysql_smoke.js crud     # 设置/标签/代理/状态页分组/维护/APIKey/通知
node go_server/e2e/mysql_smoke.js clean    # 清心跳 + 删监控
```

服务端这样起（数据目录与 SQLite 无关，但 `-data-dir` 仍用于上传/截图等）：

```bash
UPTIME_KUMA_DB_TYPE=mariadb \
UPTIME_KUMA_DB_HOSTNAME=mysql.example.com \
UPTIME_KUMA_DB_NAME=uptime_test \
UPTIME_KUMA_DB_USERNAME=uptime_test_user \
UPTIME_KUMA_DB_PASSWORD=****** \
go run ./cmd/go_server -port 3001 -data-dir /tmp/kuma-mysql
```

`init` 要求库是空的（`/api/setup` 只允许初始化一次）。要重跑先清库，最快的办法是
再跑一遍上面的集成测试——它开跑前会 DROP 掉目标库的所有表。

### 这些脚本在防什么

真实 MySQL 和 SQLite 的"宽容度"不一样，SQLite 能跑的 SQL 在 MySQL 严格模式下会直接报错。
迄今被这两层测试抓到的问题（都已修）：

| 现象 | 根因 |
|---|---|
| `1067 Invalid default value for 'updatedAt'` | MariaDB 下同表第二个 `TIMESTAMP NOT NULL` 被隐式补零值默认 |
| `1292 Incorrect datetime value: '' for column 'expiresAt'` | `WHERE expiresAt != ''` 让 MySQL 把空串转成日期 |
| `1265 Data truncated for column 'user_id'` | better-auth 的字符串 ID 被写进 `INT UNSIGNED` 列 |
| `1064` on `VACUUM` | `shrinkDatabase` 无条件执行 SQLite 专属语句 |
| `Unsupported event: add` | 事件处理器在 goroutine 里异步注册，客户端首个事件被漏掉 |

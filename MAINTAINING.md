# MAINTAINING.md — 维护指南

> 这份文档回答的是**长期怎么养这个仓库**：官方上游更新了怎么跟、Go 后端怎么改、
> 改完怎么验、什么时候发版、出了故障怎么查、哪份文档该在什么时候更新。
>
> 三份配套文档各管一段，别混着看：
>
> | 文档 | 管什么 | 什么时候打开 |
> | --- | --- | --- |
> | **本文** | 维护流程、决策规则、验证清单、发布、排障 | 每次动手前 |
> | [`go_server/SYNC.md`](go_server/SYNC.md) | 同步基线 commit + 历史同步台账 | 每次同步后更新它 |
> | [`go_server/API-PARITY.md`](go_server/API-PARITY.md) | API 逐条对齐状态、已知缺口分级（A/B/C/D/E） | 想知道"还差什么"时 |
>
> 想快速改代码而不是走流程，看 [`go_server/AGENTS.md`](go_server/AGENTS.md)（目录地图 + 硬性约定 + 踩坑）。

---

## 1. 维护模型：三条时间线

```
upstream (louislam/uptime-kuma)      官方 Node 后端 + Vue 前端，只读
        │  git fetch + merge --ff-only
        ▼
     master                          纯官方，本地不改，作为 diff 参照
        │  git merge
        ▼
      main                           官方代码 + go_server/ + CI，日常开发在这
        │  git push
        ▼
   GitHub Actions                    构建前端 + 交叉编译 + 冒烟 + 推镜像
        ▼
ghcr.io/dislazy/uptime-kuma-go       部署产物
```

| 时间线 | 分支/产物 | 规则 |
| --- | --- | --- |
| 官方上游 | `upstream/master` | 只读，永不改 |
| 镜像基线 | `master` | **只做快进合并**（`--ff-only`），必须与 `upstream/master` 逐字节一致，作为可回退的干净参照 |
| 工作分支 | `main` | 官方代码 + `go_server/` + CI。所有开发、文档、修复都在这里 |
| 发布物 | `ghcr.io/dislazy/uptime-kuma-go` | 由 CI 构建，人工不本地打镜像 |

**为什么 `master` 必须干净**：同步上游时，`git diff master..upstream/master` 就是"官方这次改了什么"的权威答案。一旦 `master` 混进自己的提交，这个 diff 就失去意义，以后每次同步都要人工排除干扰。

**前端零改动的含义**：`src/`、`public/`、`package.json` 都由官方控制，`main` 合并 `master` 就自动跟着走。需要同步到 Go 的只是**协议与数据结构约定**（socket 事件、REST 路由、DB schema、文案），不是 UI。

---

## 2. 维护节奏：什么时候该动

| 触发 | 动作 | 优先级 |
| --- | --- | --- |
| 上游发了 **security 修复**（CVE / GHSA） | 立刻同步，先看 diff 再动手 | 最高 |
| 上游改了 **DB schema / socket 事件 / REST 路由** | 同步 + 补齐 Go 侧 + 更新 API-PARITY | 高 |
| 上游改了**监控类型或通知渠道** | 同步 + 加 checker/provider + 护栏清单 | 中 |
| 上游只改前端 / 依赖 / 语言包 | 只合并 `master`，Go 侧不动 | 低（可攒着） |
| 自己发现 bug / 想加功能 | 直接改 `go_server`，不用等上游 | 按需 |
| 上游发正式版本（v3.x） | 考虑跟随版本号打 tag 发镜像 | 按需 |

建议节奏：**每月扫一次上游**，安全相关的事件驱动。攒着不影响的改动再一起合，避免天天折腾。但**不要长期不合并**——拖太久冲突会集中在 `package-lock.json` 和前端组件上，虽然与后端无关，但合并成本会变得很烦。

---

## 3. 上游同步 SOP

### 3.1 拉取与合并（5 步）

```bash
cd ~/songbo/uptime-kuma   # 本仓库

# 1) 更新基线分支（只快进）
git fetch upstream
git checkout master
git merge --ff-only upstream/master
git push origin master

# 2) 合并进工作分支
git checkout main
git merge master

# 3) 看官方这次改了什么（这才是权威 diff）
git diff --stat b0cd0f1d..master -- server/ db/ src/mixins/socket.js \
    src/components/notifications/ src/pages/EditMonitor.vue src/lang/en.json
git log b0cd0f1d..master --oneline

# 4) 按 3.2 分类处理

# 5) 更新 go_server/SYNC.md 的两个基线 commit + 台账，一起提交
```

> `b0cd0f1d` 是**当前基线**，每次同步后换成新的 `master` commit。这个值只认 `go_server/SYNC.md` 里的记录。

### 3.2 拿到 diff 后怎么分类

不是所有改动都要同步。按这张表判断：

| 官方改动 | 要不要同步到 Go | 处理方式 |
| --- | --- | --- |
| `db/patch*.sql`、`db/knex_migrations/*.js` 加列/加表 | **必须** | 新列进 `internal/store/schema.sql`；`ensureColumns` / `ensureTables` 会自动给老库补齐。**注意 `ALTER ADD COLUMN` 不支持无默认值的 NOT NULL** |
| `server/model/monitor.js` 内联分支（check 逻辑） | **必须** | 对应的 `internal/checkers/` |
| `server/monitor-types/*.js` | **必须** | 新增/修改 `internal/checkers/<type>.go` + `types_test.go` 护栏 |
| `server/notification-providers/*.js` | **必须** | `internal/notify/providers_*.go` + `registry_test.go` 前端键护栏 |
| `server/socket-handlers/`、`server/client.js` 里的 `socket.on` | **必须** | `internal/handlers/`；**官方回包文案要照抄**（前端会按字符串判断） |
| `src/mixins/socket.js` 的事件名/参数顺序 | **必须** | 以它为准（前端是权威消费方） |
| REST 路由（`server/routers/`、`server.js` 的 `app.get`） | **必须** | `internal/httpapi/`；改完更新 `API-PARITY.md` 的 REST 表 |
| 设置项的**默认值**或语义变化 | **必须** | `internal/store/settings.go` 的种子值 + 消费逻辑（`keepDataPeriodDays` 180→365 就是这么来的） |
| 通知/检查的**文案**（`src/lang/en.json`） | **必须** | Go 里的消息字符串保持一致 |
| 纯前端组件、样式、i18n 新增语言 | 不需要 | 合并 `master` 即可 |
| 依赖升级（`package.json` / `package-lock.json`） | 一般不需要 | 除非影响前端构建或引入新的运行时约定 |
| CI / release 脚本 | 不需要 | 本仓库用自己的 workflow |

### 3.3 四个高频陷阱

1. **`active || true` 类"官方 bug"要不要照抄？** 照抄。目标是行为等价，不是修上游 bug。修正会让前端在两种后端下表现不一致。历史上 proxy 的启用开关就是这种恒真表达式，Go 侧保留并写注释说明。
2. **官方注释说"公开"的端点别自己加鉴权**。典型是 status page：官方就是公开访问的，加了登录门槛反而破坏嵌入场景。
3. **不要给字段做"顺手优化"**（trim、类型转换、默认值补全）。例如用户名密码校验官方不 trim，Go 也不能 trim，否则会产生"官方能登、Go 不能登"的疑难杂症。
4. **"SQLite 能跑"不等于"MySQL 能跑"**。SQLite 是动态类型 + 宽容比较，MySQL 严格模式下会直接报错。2026-11-21 真库联调抓到的五类：
   - `TIMESTAMP NOT NULL` 有第二列时 MariaDB 隐式补零值默认 → **1067**（所以时间列统一 `DATETIME(3)`）；
   - `WHERE datetime_col != ''` 让 MySQL 把 `''` 转日期 → **1292**（改用 `LENGTH(col) > 0`）；
   - better-auth 的字符串用户 ID 写进 `INT UNSIGNED` → **1265**（统一走 `handlers.numericUserID`，非数字记 0）；
   - `VACUUM` 是 SQLite 专属（`shrinkDatabase` 要按 `DB.IsMySQL()` 分支，官方也是空操作）；
   - 派生表 `FROM (SELECT ...)` 必须有别名 → **1248**（`TestNoSQLiteOnlySQLInSources` 会扫）。
   **新增 SQL 后先跑 `go test ./internal/store/ -run TestNoSQLiteOnlySQLInSources`，涉及写路径再跑一次真库冒烟。**

---

## 4. 改 Go 代码的规范

### 4.1 目录职责（改错地方会带来后续排查困难）

| 目录 | 职责 | 不该做的事 |
| --- | --- | --- |
| `internal/store/` | SQL 与数据结构，含 schema.sql | 不写业务判断（除了官方本来就写在 model 里的） |
| `internal/checkers/` | 探测实现，输入 monitor → 输出 Result | 不直接写库、不发通知 |
| `internal/monitor/` | 调度、心跳落库、钩子（TLS/域名到期/通知） | 不实现具体探测 |
| `internal/notify/` | 通知渠道 + 引擎 | 不判断"该不该发"（那是 monitor 层的事） |
| `internal/handlers/` | socket 事件（客户端→服务端） | 不写 SQL，调 store |
| `internal/httpapi/` | REST 路由 | 同上 |
| `internal/socketio/`、`internal/hub/` | 协议层与连接管理 | 不掺业务 |

### 4.2 按改动类型执行的 checklist

**加一个监控类型**

1. `internal/checkers/<type>.go` 实现 `Check(ctx, mon) Result`，注册进 `init()`
2. 消息文案从 `src/lang/en.json` 对应键抄
3. `internal/checkers/types_test.go` 的护栏清单加这个类型（前端下拉框取值是权威来源）
4. 若走 HTTP 家族（http/keyword/json-query），确认代理注入 `attachProxy` 覆盖到

**加一个通知渠道**

1. `internal/notify/providers_*.go` 实现发送，在 `init()` 里注册进渠道表
2. `internal/notify/registry_test.go` 加前端 `NotificationFormList` 的键护栏
3. 注意官方有些渠道用 GET、有些用 POST+JSON、有些要 form —— 照官方 provider 逐字段抄

**加一个 socket 事件**

1. `internal/handlers/<x>.go` 写 handler，`registerEvents` 注册
2. 回包结构照官方（`{ok:true, msg:"Saved."}` / `{ok:false, msg:i18nKey}`）
3. **必须在 `socket.io-client@4` 下实测**——curl 验不了 socket
4. 同步更新 `API-PARITY.md` §3 的计数

**加一个 REST 路由**

1. `internal/httpapi/` 里实现，`router.go` 挂载
2. 鉴权语义照官方对应中间件（`requireApiKey` / `requireApiAccess` / 公开）
3. 更新 `API-PARITY.md` §1 表格与结论速览的计数

**改数据库 schema**

1. **两份都要改**：`internal/store/schema.sql`（SQLite）和 `internal/store/schema_mysql.sql`（MySQL）。
   `TestSchemaParity` 逐表逐列比对，只改一份会直接失败
2. MySQL 侧的类型翻译规则：`INTEGER PRIMARY KEY AUTOINCREMENT`→`INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY`、
   `BOOLEAN`→`TINYINT(1)`、`REAL`→`DOUBLE`、`DATETIME`→`DATETIME(3)`（带毫秒）、
   `TEXT ... DEFAULT 'x'`→`DEFAULT ('x')`、索引内联 `KEY`/`UNIQUE KEY`、所有标识符加反引号；
   **TEXT 不能做主键或唯一索引**，需改 `VARCHAR(255)`
3. 老库兼容：`ensureColumns`（补列）/`ensureTables`（补表）会自动处理，但要确认真实老库能起来
4. 验证方式：拿一个**旧版 Go 或官方版**的 `kuma.db` 启动新二进制，看是否报错
5. 写 SQL 的方言限制（`PRAGMA`/`ON CONFLICT`/`excluded.`/`CAST AS INTEGER`/派生表别名）
   由 `TestNoSQLiteOnlySQLInSources` 扫描源码守护，改动后再跑一次 `go test ./internal/store/`

---

## 5. 验证矩阵：改完必须做到哪一步

### 5.1 分层验证（按改动范围选）

| 改动范围 | 最低验证 | 命令 |
| --- | --- | --- |
| 纯文档 | 无 | — |
| 单个 checker / provider | 单测 + 编译 | `go build ./... && go test ./internal/checkers/ -count=1` |
| store / schema | 单测 + 老库启动 | `go test ./internal/store/ -count=1` + 老 `kuma.db` 冒烟 |
| store 方言（涉及 SQL / schema） | 上述 + 两份 schema 对齐 + **真库跑一次** | `go test ./internal/store/ -run 'Schema\|SQL\|DSN' -v`（`TestSchemaParity`、`TestNoSQLiteOnlySQLInSources`）；再按 [`go_server/e2e/README.md`](go_server/e2e/README.md) 跑 `TestMariaDBIntegration` |
| socket 事件 | **必须真连 socket** | 起服务 + `node` 脚本用 `socket.io-client@4` 走完整流程 |
| REST 路由 | curl/HTTP 断言 | 参照 `API-PARITY.md` 附录命令 |
| 协议层（socketio / MCP / Playwright） | **必须对接真实对端** | socket 用官方客户端库；MCP 用 `@modelcontextprotocol/client`；远程浏览器用真 `launchServer` |

### 5.2 提交前全量门禁

```bash
# 本机 go 不在 PATH 里时（macOS 安装位置）
export PATH=$PATH:$HOME/.workbuddy/binaries/go/bin
cd go_server

gofmt -l internal/ cmd/          # 必须无输出
go build ./... && go build -tags realbrowser ./...   # 两套构建都要过
go vet ./...
go test ./... -count=1
```

> **realbrowser tag**：`internal/checkers/realbrowser.go` 走构建标签，默认构建是 stub。改了 real-browser 相关代码必须用 `-tags realbrowser` 验一遍。

### 5.3 CI 目前覆盖到哪（和缺口）

`.github/workflows/build-go-server.yml` 分两条路径（2026-11-21 调整）：

**push `main`（`go_server/**` 或前端文件变化）→ 只校验，不构建镜像**
`go vet ./...` + `go test ./... -count=1`，两分钟内出结果。

**打 `v*` tag（或手动 dispatch）→ 才发版构建**
1. 构建前端（Vue → dist，打进镜像）
2. `go vet` + `go test`
3. 交叉编译 linux/amd64 + arm64（版本号注入 `${package.json版本}-go.${短sha}`）
4. 组装多架构镜像推 GHCR：`x.y.z` / `x.y` / `x` / `latest` / `sha-<sha>`
5. **冒烟测试**：真起容器，轮询 `/api/health` 等 `{"ok":true}`

这样日常提交不烧构建额度，`latest` 只在正式发版时前进；需要"某次提交的镜像"时用 `sha-<短sha>` 或手动 dispatch。

### 5.4 当前维护债（该修还没修的）

| 项 | 影响 | 建议做法 |
| --- | --- | --- |
| ~~CI 不跑单测~~ | 已在 2026-11-21 补上（`main` push 与 tag 都跑 `go test ./...`） | 已完成 |
| ~~E2E 脚本散在 `/tmp`~~ | MySQL 相关脚本已收进 `go_server/e2e/`；其余协议验证脚本（proxy / MCP / Playwright）仍在 `/tmp`，重写前先看 git 历史里的提交说明 | 需要时按同样的格式收进 `go_server/e2e/` |
| `/metrics` 只有 3 个自订 gauge（B12） | 官方 Grafana 面板接不上 | 见 `API-PARITY.md` B12 |
| 徽章忽略 `style` 参数（C 级） | 用户传 `?style=plastic` 无效 | 低优先，需要时补字宽表 |

---

## 6. 发布与版本

### 6.1 版本号从哪来

| 位置 | 值 | 谁维护 |
| --- | --- | --- |
| 前端 `package.json` 的 `version` | 官方版本（如 `3.0.0-beta.1`） | 官方，合并自动更新 |
| `go_server/internal/version/version.go` 的 `Version` | 本地开发默认值（如 `3.0.0-go.1`） | 手工 |
| 运行时实际版本 | `${package.json版本}-go.${短sha}` | **CI 用 `-ldflags -X` 注入，覆盖 Go 默认值** |

也就是说：本地跑 `go run ./cmd/go_server` 看到的是 `version.go` 里的默认串，镜像里看到的是带 sha 的真实版本。**判断线上跑的是哪个构建，看版本号尾部的 sha。**

### 6.2 打 tag 发正式镜像

```bash
git tag v3.0.0-go.2          # 语义化：官方版本 + -go.N
git push origin v3.0.0-go.2  # tags: ["v*"] 会触发 CI
```

产出镜像 tag：`v3.0.0-go.2` 与 `3.0.0-go.2`（都从同一个 tag 派生）以及 `sha-<短哈希>`。
`latest` 只在默认分支（`main`）上打，tag 构建不会覆盖 `latest`——这是刻意设计，避免测试版顶掉稳定标签。

### 6.3 回滚

镜像用不可变 tag 部署（`sha-xxxxxxxx` 或 semver），回滚就是改回上一个 tag 重启：

```bash
docker run -d --restart=unless-stopped -p 3001:3001 -v uptime-kuma:/app/data \
  ghcr.io/dislazy/uptime-kuma-go:sha-<上一个好用的短哈希>
```

数据层不需要回滚（schema 只增不减），但**如果新版加了列而回滚到老版**，老版会忽略新列，行为安全。

---

## 7. 文档维护矩阵（改什么 → 更新哪份）

这是最容易烂掉的部分。把"改完记得更新文档"变成硬规则：

| 你改了什么 | 必须更新 |
| --- | --- |
| 同步了上游 | `go_server/SYNC.md` 的**两个基线 commit** + 历史记录表追加一行 |
| 新增/补齐 REST 路由、socket 事件、推送 | `go_server/API-PARITY.md` 对应表 + **结论速览里的计数**（39/40 这种数字必须准） |
| 关闭或新增一个缺口 | `API-PARITY.md` §5 分级表（A/B/C/D/E），并在 `GO-SERVER.md` 的对齐段落同步计数 |
| 新增功能模块（用户可见） | `go_server/README.md` 的"已实现"；`GO-SERVER.md` 的"覆盖了什么" |
| 改启动参数/环境变量 | `go_server/README.md` 配置表 |
| 改目录结构或新增包 | `go_server/AGENTS.md` 目录地图 |
| 新增踩坑/硬性约定 | `go_server/AGENTS.md` 的踩坑段落（**踩到坑就记，别攒**） |
| 新增文档 | `GO-SERVER.md` 文末"文档索引"表 |

**计数一致性**：`API-PARITY.md`、`GO-SERVER.md` 里都有"40 条 REST / 76 个 socket"这类数字，改一处必须同时改另一处，否则两份文档互相打脸。改完用 `grep` 扫一遍：

```bash
grep -n "直接对齐 40 条\|REST 路由（生产挂载）\|Socket 事件（客户端" GO-SERVER.md go_server/API-PARITY.md
```

---

## 8. 事故处理手册

| 现象 | 先查什么 | 常见根因 |
| --- | --- | --- |
| 容器起不来 `permission denied` | 二进制可执行位 | CI artifact 传输（zip）不保留 `+x`，`Dockerfile.ci` 和 prepare 步骤都要 `chmod +x` |
| 容器起来但 `/api/health` 不通 | `docker logs`，端口/数据目录权限 | 数据目录不可写、端口被占、DB 文件损坏 |
| 前端白屏 / 一直转圈 | 浏览器控制台 + socket 帧 | socket 握手失败（协议层）、`info` 事件没推、静态资源 404 |
| 登录成功但立刻掉线 | 会话 cookie 域/路径、`ServerTimezone` | better-auth 兼容层返回结构不对、cookie 属性不匹配 |
| 接管官方 `kuma.db` 后某个功能坏 | 该表列名（camelCase vs snake_case） | schema 版本差异，`ensureColumns` 没覆盖到 |
| 监控全部不动 | `manager` 调度、`interval` 解析、状态机 | 时间单位（秒/毫秒）搞错、retry 死循环 |
| 带代理的监控直连 | `attachProxy` 是否命中该类型 | 只对 HTTP 家族生效，其他类型官方也不走代理 |
| 徽章/文档 404 | 路由挂载 + 方法匹配 | Go `ServeMux` 的 pattern 写法（`GET /path` vs `/path`） |
| 编译秒失败 `go.mod requires go >= 1.26.0` | CI go 版本 | 用 `go-version-file` 会踩；**显式 pin `go-version: "1.26.0"`** |
| 某文件"改了但没进 git" | `git check-ignore -v <path>` | `.gitignore` 里不带 `/` 的规则会匹配任意层级（`go_server` 曾把 `cmd/go_server` 整目录忽略） |

**排查原则**：先确认**官方行为是什么**，再判断 Go 是否偏离。官方的权威来源是 `server/`（Node 实现），不是文档、不是记忆。有疑问就 `git diff` 或直接读官方源码。

---

## 9. 已知缺口与既定决策

完整清单在 [`go_server/API-PARITY.md`](go_server/API-PARITY.md) §5，这里只记**决策**（避免以后反复纠结）：

| 项 | 决策 | 理由 |
| --- | --- | --- |
| Cloudflared 隧道（5 个 socket 事件） | **明确不做** | 部署机有公网直连，无使用场景；官方还要管二进制进程，性价比最低 |
| `/screenshots/*` 静态外链 | 不实现 | real-browser 不落截图产物，等价偏差 |
| `importantHeartbeatList` 推送 | 暂不实现 | 前端不消费 |
| `/metrics` 完整指标集（B12） | **暂不做**（2026-11-21 决定） | 需要 7 个标准指标 + 动态 tag label；当前只有 3 个 `gokuma_*` gauge，官方 Grafana 面板接不上 |
| `embedded-mariadb` 存储 | **不做** | 要把 `mariadbd` 打进镜像（约 +430MB）并实现 datadir 初始化、socket 启动；外部 MariaDB 已覆盖生产用法 |
| MySQL/MariaDB 真库联调 | **已完成**（2026-11-21，外部 MariaDB） | `TestMariaDBIntegration` + `e2e/mysql_smoke.js` 跑通建库/接管/读写/聚合/CRUD；抓出并修掉 5 个 SQLite 才宽容的问题（见 §3.3 第 4 条）。开发机仍不装数据库，复测依赖外部库 + 环境变量 |
| C 级"等价实现"5 项 | 不动 | 行为一致，机制不同，改了没有收益 |

**新增缺口的记录方式**：先在 `API-PARITY.md` §5 分好级（A 安全 / B 功能 / C 等价 / D 占位 / E 不适用），再决定做不做。**分级要诚实**——把功能缺失写成"等价实现"会让以后的自己误判，`/metrics` 就是这么被纠回来的。

---

## 10. 给 AI Agent 的接手方式

如果让 AI 接手维护，开场把这几件事说清楚，能省掉大量来回：

1. **先读**：`go_server/AGENTS.md`（约定与踩坑）→ `API-PARITY.md`（现状）→ 本文（流程）
2. **权威来源是官方源码**，不是文档、不是记忆。任何"官方是不是这样"的判断都要落到 `server/` 里的具体文件行号
3. **行为等价优先于代码优雅**。官方有 bug（如恒真表达式）也照抄，并写注释说明为什么
4. **改完必须跑** §5.2 的全量门禁；socket/协议类改动必须对接真实对端验证
5. **不要同时改文档和代码又不跑测试**。文档里的计数和代码必须对得上
6. **提交信息**：`feat:` / `fix:` / `docs(parity):` + 一句人话结论；涉及审计项的写明 `(audit B12)` 这类编号，方便回溯

环境备忘（本机）：

| 项 | 值 |
| --- | --- |
| Go | `$HOME/.workbuddy/binaries/go/bin`（go1.26.0），需手动加 PATH |
| Node | `$HOME/.workbuddy/binaries/node/versions/22.22.2-6/bin` |
| 出网代理 | `127.0.0.1:7897`（GitHub API / RDAP / GHCR 都要走） |
| E2E 脚手架 | `/tmp/sio/*.js`（socket.io-client 脚本），起服务端口固定 `3999` 避免撞官方 3001 |
| 远程服务器 | `ce-234`（验证镜像用，已装 go1.26.0） |

# AGENTS.md — go_server 接手指南

> 给后续接手的 AI agent / 新协作者看的项目全景。改代码前先读完这份，
> 再按需读 `README.md`（功能清单）和 `SYNC.md`（官方同步流程）。

## 这个项目是什么

**Uptime Kuma 的 Go 后端复刻**。动机：Node 后端对低配服务器不友好。

- 官方上游：`louislam/uptime-kuma`（本仓库 fork 自 `dislazy/uptime-kuma`）
- 分支约定：`master` = 官方原版（不要动），`main` = 官方 + `go_server/`（工作分支）
- **前端（根目录 `src/`）零改动**，Vue 源码原样复用，运行期由 Go 进程托管
- **数据库完全兼容官方 SQLite schema**：老用户把 `kuma.db` 拷过来就能跑；
  启动时 `ensureColumns` 按本仓库 `schema.sql` 给旧表自动补列，无需手工迁移
- 通信协议：自实现 **Socket.IO v4 / Engine.IO v4**（polling + websocket 直连 + 升级），
  事件名、参数顺序、消息文案与官方逐一对应

## 目录地图

```
go_server/
├── Dockerfile              # 多阶段：node 编前端 → go 编后端 → alpine 运行镜像
├── SYNC.md                 # 同步官方改动的基线 commit + 映射表（同步前必读）
├── README.md               # 功能清单（已实现/已知差异）
├── cmd/go_server/main.go   # 入口：装配 store/checkers/monitor/notify/handlers
└── internal/
    ├── socketio/           # Engine.IO + Socket.IO 协议层（帧编解码、会话、ack）
    ├── auth/               # better-auth 兼容：sign-in/username、scrypt、cookie 签名、2FA
    ├── httpapi/            # REST：/api/auth/*、/api/setup、状态页公开接口、RSS、badge
    ├── store/              # SQLite（modernc.org/sqlite 纯 Go）：schema、CRUD、列名自适应
    ├── model/              # Monitor 等结构体；Raw 保留原始行，按需取列
    ├── checkers/           # 34 种监控类型，一类型一文件，init() 里 Register
    ├── monitor/            # 调度器（时间轮 + worker 池）、group 聚合
    ├── notify/             # 111 个通知渠道，表驱动 registerFunc
    ├── conditions/         # 监控条件求值（14 操作符 + and/or 分组）
    ├── cron/               # 自研 5 字段 cron（维护窗口用）
    ├── handlers/           # 100+ socket 事件，一文件一域
    └── version/            # -ldflags 注入的版本号
```

## 当前状态（2026-11-20）

- 监控 **34/34**（前端 EditMonitor.vue 下拉框全量，`types_test.go` 护栏断言）
- 通知 **111/111**（与前端 NotificationFormList 逐键对应，`registry_test.go` 护栏）
- 2FA、Web Push、状态页/RSS/badge、维护窗口 6 策略、conditions、统计聚合、Docker Host 管理：完成
- 已知差异（详见 README「已知差异与未实现」）：`pm2` 明确不支持；
  JSONata 用纯 Go 实现，个别边缘语法可能与官方 node:vm 有差异；
  real-browser 需要 `-tags realbrowser` 且机器有 Chromium；
  证书/域名到期监控未实现
- 全功能静态二进制 **51MB**（CGO 关闭、`-trimpath -ldflags "-s -w"`）

## 常用命令

```bash
cd go_server
export PATH=$HOME/.workbuddy/binaries/go/bin:$PATH   # 本机 go 工具链位置
export GOPROXY=https://goproxy.cn,direct             # 国内代理

go build ./... && go vet ./... && go test ./... -count=1   # 全量校验

# 起服务冒烟（端口/数据目录随便换，别用 3001 以免撞官方实例）
CGO_ENABLED=0 go build -o /tmp/go_server ./cmd/go_server
/tmp/go_server -port 3999 -data-dir /tmp/kuma-test
curl localhost:3999/api/health    # {"ok":true,"version":"..."}

# Docker 镜像（GitHub Actions 会自动构建，本地无 docker 时跳过）
# docker build -f go_server/Dockerfile -t kuma-go:dev .
```

**socket 层验证必须用真实客户端**（curl 只能验 REST）：
`socket.io-client@4` 已装在 `/tmp/sio`，node 在
`~/.workbuddy/binaries/node/versions/22.22.2-6/bin`，参考脚本 `/tmp/sio/probe*.js`。

## 硬性约定（违反会出静默 bug）

1. **字段名照抄官方 JS 写法**（camelCase、事件名、消息文案），不要顺手改 Go 风格。
   前端读的是 JSON key，写错就是"字段填了但保存后回显为空"这类难查问题。
2. **新增监控类型**：`internal/checkers/` 写文件 + `init()` Register，
   **必须**同时在 `types_test.go` 的 `frontendMonitorTypes` 补一行；
3. **新增通知渠道**：`internal/notify/` 实现 Provider/ContextProvider，
   `registry_test.go` 的 111 键清单要同步补；
4. **monitor.Raw 的内部键用下划线前缀**（如 `_docker_daemon`），
   `monitorJSON()` 会过滤掉不下发前端；用户字段 camelCase。
5. **JSON 字符串列下发前要 parse 成对象**：`monitorJSON()` 里那份清单
   （`conditions`、`config`、`kafkaProducerBrokers`…），新 JSON 列记得加进去。
6. **列名自适应**：官方 v3 库同表 camelCase/snake_case 并存，读列走
   `resolveColumn`，不要硬编码列名直查。
7. 官方行为哪怕看着"不合理"也**先核对源码再改**（例：`settings.type` 不匹配就
   跳过是官方原样行为，不是 bug）。

## 历史踩坑（改协议层前必读）

- Socket.IO CONNECT 应答必须是 `40{...}`（Engine.IO `4` + Socket.IO `0`），
  少写 `4` 客户端会把它当 open 包重新握手然后崩；
- 直连 websocket 的握手 `upgrades` 必须是 `[]`，polling 起步才是 `["websocket"]`；
- `setSettings` 官方签名是 `(data, currentPassword)`，不是 `(type, data)`；
- RADIUS 的 User-Password 混淆与响应校验、franz-go 的
  `kgo.AllowAutoTopicCreation()` 无参、kafkajs `mechanism:"None"` 映射为不发 SASL，
  细节见各 checker 文件头注释与 `SYNC.md`；
- `go_server/.gitignore` 的编译产物条目**必须用 `/go_server` 锚定**：不带 `/` 的
  模式会匹配任意层级同名路径，曾把 `cmd/go_server/` 源码目录整个忽略，
  导致 CI 上 checkout 后 `go build ./cmd/go_server` 秒失败（本地能编译因为
  文件还在工作区，极具迷惑性）。新增源码目录后先 `git ls-files` 确认真的入库了。

## 同步官方新改动

**先读 `SYNC.md`**：用基线 commit 圈出 `git log <baseline>..origin/master` 的范围，
按映射表落到 go_server 对应文件，跑全量测试，socket 改动实连验证，
最后更新 `SYNC.md` 的基线与历史记录表。

## 提交与身份

- 本仓库是个人 fork，`main` 分支直接提交即可，push 走 SSH
  （`~/.ssh/config` 已配 `github.com → ~/.ssh/songbo2099_git`）；
- 提交身份当前为 `dislazy <dislazy@users.noreply.github.com>`；
- 若要给官方上游提 PR：根目录 `AGENTS.md` 是官方的 AI-slop 政策，
  禁止未审阅的生成代码直接送 PR——go_server 的改动**只在本 fork**，不受此约束。

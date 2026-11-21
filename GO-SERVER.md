# GO-SERVER.md — 本 Fork 的整体说明

> 本文件独立于官方 `README.md`（后者保持官方原文不动）。想了解"这个仓库是什么、改了什么、
> 能不能用、怎么部署、怎么跟官方同步"，看这一篇就够了。

## 这是什么

本仓库 fork 自 [louislam/uptime-kuma](https://github.com/louislam/uptime-kuma)（Uptime Kuma 官方，
Node.js 后端 + Vue 前端的自托管监控工具）。在此之上，**用 Go 从零复刻了全部后端**，
放在 [`go_server/`](go_server/) 目录，目标：

- **低配置服务器友好**：不需要 Node 运行时，产物是一个静态二进制（约 68MB，
  `-ldflags "-s -w"` 后约 51MB），内存占用远低于官方 Node 版；
- **前端零改动**：官方 Vue 源码原样构建，Go 进程直接托管 dist；
- **数据库零迁移**：完全兼容官方 SQLite schema（含 camelCase/snake_case 列名自适应），
  官方版用户把 `kuma.db` 拷过来即可无缝接管，反过来也成立。

## 分支布局

| 分支 | 内容 | 用法 |
| --- | --- | --- |
| `master` | **与官方上游严格一致**（快进同步，不做任何改动） | 同步基线，勿改 |
| `main` | 官方代码 + `go_server/` Go 后端 + CI | 工作分支，日常开发在这 |

同步机制：`main` 定期把 `master` 合并进来，评估官方新提交对后端的影响，
有后端语义变化就同步到 `go_server`（流程与基线记录见
[`go_server/SYNC.md`](go_server/SYNC.md)）。

## Go 后端覆盖了什么

- **34/34 监控类型**：HTTP(s)、TCP/UDP、ping、DNS、Docker、各类数据库、MQTT、
  gRPC、SNMP、RADIUS、Kafka、游戏服务器查询（GameDig）等，与官方 monitor-types 一一对应
- **111/111 通知渠道**：Telegram、Discord、企业微信、钉钉、飞书、Bark、Gotify……
- **认证体系**：better-auth 兼容（用户名登录、会话、改密、2FA TOTP+备用码、
  API Key 全生命周期、REST API 鉴权）
- **实时协议**：Socket.IO v4 协议层自实现，前端不改一行即可连上
- **状态页**：多状态页、公告（incident）、RSS、PWA manifest、徽章
- **运维面**：维护窗口、Docker 主机管理、统计聚合、数据保留清理（跟随 UI 设置）、
  Prometheus `/metrics`（最小集）

**API 对齐的精确程度**：全部 REST 路由与 socket 事件已逐条审计，
结果在 [`go_server/API-PARITY.md`](go_server/API-PARITY.md) ——
直接对齐 40 条 REST 中的 38 条、76 个 socket 事件中的 71 个、
23 个推送中的 19 个；域名到期（RDAP）、TLS 证书、Proxy、远程浏览器、
状态页徽章、API 文档、robots.txt/时区初始化等 B 级缺口已按官方逻辑补齐，
剩余缺口只有 `/mcp`（AI 工具接入）与截图外链两条
（Cloudflared 隧道管理明确不做——部署机直连公网时无使用场景），
审计文档按安全/功能/等价/占位四级分好类，可直接当 TODO 清单用。

## 部署

```bash
# 方式一：官方镜像（多架构，GitHub Actions 自动构建）
docker run -d --restart=unless-stopped \
  -p 3001:3001 -v uptime-kuma:/app/data \
  ghcr.io/dislazy/uptime-kuma-go:latest

# 方式二：裸机二进制（无任何运行时依赖）
# 从 CI artifact 取 go_server-linux-amd64，放到任意 Linux 上直接跑
./go_server -port 3001 -data-dir /path/to/data -web-dir /path/to/dist
```

- 镜像：`ghcr.io/dislazy/uptime-kuma-go`（tag：`latest` / `main` / `sha-<短哈希>`）
- 数据目录结构与官方一致（`kuma.db` + `upload/`），官方版 ↔ Go 版可互换
- 反代/TLS 在宿主机边缘做（Caddy/Nginx → 容器 3001），镜像内单进程单端口
- 常用配置项见 [`go_server/README.md`](go_server/README.md) 环境变量表

## 相对官方动了哪里

- **新增**：`go_server/`（全部 Go 后端）、`.github/workflows/build-go-server.yml`（CI）、
  本文件与根 `AGENTS.md` 末尾的接手段落
- **未动**：官方的 `server/`（Node 后端）、`src/`（前端）、`db/`（迁移脚本）、
  `README.md`、构建配置——本仓库理论上仍可当官方 fork 跑 Node 版

## 文档索引

| 文档 | 内容 |
| --- | --- |
| [`go_server/README.md`](go_server/README.md) | Go 后端功能清单、启动参数、已知差异 |
| [`go_server/API-PARITY.md`](go_server/API-PARITY.md) | **API 对齐审计**：REST + socket 逐条比对 |
| [`go_server/AGENTS.md`](go_server/AGENTS.md) | Agent/接手指南：目录地图、约定、踩坑实录 |
| [`go_server/SYNC.md`](go_server/SYNC.md) | 与官方上游的同步基线、文件映射表、操作流程 |
| [`go_server/ROADMAP.md`](go_server/ROADMAP.md) | 改造路线图与完成度记录 |

## 免责声明

本 Go 后端为个人复刻项目，未经官方测试或认可。遇到问题请在本仓库提 issue，
**不要把 go_server 相关问题报给官方上游**。

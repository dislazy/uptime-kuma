# SYNC.md — 同步基线台账

> **本文件只当记录用**：存当前基线 commit、历史同步台账。
> 完整的同步流程、改动分类规则、验证清单在根目录 [`MAINTAINING.md`](../MAINTAINING.md)（§3 上游同步 SOP）。
>
> 每次同步官方新改动后，把下面两个基线 commit 更新到最新值，并在台账追加一行。

## 当前基线

| 项 | commit | 说明 |
| --- | --- | --- |
| 官方上游基线 | `b0cd0f1d` | `upstream/master`（= 本地 `master`），2026-11-21，*fix(settings): use the server's 365-day default for keepDataPeriodDays (#8213)* |
| go_server 基线 | `ab5bd237` | `main` 分支，go_server 全量首发（110 文件 / 29,389 行） |

前端（`src/`、`public/`、`package.json`）零改动，跟随官方 `master`；
需要同步到 Go 的只有 `server/`、`db/`、`src/` 里的**协议与数据结构约定**。

## 圈定范围的命令

```bash
git fetch upstream
git log b0cd0f1d..upstream/master --oneline         # 官方新提交一览
git diff --stat b0cd0f1d..upstream/master -- \
    server/ db/ src/mixins/socket.js src/components/notifications/ \
    src/pages/EditMonitor.vue src/lang/en.json       # 只看后端相关
```

## 历史同步记录

| 日期 | 官方基线 | go_server 提交 | 备注 |
| --- | --- | --- | --- |
| 2026-11-19 | `160acac6` | `ab5bd237` | 首次基线 |
| 2026-11-21 | `b0cd0f1d` | 见 `main` | 3 个上游提交：加泰罗尼亚语言、依赖更新（纯前端，合并即得）；keepDataPeriodDays 修复 → Go 侧同步：settings 种子 180→365、prune 改为跟随 UI 设置（缺失/解析失败回落 365，<1 禁用清理，对齐官方 clear-old-data.js），并补上此前缺失的 heartbeat 过期清理（Go 原本只清聚合表，heartbeat 无限增长） |
| 2026-11-21 | `b0cd0f1d` | `7ceba0a9` + `02bc40ea` | 无上游变更；本轮是**补齐对齐缺口**而非同步：B1 TLS 证书、B2 Proxy、B3 远程浏览器、B5 API 文档、B6 状态页徽章、B7 域名到期 RDAP、B8/B9 小路由与推送、B10 MCP 端点，并把 `/metrics` 从"等价实现"纠正为缺口 B12（详见 `API-PARITY.md` §5） |

> 台账里的 `go_server 提交` 写**该轮收尾时的 main HEAD**，方便以后回溯"这套代码对应哪版行为"。

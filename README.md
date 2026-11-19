<div align="center" width="100%">
    <img src="./public/icon.svg" width="128" alt="Uptime Kuma Logo" />
</div>

# Uptime Kuma

Uptime Kuma is an easy-to-use self-hosted monitoring tool.

<a target="_blank" href="https://github.com/louislam/uptime-kuma"><img src="https://img.shields.io/github/stars/louislam/uptime-kuma?style=flat" /></a> <a target="_blank" href="https://hub.docker.com/r/louislam/uptime-kuma"><img src="https://img.shields.io/docker/pulls/louislam/uptime-kuma" /></a> <a target="_blank" href="https://hub.docker.com/r/louislam/uptime-kuma"><img src="https://img.shields.io/docker/v/louislam/uptime-kuma/2?label=docker%20image%20ver." /></a> <a target="_blank" href="https://github.com/louislam/uptime-kuma"><img src="https://img.shields.io/github/last-commit/louislam/uptime-kuma" /></a> <a target="_blank" href="https://opencollective.com/uptime-kuma"><img src="https://opencollective.com/uptime-kuma/total/badge.svg?label=Open%20Collective%20Backers&color=brightgreen" /></a>
[![GitHub Sponsors](https://img.shields.io/github/sponsors/louislam?label=GitHub%20Sponsors)](https://github.com/sponsors/louislam)

<img src="https://user-images.githubusercontent.com/1336778/212262296-e6205815-ad62-488c-83ec-a5b0d0689f7c.jpg" width="700" alt="Uptime Kuma Dashboard Screenshot" />

## 🐹 About This Fork / 关于本项目

本仓库 fork 自 [louislam/uptime-kuma](https://github.com/louislam/uptime-kuma)，在此之上**用 Go 重写了全部后端**（目录 [`go_server/`](go_server/)），目标是让低配置服务器也能跑 Uptime Kuma——不再需要 Node 运行时，最终产物是一个约 51MB 的静态二进制。

| 分支 | 内容 |
| --- | --- |
| `master` | 与官方上游完全一致（同步基线，不要改动） |
| `main` | 官方代码 + `go_server/` Go 后端（工作分支） |

**相对官方做了什么：**

- **新增 `go_server/`**：Golang 复刻的后端，功能对标官方 Node 版——
  34 种监控类型、111 个通知渠道、Socket.IO v4 协议层、2FA、状态页（含 RSS/徽章）、
  维护窗口、监控条件、统计聚合、Docker 主机管理等，与官方逐一对应
- **前端零改动**：Vue 源码原样复用，构建产物由 Go 进程托管
- **数据库零迁移**：完全兼容官方 SQLite schema，老用户直接拷 `kuma.db` 即可运行；
  列名自适应兼容官方 v3 库 camelCase/snake_case 混用的情况
- **新增 Dockerfile 与 CI**：`go_server/Dockerfile` 多阶段构建（前端 + Go + 运行镜像），
  GitHub Actions 自动构建多架构镜像推送到 `ghcr.io/<owner>/uptime-kuma-go`

**没有改动什么：** 官方的 Node 后端（根目录 `server/`）、数据库迁移脚本、
前端源码、构建配置全部保留原样——理论上本仓库仍可作为官方 fork 正常运行 Node 版。

详细文档：

- [`go_server/README.md`](go_server/README.md) — Go 后端功能清单与已知差异
- [`go_server/AGENTS.md`](go_server/AGENTS.md) — 接手指南（目录地图、约定、踩坑记录）
- [`go_server/SYNC.md`](go_server/SYNC.md) — 同步官方上游改动的基线与映射表

> ⚠️ 注意：本 fork 的 Go 后端为个人项目，未经官方测试；遇到问题请先在本 fork 提 issue，
> 不要把 `go_server` 相关问题报给官方上游。

## 🥔 Live Demo

Try it!

Demo Server (Location: Frankfurt - Germany): <https://demo.kuma.pet/start-demo>

It is a temporary live demo, all data will be deleted after 10 minutes. Sponsored by [Uptime Kuma Sponsors](https://github.com/louislam/uptime-kuma#%EF%B8%8F-sponsors).

> [!WARNING]
> Be careful - the top result in Google, uptimekuma.co, is not our official website, we don't control the website. I recommend following the installation guide in this GitHub repo.

## ⭐ Features

- Monitoring uptime for HTTP(s) / TCP / HTTP(s) Keyword / HTTP(s) Json Query / Websocket / Ping / DNS Record / Push / Steam Game Server / Docker Containers
- Fancy, Reactive, Fast UI/UX
- Notifications via Telegram, Discord, Gotify, Slack, Pushover, Email (SMTP), and [90+ notification services, click here for the full list](https://github.com/louislam/uptime-kuma/tree/master/src/components/notifications)
- 20-second intervals
- [Multi Languages](https://github.com/louislam/uptime-kuma/tree/master/src/lang)
- Multiple status pages
- Map status pages to specific domains
- Ping chart
- Certificate info
- Proxy support
- 2FA support

## 🔧 How to Install

### 🐳 Docker Compose

```bash
mkdir uptime-kuma
cd uptime-kuma
curl -o compose.yaml https://raw.githubusercontent.com/louislam/uptime-kuma/master/compose.yaml
docker compose up -d
```

Uptime Kuma is now running on all network interfaces (e.g. http://localhost:3001 or http://your-ip:3001).

> [!WARNING]
> File Systems like **NFS** (Network File System) are **NOT** supported. Please map to a local directory or volume.

### 🐳 Docker Command

```bash
docker run -d --restart=always -p 3001:3001 -v uptime-kuma:/app/data --name uptime-kuma louislam/uptime-kuma:2
```

Uptime Kuma is now running on all network interfaces (e.g. http://localhost:3001 or http://your-ip:3001).

If you want to limit exposure to localhost only:

```bash
docker run ... -p 127.0.0.1:3001:3001 ...
```

### 💪🏻 Non-Docker

Requirements:

- Platform
  - ✅ Major Linux distros such as Debian, Ubuntu, Fedora and ArchLinux etc.
  - ✅ Windows 10 (x64), Windows Server 2012 R2 (x64) or higher
  - ❌ FreeBSD / OpenBSD / NetBSD
  - ❌ Replit / Heroku
- [Node.js](https://nodejs.org/en/download/) >= 20.4
- [Git](https://git-scm.com/downloads)
- [pm2](https://pm2.keymetrics.io/) - For running Uptime Kuma in the background

```bash
git clone https://github.com/louislam/uptime-kuma.git
cd uptime-kuma
npm run setup

# Option 1. Try it
node server/server.js

# (Recommended) Option 2. Run in the background using PM2
# Install PM2 if you don't have it:
npm install pm2 -g && pm2 install pm2-logrotate

# Start Server
pm2 start server/server.js --name uptime-kuma
```

Uptime Kuma is now running on all network interfaces (e.g. http://localhost:3001 or http://your-ip:3001).

More useful PM2 Commands

```bash
# If you want to see the current console output
pm2 monit

# If you want to add it to startup
pm2 startup && pm2 save
```

### Advanced Installation

If you need more options or need to browse via a reverse proxy, please read:

<https://github.com/louislam/uptime-kuma/wiki/%F0%9F%94%A7-How-to-Install>

## 🆙 How to Update

Please read:

<https://github.com/louislam/uptime-kuma/wiki/%F0%9F%86%99-How-to-Update>

## 🆕 What's Next?

I will assign requests/issues to the next milestone.

<https://github.com/louislam/uptime-kuma/milestones>

## ❤️ Sponsors

Thank you so much for supporting this project!

### Special Sponsors

[<img src="https://avatars.githubusercontent.com/u/249862100?s=200&v=4">](https://www.atlascloud.ai)

Atlas Cloud (https://www.atlascloud.ai) is sponsoring this project USD 400 per month for using their AI models.

[![Powered by Atlas Cloud](https://www.atlascloud.ai/oss-program/powered-by-atlas-cloud.svg)](https://www.atlascloud.ai/?ref=UZCYBN)

### Open Collective and GitHub Sponsors

(The list will be cached by GitHub, it may take up 24 hours to be updated)

<img src="https://raw.githubusercontent.com/louislam/uptime-kuma-website/refs/heads/master/sponsors.svg?version=2" alt="Uptime Kuma Sponsors" />

## 🖼 More Screenshots

Light Mode:

<img src="https://uptime.kuma.pet/img/light.jpg" width="512" alt="Uptime Kuma Light Mode Screenshot of how the Dashboard looks" />

Status Page:

<img src="https://user-images.githubusercontent.com/1336778/134628766-a3fe0981-0926-4285-ab46-891a21c3e4cb.png" width="512" alt="Uptime Kuma Status Page Screenshot" />

Settings Page:

<img src="https://louislam.net/uptimekuma/2.jpg" width="400" alt="Uptime Kuma Settings Page Screenshot" />

Telegram Notification Sample:

<img src="https://louislam.net/uptimekuma/3.jpg" width="400" alt="Uptime Kuma Telegram Notification Sample Screenshot" />

## Motivation

- I was looking for a self-hosted monitoring tool like "Uptime Robot", but it is hard to find a suitable one. One of the closest ones is statping. Unfortunately, it is not stable and no longer maintained.
- Wanted to build a fancy UI.
- Learn Vue 3 and vite.js.
- Show the power of Bootstrap 5.
- Try to use WebSocket with SPA instead of a REST API.
- Deploy my first Docker image to Docker Hub.

If you love this project, please consider giving it a ⭐.

## 🗣️ Discussion / Ask for Help

⚠️ For any general or technical questions, please don't send me an email, as I am unable to provide support in that manner. I will not respond if you ask questions there.

I recommend using Google, GitHub Issues, or Uptime Kuma's subreddit for finding answers to your question. If you cannot find the information you need, feel free to ask:

- [GitHub Issues](https://github.com/louislam/uptime-kuma/issues)
- [Subreddit (r/UptimeKuma)](https://www.reddit.com/r/UptimeKuma/)

My Reddit account: [u/louislamlam](https://reddit.com/u/louislamlam)
You can mention me if you ask a question on the subreddit.

## Contributions

### Create Pull Requests

Pull requests are awesome.
To keep reviews fast and effective, please make sure you’ve [read our pull request guidelines](https://github.com/louislam/uptime-kuma/blob/master/CONTRIBUTING.md#can-i-create-a-pull-request-for-uptime-kuma).

### Test Pull Requests

There are a lot of pull requests right now, but I don't have time to test them all.

If you want to help, you can check this:
<https://github.com/louislam/uptime-kuma/wiki/Test-Pull-Requests>

### Test Beta Version

Check out the latest beta release here: <https://github.com/louislam/uptime-kuma/releases>

### Bug Reports / Feature Requests

If you want to report a bug or request a new feature, feel free to open a [new issue](https://github.com/louislam/uptime-kuma/issues).

### Translations

If you want to translate Uptime Kuma into your language, please visit [Weblate Readme](https://github.com/louislam/uptime-kuma/blob/master/src/lang/README.md).

### Spelling & Grammar

Feel free to correct the grammar in the documentation or code.
My mother language is not English and my grammar is not that great.

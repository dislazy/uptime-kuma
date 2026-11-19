# Uptime Kuma Go 后端（go_server）镜像
#
# 构建上下文必须是仓库根目录（需要前端源码）：
#   docker build -f go_server/Dockerfile -t uptime-kuma-go:dev .
#
# 之所以分三个阶段：前端用 Node 构建、后端用 Go 编译，最终镜像里两者都不需要，
# 只留一个静态二进制 + dist 资源 + 时区数据。

############################################
# 1) 构建前端
############################################
# 上游 package.json 要求 node >= 26；若你的环境只有 22/24，把这里换成 node:24-alpine
# 并确认 npm run build 能通过（前端本身对 Node 版本不敏感）。
FROM node:26-alpine AS web
WORKDIR /src

COPY package.json package-lock.json .npmrc ./
RUN npm ci

COPY index.html vite.config.mjs tsconfig.json ./
COPY src ./src
COPY public ./public

RUN npm run build

############################################
# 2) 编译 Go 后端
############################################
FROM golang:1.26-alpine AS build
WORKDIR /src/go_server

# 先只拷依赖清单，让 go mod download 这层能被缓存
COPY go_server/go.mod ./
COPY go_server/go.sum* ./
RUN go mod download || true

COPY go_server/ ./

# 把前端产物放进来，main.go 通过 -web-dir 指向它
COPY --from=web /src/dist ./web/dist

ARG VERSION=dev
ARG COMMIT=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X github.com/songbo/uptime-kuma/go_server/internal/version.Version=${VERSION} -X github.com/songbo/uptime-kuma/go_server/internal/version.Commit=${COMMIT}" \
        -o /out/go_server ./cmd/go_server

############################################
# 3) 运行镜像
############################################
FROM alpine:3.21

# ca-certificates: HTTPS 监控与通知渠道
# tzdata:          时区换算（心跳时间按服务器时区展示）
# iputils:         ping 类监控依赖系统 ping 命令
RUN apk add --no-cache ca-certificates tzdata iputils \
    && ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app

COPY --from=build /out/go_server /app/go_server
COPY --from=build /src/go_server/web/dist /app/web/dist

# 与官方保持一致的约定：端口 3001、数据目录 /app/data
ENV UPTIME_KUMA_PORT=3001 \
    DATA_DIR=/app/data \
    UPTIME_KUMA_WEB_DIR=/app/web/dist

VOLUME ["/app/data"]
EXPOSE 3001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/go_server", "-healthcheck"]

ENTRYPOINT ["/app/go_server"]

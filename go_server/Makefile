.PHONY: help tidy build run dev test lint docker clean web

BINARY := go_server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w \
	-X github.com/songbo/uptime-kuma/go_server/internal/version.Version=$(VERSION) \
	-X github.com/songbo/uptime-kuma/go_server/internal/version.Commit=$(COMMIT)

help:
	@echo "make tidy    整理依赖（首次必跑）"
	@echo "make web     构建前端到 ../dist"
	@echo "make build   编译二进制（含前端资源路径）"
	@echo "make run     本地运行（需先 make web）"
	@echo "make test    跑单元测试"
	@echo "make docker  构建镜像"
	@echo "make clean   清理产物"

tidy:
	go mod tidy

# 二进制放在 go_server/ 下，前端产物由 -web-dir 指向仓库根的 dist
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/go_server

web:
	cd .. && npm run build

run: build
	./$(BINARY) -web-dir ../dist -debug

test:
	go test ./... -count=1

test-race:
	go test ./... -count=1 -race

lint:
	go vet ./...

docker:
	cd .. && docker build -f go_server/Dockerfile -t uptime-kuma-go:$(VERSION) .

clean:
	rm -f $(BINARY)
	rm -rf web/dist

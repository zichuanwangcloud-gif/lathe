# syntax=docker/dockerfile:1
# Lathe 一体化镜像：单二进制（内嵌 Web UI）+ git + docker CLI（预览环境用）
# 构建：docker build --build-arg VERSION=0.1.0 -t ghcr.io/zichuanwangcloud-gif/lathe:v0.1.0 .

########## 阶段 1：构建 Web UI ##########
FROM node:22-alpine AS ui
WORKDIR /build
# lockfile 是 pnpm；--ignore-scripts 避开 esbuild 安装脚本的交互确认（同 Makefile ui-deps）
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile --ignore-scripts
COPY web/ ./
RUN node_modules/.bin/vite build

########## 阶段 2：构建 Go 二进制（注入版本号，go:embed 内嵌 UI） ##########
FROM golang:1.25-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 用阶段 1 的真实产物覆盖内嵌目录（库里只有占位 .gitkeep）
COPY --from=ui /build/dist ./internal/webui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/lathe ./cmd/lathe

########## 阶段 3：运行时 ##########
FROM alpine:3.21
# git：worktree 执行；docker CLI + compose：任务预览环境（需挂载 /var/run/docker.sock）
# 注意：镜像不含 claude CLI —— agent 执行依赖它，需另行挂载/安装，见 release notes
RUN apk add --no-cache ca-certificates git tzdata docker-cli docker-cli-compose
LABEL org.opencontainers.image.source=https://github.com/zichuanwangcloud-gif/lathe
# 数据（secret.key、验证日志）与工作区（worktree/mirror）都收进一个卷
ENV LATHE_DATA_DIR=/data \
    LATHE_WORKSPACE_ROOT=/data/workspaces
VOLUME /data
COPY --from=go /out/lathe /usr/local/bin/lathe
EXPOSE 8200
ENTRYPOINT ["lathe"]
CMD ["serve"]

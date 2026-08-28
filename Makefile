.PHONY: help all build test test-race lint run migrate dev-infra dev-infra-down ui ui-deps ui-dev clean

BIN_DIR    := bin
CTRL_BIN   := $(BIN_DIR)/lathe
RUNNER_BIN := $(BIN_DIR)/lathe-runner
UI_SRC     := web/dist
UI_EMBED   := internal/webui/dist

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

ui: ## 构建管理界面并同步到内嵌目录
	# 依赖已在 pnpm-lock.yaml 锁定；此处不跑 pnpm install，因为 pnpm 会
	# 因 esbuild 的安装脚本停在交互确认上。首次构建请手动执行 make ui-deps。
	cd web && node_modules/.bin/vite build
	rm -rf $(UI_EMBED)
	cp -r $(UI_SRC) $(UI_EMBED)
	@echo "→ 界面已同步到 $(UI_EMBED)"

ui-deps: ## 安装前端依赖（首次或依赖变更后执行）
	cd web && pnpm install --ignore-scripts

ui-dev: ## 前端热重载（需另起 make run）
	cd web && node_modules/.bin/vite

build: ## 编译控制面与节点代理（不重建界面，用 make all 一起构建）
	@mkdir -p $(BIN_DIR)
	go build -o $(CTRL_BIN) ./cmd/lathe
	go build -o $(RUNNER_BIN) ./cmd/lathe-runner
	@echo "→ $(CTRL_BIN) $(RUNNER_BIN)"

all: ui build ## 构建界面与二进制

test: ## 跑测试
	# -p 1：DB 领单调度器（internal/task.Machine.ClaimReady）是全局查询，不按包/fixture
	# 隔离，多个测试包默认并行跑在同一个真实 Postgres 上会互相抢/污染对方建的 'queued'
	# 行，产生间歇性 FAIL（与代码正确性无关，是共享测试库的既有限制）。强制包间串行即可。
	go test -p 1 ./... -count=1

test-race: ## 跑并发相关测试的 -race 检测（F2.1-AC5；本地目标，未接入 CI）
	# -p 1：这几个包的测试都跑在同一个真实 Postgres 上，ClaimReady 之类
	# 的查询是全局的（不按包/fixture 隔离），多个包的测试二进制被 go
	# test 默认并行跑起来时会互相在数据库里踩到对方的行，所以强制包间
	# 串行；-race 检测的是包内 goroutine 竞态，与包间串行不冲突。
	go test -p 1 ./cmd/lathe/... ./internal/task/... ./internal/runner/... ./internal/flow/... -race -count=1

lint: ## 静态检查
	go vet ./...
	gofmt -l -e .

run: build ## 起控制面
	$(CTRL_BIN)

migrate: ## 应用数据库迁移
	$(CTRL_BIN) migrate up

dev-infra: ## 起本地 Postgres（端口 55432，避开常见占用）
	docker compose -f docker-compose.dev.yml up -d

dev-infra-down: ## 停本地 Postgres
	docker compose -f docker-compose.dev.yml down

clean: ## 清理构建产物
	rm -rf $(BIN_DIR) $(UI_SRC) $(UI_EMBED)

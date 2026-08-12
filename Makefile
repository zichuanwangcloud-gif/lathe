.PHONY: help all build test lint run migrate dev-infra dev-infra-down ui ui-deps ui-dev clean

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
	go test ./... -count=1

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

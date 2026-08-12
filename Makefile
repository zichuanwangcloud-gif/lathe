.PHONY: help build test lint run migrate dev-infra dev-infra-down ui clean

BIN_DIR    := bin
CTRL_BIN   := $(BIN_DIR)/lathe
RUNNER_BIN := $(BIN_DIR)/lathe-runner
PG_PORT    ?= 55432

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译控制面与节点代理
	@mkdir -p $(BIN_DIR)
	go build -o $(CTRL_BIN) ./cmd/lathe
	go build -o $(RUNNER_BIN) ./cmd/lathe-runner
	@echo "→ $(CTRL_BIN) $(RUNNER_BIN)"

test: ## 跑测试
	go test ./... -count=1

lint: ## 静态检查
	go vet ./...
	gofmt -l -e .

run: build ## 起控制面
	$(CTRL_BIN)

migrate: ## 应用数据库迁移
	$(CTRL_BIN) migrate up

dev-infra: ## 起本地 Postgres（端口 $(PG_PORT)，避开现有容器）
	docker compose -f docker-compose.dev.yml up -d

dev-infra-down: ## 停本地 Postgres
	docker compose -f docker-compose.dev.yml down

ui: ## 构建 Vue SPA 到 web/dist（供 go:embed）
	cd web && pnpm install && pnpm build

clean: ## 清理构建产物
	rm -rf $(BIN_DIR) web/dist

.PHONY: help all build test test-ci test-race lint run migrate dev-infra dev-infra-down clean-test-db ui ui-deps ui-dev clean

BIN_DIR    := bin
CTRL_BIN   := $(BIN_DIR)/lathe
UI_SRC     := web/dist
UI_EMBED   := internal/webui/dist

# 发布构建注入版本号（make build VERSION=0.1.0）；不传则二进制自报 dev。
VERSION    ?=
LDFLAGS    := $(if $(VERSION),-ldflags "-X main.version=$(VERSION)",)

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

ui: ## 构建管理界面并同步到内嵌目录
	# 依赖已在 pnpm-lock.yaml 锁定；此处不跑 pnpm install，因为 pnpm 会
	# 因 esbuild 的安装脚本停在交互确认上。首次构建请手动执行 make ui-deps。
	cd web && node_modules/.bin/vite build
	rm -rf $(UI_EMBED)
	cp -r $(UI_SRC) $(UI_EMBED)
	# 补回占位文件：上面的 rm -rf 会把它删掉，而它是入库的唯一 dist 内容 ——
	# 没有它，干净克隆里 //go:embed all:dist 找不到目录，go build/vet/test 全挂。
	@touch $(UI_EMBED)/.gitkeep
	@echo "→ 界面已同步到 $(UI_EMBED)"

ui-deps: ## 安装前端依赖（首次或依赖变更后执行）
	cd web && pnpm install --ignore-scripts

ui-dev: ## 前端热重载（需另起 make run）
	cd web && node_modules/.bin/vite

build: ## 编译控制面（不重建界面，用 make all 一起构建）
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(CTRL_BIN) ./cmd/lathe
	@echo "→ $(CTRL_BIN)"

all: ui build ## 构建界面与二进制

test: ## 跑测试
	# -p 1：DB 领单调度器（internal/task.Machine.ClaimReady）是全局查询，不按包/fixture
	# 隔离，多个测试包默认并行跑在同一个真实 Postgres 上会互相抢/污染对方建的 'queued'
	# 行。强制包间串行即可。
	#
	# 注意 -p 1 只挡【包间并行】，挡不住【跨轮次的数据残留】—— 测试进程被杀时
	# t.Cleanup 不执行，fixture 行留在库里，下一轮的全局查询会捞到它们。
	# 那一层由测试侧自己扛：fixture 的 email 带随机量、领单断言按归属过滤、
	# internal/task 在建自己的任务前先排空队列。详见 docs/08-debt-cleanup.md §7。
	# 库里的残留用 make clean-test-db 清（默认干跑）。
	go test -p 1 ./... -count=1

test-ci: ## 按 CI 的严格口径跑测试（库连不上直接失败，不静默跳过）
	# 与 make test 的唯一区别是 LATHE_TEST_REQUIRE_DB —— 数据库 helper 在
	# 连不上库时不再 t.Skip 而是 t.Fatal。CI 里必须这么跑：否则 Postgres 起
	# 晚一秒或 DSN 写错一个字符，整套数据库测试会被静默跳过，流水线全绿却
	# 什么都没验证。本地想复现 CI 的判定口径时用它。
	LATHE_TEST_REQUIRE_DB=1 go test -p 1 ./... -count=1

test-race: ## 跑并发相关测试的 -race 检测（F2.1-AC5；本地目标，未接入 CI）
	# -p 1：这几个包的测试都跑在同一个真实 Postgres 上，ClaimReady 之类
	# 的查询是全局的（不按包/fixture 隔离），多个包的测试二进制被 go
	# test 默认并行跑起来时会互相在数据库里踩到对方的行，所以强制包间
	# 串行；-race 检测的是包内 goroutine 竞态，与包间串行不冲突。
	go test -p 1 ./cmd/lathe/... ./internal/task/... ./internal/runner/... ./internal/flow/... -race -count=1

lint: ## 静态检查
	go vet ./...
	# 排除 .claude/：那是 Claude Code 的本地 worktree，里面是别的分支的
	# 检出副本，不是本仓工作树文件，扫它只会报无关噪声。
	gofmt -l -e $$(git ls-files "*.go")

run: build ## 起控制面
	$(CTRL_BIN)

migrate: ## 应用数据库迁移
	$(CTRL_BIN) migrate up

dev-infra: ## 起本地 Postgres（端口 55432，避开常见占用）
	docker compose -f docker-compose.dev.yml up -d

dev-infra-down: ## 停本地 Postgres
	docker compose -f docker-compose.dev.yml down

clean-test-db: ## 报告开发库里的测试残留（加 YES=1 真删）
	scripts/clean-test-db.sh $(if $(YES),--yes,)

clean: ## 清理构建产物
	rm -rf $(BIN_DIR) $(UI_SRC) $(UI_EMBED)

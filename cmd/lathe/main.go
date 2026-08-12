// Command lathe 是 Lathe 的控制面：HTTP API、任务状态机、串行执行队列。
//
// P0 形态：单机、单用户、串行。多用户与多节点见 docs/02-design.md §8。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/creds"
	"github.com/Clouditera/lathe/internal/httpapi"
	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/secret"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/Clouditera/lathe/internal/webui"
)

func main() {
	if err := run(); err != nil {
		slog.Error("控制面退出", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return runMigrate(cfg, os.Args[2:])
		case "version":
			fmt.Println("lathe dev")
			return nil
		case "serve":
			// 显式 serve 与默认行为一致
		default:
			return fmt.Errorf("未知子命令 %q（可用：serve, migrate, version）", os.Args[1])
		}
	}
	return serve(cfg)
}

func runMigrate(cfg config.Config, args []string) error {
	dir := "up"
	if len(args) > 0 {
		dir = args[0]
	}
	if dir != "up" && dir != "down" {
		return fmt.Errorf("migrate 方向须为 up 或 down，得到 %q", dir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	st, err := store.Open(ctx, cfg.Database.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	if dir == "up" {
		return st.MigrateUp(ctx)
	}
	return st.MigrateDown(ctx)
}

func serve(cfg config.Config) error {
	// 收到 TERM/INT 时让在途任务收到 context 取消，而不是被硬杀
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Lathe 控制面启动", "config", cfg.Redacted())

	st, err := store.Open(ctx, cfg.Database.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	// 凭据加密存储：主密钥在数据库之外，拿到库转储也解不出凭据
	key, keySource, err := secret.LoadKey(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return err
	}
	sealer, err := secret.New(key)
	if err != nil {
		return err
	}
	slog.Info("凭据加密已就绪", "key_source", keySource)

	secrets := st.NewSecrets(sealer)
	userID, err := ensureUser(ctx, st, cfg)
	if err != nil {
		return err
	}

	provider := creds.NewProvider(secrets, userID, creds.EnvFallback{
		LinearToken:         cfg.LinearToken,
		LinearWebhookSecret: cfg.LinearWebhookSecret,
		GitHubToken:         cfg.GitHubToken,
		LinearUserID:        os.Getenv("LATHE_LINEAR_USER_ID"),
	})

	pipeline, err := buildPipeline(cfg, st, creds.NewClients(provider))
	if err != nil {
		return err
	}

	if ready, missing := provider.Ready(ctx); !ready {
		slog.Warn("凭据尚不完整，请在设置页配置后再触发任务", "缺少", missing)
	}

	// P0 串行队列：一次只跑一个任务，彻底绕开端口冲突与 DB 隔离问题
	// （docs/02-design.md §8 —— 并发留到 P1）
	q := newQueue(st, task.NewMachine(st.Pool()), pipeline, cfg)
	go q.work(ctx)

	adminToken := os.Getenv("LATHE_ADMIN_TOKEN")
	auth := httpapi.NewAuth(adminToken)
	if !auth.Enabled() {
		// 不因此拒绝启动：webhook 链路不依赖管理界面，
		// 但必须让人知道界面为什么打不开
		slog.Warn("未配置 LATHE_ADMIN_TOKEN，管理界面与 API 已禁用（webhook 仍正常工作）")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpapi.Health)
	mux.Handle("POST /webhooks/linear", &httpapi.LinearWebhook{
		// 用函数取值而非固定字符串：凭据可在界面里改，改完即刻生效
		SecretFunc: func() string { return provider.WebhookSecret(context.Background()) },
		UserIDFunc: func() string { return provider.LinearUserID(context.Background()) },
		Deliveries: st,
		Tasks:      q,
	})

	apiSrv := &httpapi.API{
		Store:        st,
		Tasks:        task.NewMachine(st.Pool()),
		Queue:        q,
		Auth:         auth,
		ConfigStatus: configStatus(cfg),
	}
	apiSrv.Routes(mux)

	credAPI := &httpapi.CredentialAPI{
		Secrets:  secrets,
		UserID:   userID,
		Verifier: creds.Verifier{},
		Auth:     auth,
		OnChange: provider.Invalidate,
		EnvConfigured: func(kind string) bool {
			switch kind {
			case store.KindLinear:
				return cfg.LinearToken != ""
			case store.KindLinearWebhook:
				return cfg.LinearWebhookSecret != ""
			case store.KindGitHub:
				return cfg.GitHubToken != ""
			}
			return false
		},
	}
	credAPI.Routes(mux)

	if webui.Available() {
		mux.Handle("/", webui.Handler())
	} else {
		slog.Warn("管理界面未构建进二进制，执行 make ui 后重新编译")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "管理界面未构建：请执行 make ui && make build", http.StatusNotImplemented)
		})
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP 服务监听中", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("收到关闭信号，正在停止")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// buildPipeline 装配流水线。
//
// 刻意不在此校验 Linear/GitHub 凭据：凭据现在可在界面里配置，
// 缺凭据不该阻止服务启动 —— 否则新用户连配置页都打不开。
// 真正需要凭据时（执行任务）才会报错，并指引去设置页。
func buildPipeline(cfg config.Config, st *store.Store, clients runner.Clients) (*runner.Pipeline, error) {
	wm, err := runner.NewWorktreeManager(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	return &runner.Pipeline{
		Tasks:          task.NewMachine(st.Pool()),
		Worktrees:      wm,
		Verifier:       runner.NewVerifier(15*time.Minute, cfg.PnpmStore),
		Agent:          agent.NewDriver(cfg.ClaudeBin, cfg.AgentTimeout),
		Clients:        clients,
		Notifier:       logNotifier{},
		PermissionMode: "acceptEdits",
	}, nil
}

// configStatus 生成给管理界面的配置状态。
//
// 只报告「配没配、从哪个环境变量读」，绝不包含凭据内容本身。
func configStatus(cfg config.Config) func() map[string]any {
	return func() map[string]any {
		item := func(v string) map[string]any {
			return map[string]any{"configured": v != ""}
		}
		return map[string]any{
			"linear":        item(cfg.LinearToken),
			"linearWebhook": item(cfg.LinearWebhookSecret),
			"linearUser":    item(os.Getenv("LATHE_LINEAR_USER_ID")),
			"github":        item(cfg.GitHubToken),
			"runtime": map[string]any{
				"node":          cfg.NodeName,
				"workspaceRoot": cfg.WorkspaceRoot,
				"pnpmStore":     cfg.PnpmStore,
				"claudeBin":     cfg.ClaudeBin,
				"agentTimeout":  cfg.AgentTimeout.String(),
				"mode":          "P0 串行（单机单用户）",
			},
		}
	}
}

// logNotifier 是 P0 的占位通知实现：先写日志。
// 真正的推送通道（终端/手机）留到 P2 随 Web UI 一起做。
type logNotifier struct{}

func (logNotifier) Notify(ctx context.Context, msg string) error {
	slog.Warn("【通知】" + msg)
	return nil
}

// ensureUser 取得（或创建）P0 单用户模式下的用户记录。
//
// 凭据、仓库配置都挂在 user 上；P0 只有一个用户，首次启动自动建好，
// 免得新用户还要先手工往表里插一条才能用界面。
func ensureUser(ctx context.Context, st *store.Store, cfg config.Config) (int64, error) {
	var id int64
	err := st.Pool().QueryRow(ctx, `
		INSERT INTO users (email) VALUES ($1)
		ON CONFLICT (email) DO UPDATE SET updated_at = now()
		RETURNING id`, cfg.AdminEmail).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("初始化用户失败: %w", err)
	}
	return id, nil
}

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
	"syscall"
	"time"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/httpapi"
	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
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

	pipeline, err := buildPipeline(cfg, st)
	if err != nil {
		return err
	}

	// P0 串行队列：一次只跑一个任务，彻底绕开端口冲突与 DB 隔离问题
	// （docs/02-design.md §8 —— 并发留到 P1）
	q := newQueue(st, task.NewMachine(st.Pool()), pipeline, cfg)
	go q.work(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpapi.Health)
	mux.Handle("POST /webhooks/linear", &httpapi.LinearWebhook{
		Secret:     cfg.LinearWebhookSecret,
		UserID:     os.Getenv("LATHE_LINEAR_USER_ID"),
		Deliveries: st,
		Tasks:      q,
	})

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

func buildPipeline(cfg config.Config, st *store.Store) (*runner.Pipeline, error) {
	if cfg.LinearToken == "" {
		return nil, errors.New("未配置 LATHE_LINEAR_TOKEN")
	}
	if cfg.GitHubToken == "" {
		return nil, errors.New("未配置 LATHE_GITHUB_TOKEN")
	}

	lin, err := linear.NewClient(cfg.LinearToken)
	if err != nil {
		return nil, err
	}
	ghc, err := github.NewClient(cfg.GitHubToken)
	if err != nil {
		return nil, err
	}
	wm, err := runner.NewWorktreeManager(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	return &runner.Pipeline{
		Tasks:          task.NewMachine(st.Pool()),
		Worktrees:      wm,
		Verifier:       runner.NewVerifier(15*time.Minute, cfg.PnpmStore),
		Agent:          agent.NewDriver(cfg.ClaudeBin, cfg.AgentTimeout),
		Linear:         lin,
		GitHub:         ghc,
		Notifier:       logNotifier{},
		PermissionMode: "acceptEdits",
	}, nil
}

// logNotifier 是 P0 的占位通知实现：先写日志。
// 真正的推送通道（终端/手机）留到 P2 随 Web UI 一起做。
type logNotifier struct{}

func (logNotifier) Notify(ctx context.Context, msg string) error {
	slog.Warn("【通知】" + msg)
	return nil
}

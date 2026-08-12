// Command lathe 是 Lathe 的控制面：HTTP API、任务状态机、调度器与内嵌 UI。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/store"
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// 子命令：migrate up|down
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return runMigrate(cfg, os.Args[2:])
		case "version":
			fmt.Println("lathe dev")
			return nil
		default:
			return fmt.Errorf("未知子命令 %q（可用：migrate, version）", os.Args[1])
		}
	}

	// 信号驱动的优雅关闭：控制面要能长期驻留，收到 TERM/INT 时
	// 必须让在途任务收到 context 取消，而不是被硬杀。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Lathe 控制面启动", "config", cfg.Redacted())

	// TODO(task#8): serve 路径装配 store（store 包已就绪，见 internal/store）
	// TODO(task#3): 装配任务状态机
	// TODO(task#4): 挂载 Linear webhook ingress
	// TODO(task#1-ui): go:embed web/dist 提供 SPA
	slog.Warn("控制面骨架就位，业务组件尚未装配（见 cmd/lathe/main.go TODO）")

	<-ctx.Done()
	slog.Info("收到关闭信号，正在停止")
	return nil
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

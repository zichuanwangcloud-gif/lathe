// Command lathe-runner 是节点代理：向控制面领取任务，在本节点上
// 管理 worktree、驱动 claude CLI、跑验证，并续租任务租约。
//
// 设计要点：runner 是进程监管器。它必须保证自己退出时不留下游荡的
// claude 子进程或悬空的 compose 栈，因此所有子进程都放进独立进程组，
// 由 context 取消统一杀进程树。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Clouditera/lathe/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("节点代理退出", "err", err)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Lathe 节点代理启动",
		"node", cfg.NodeName,
		"workspace_root", cfg.WorkspaceRoot,
		"claude_bin", cfg.ClaudeBin,
		"agent_timeout", cfg.AgentTimeout,
	)

	// TODO(task#5): 装配 claude CLI driver（stream-json / resume / --from-pr）
	// TODO(task#6): 装配 worktree 管理与 light 档验证
	// TODO(task#6): 上报节点能力（docker 可用性、内存/磁盘余量）
	slog.Warn("节点代理骨架就位，执行组件尚未装配（见 cmd/lathe-runner/main.go TODO）")

	<-ctx.Done()
	slog.Info("收到关闭信号，正在停止在途任务")
	// TODO(task#5): 等待在途任务收敛并确认子进程树已回收
	return nil
}

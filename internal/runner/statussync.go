package runner

import (
	"context"
	"log/slog"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

// syncTrackerStatus 把任务生命周期同步回需求平台（f08 云效状态同步）。
//
// 这是 tracker.StatusSyncer 唯一的类型断言点（docs/features/tracker-yunxiao.md
// R9）：tr 不实现它（Linear、内置工单）就直接返回，行为与本功能之前逐字节
// 一致；实现了就调用，**错误只告警、绝不返回、绝不影响状态机**——调用点
// 都在状态转移成功之后，同步只是尽力而为。
//
// ref 与同一处的回帖用同一个引用（pipeline 是 ExecuteParams.IssueRef，
// mergepoll 是 IssueRefOf(tk)）。
func syncTrackerStatus(ctx context.Context, tr tracker.Tracker, tk *task.Task, ref string, ev tracker.Lifecycle) {
	if tr == nil || tk == nil {
		return
	}
	s, ok := tr.(tracker.StatusSyncer)
	if !ok {
		return
	}
	if ref == "" {
		return
	}
	if err := s.SyncStatus(ctx, ref, ev); err != nil {
		slog.Warn("需求平台状态同步失败（不影响任务状态）",
			"task", tk.ID, "provider", tk.TrackerProvider, "event", string(ev), "err", err)
	}
}

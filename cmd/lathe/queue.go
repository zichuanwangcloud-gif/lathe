package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// queueDepth 是待处理任务的缓冲上限。
//
// 有界而非无界：队列堆积说明处理速度跟不上接单速度，此时快速失败
// 并回帖告知，比默默积压几百个任务更诚实。
const queueDepth = 64

// job 是一条待执行的任务。
type job struct {
	IssueID  string
	IssueKey string
}

// queue 是 P0 的串行执行队列。
//
// 刻意串行：一次只跑一个任务，彻底绕开端口冲突与数据库隔离问题
// （见 docs/02-design.md §8，并发留到 P1）。按每单 2 小时估算，
// 无人值守串行一天可消化约 12 单，正好覆盖当前峰值吞吐。
type queue struct {
	store    *store.Store
	tasks    *task.Machine
	pipeline *runner.Pipeline
	cfg      config.Config
	ch       chan job
}

func newQueue(st *store.Store, tm *task.Machine, p *runner.Pipeline, cfg config.Config) *queue {
	return &queue{
		store: st, tasks: tm, pipeline: p, cfg: cfg,
		ch: make(chan job, queueDepth),
	}
}

// Enqueue 实现 httpapi.TaskEnqueuer。
func (q *queue) Enqueue(ctx context.Context, issueID, issueKey string) error {
	select {
	case q.ch <- job{IssueID: issueID, IssueKey: issueKey}:
		return nil
	default:
		return fmt.Errorf("执行队列已满（上限 %d），暂时无法接单", queueDepth)
	}
}

// work 串行消费队列，直到 ctx 结束。
func (q *queue) work(ctx context.Context) {
	slog.Info("执行队列已启动（P0 串行模式）", "depth", queueDepth)
	for {
		select {
		case <-ctx.Done():
			slog.Info("执行队列停止")
			return
		case j := <-q.ch:
			q.runOne(ctx, j)
		}
	}
}

func (q *queue) runOne(ctx context.Context, j job) {
	slog.Info("开始处理", "issue", j.IssueKey)

	userID, repoID, repoCfg, cloneURL, err := q.resolveRepo(ctx, j)
	if err != nil {
		slog.Error("无法确定任务归属仓库", "issue", j.IssueKey, "err", err)
		return
	}

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: j.IssueKey,
	})
	if err != nil {
		// 同一 issue 已有活任务时会撞上部分唯一索引 —— 这是预期行为，非错误
		slog.Warn("建任务失败（可能已有进行中的同名任务）", "issue", j.IssueKey, "err", err)
		return
	}

	if err := q.pipeline.Execute(ctx, runner.ExecuteParams{
		TaskID:   tk.ID,
		Repo:     repoCfg,
		CloneURL: cloneURL,
		IssueID:  j.IssueID,
		Actor:    "node:" + q.cfg.NodeName,
	}); err != nil {
		// 失败三件套已在 pipeline 内部完成，这里只记日志
		slog.Error("任务处理失败", "issue", j.IssueKey, "task", tk.ID, "err", err)
		return
	}
	slog.Info("任务处理完成", "issue", j.IssueKey, "task", tk.ID)
}

// resolveRepo 查出要操作的仓库。
//
// 当前单仓：取第一条 repos 记录。账号体系的第一步数据仍然共享，所有任务都
// 落在同一份仓库配置上；按用户隔离与按 issue 的 team/project 路由到不同仓库
// 都留到第二步（见 docs/02-design.md §8）。
func (q *queue) resolveRepo(ctx context.Context, j job) (userID, repoID int64, cfg runner.RepoConfig, cloneURL string, err error) {
	var (
		providerRepo  string
		defaultBranch string
		hotfixBase    string
		protected     []string
		pattern       string
		tierOverride  string
	)
	err = q.store.Pool().QueryRow(ctx, `
		SELECT user_id, id, provider_repo, default_branch, hotfix_base, protected_branches, branch_pattern,
		       COALESCE(verify_tier_override, '')
		FROM repos ORDER BY id LIMIT 1`,
	).Scan(&userID, &repoID, &providerRepo, &defaultBranch, &hotfixBase, &protected, &pattern, &tierOverride)
	if err != nil {
		return 0, 0, cfg, "", fmt.Errorf("读取仓库配置失败（P0 需先在 repos 表插入一条记录）: %w", err)
	}

	cfg = runner.RepoConfig{
		ProviderRepo:       providerRepo,
		DefaultBranch:      defaultBranch,
		HotfixBase:         hotfixBase,
		ProtectedBranches:  protected,
		BranchPattern:      pattern,
		VerifyTierOverride: tierOverride,
	}
	return userID, repoID, cfg, "git@github.com:" + providerRepo + ".git", nil
}

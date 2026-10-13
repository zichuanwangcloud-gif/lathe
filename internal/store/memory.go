package store

// memory.go 跨任务失败记忆（docs/05-roadmap.md §3.6 第 16 条，migration 0030）。
//
// 与修复回路（docs/02-design.md §5）互补：回路管「单次任务内，验证挂了就地
// 修」；这里管「跨任务，这个单子上次怎么死的」。没有它，重试的 agent 在
// 同一个坑上再烧一轮 token —— 而它本可以一开始就绕开。
//
// 表结构与设计理由见 migrations/0030_failure_digests.up.sql。这里只重复
// 两件在读取侧必须记住的事：
//
//  1. **定位键是「这个单子」不是「这一行」**：(repo_id, tracker_provider,
//     external_key)。人工重试走原任务行，但 issue 关掉重开、内置工单重新
//     开跑都会新建任务行 —— 两种形态都得读到历史。
//  2. **多租户**：user_id 是显式列（不靠 repo_id 隐式限定），所以这里的
//     查询不必 JOIN 就能过滤，少一处可以漏掉 JOIN 的地方。

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// FailureDigest 是一次失败的机器可读摘要。
//
// 同一个类型被 runner 的 FailureMemory 接口直接使用（*store.Store 实现该
// 接口），所以这里**不另立一份同形结构**：复制一份就要手工保持同步，而
// 两份的字段集完全一样。
type FailureDigest struct {
	ID     int64  `json:"id"`
	TaskID int64  `json:"taskId"`
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
	// Excerpt 是验证输出摘要。写入侧按 runner 的上限截断，读取侧原样返回。
	Excerpt string `json:"excerpt"`
	// At 是格式化后的时间（YYYY-MM-DD HH:MM，UTC）—— 直接给提示词渲染用，
	// 不再让消费方各自格式化一遍。
	At string `json:"at"`
}

// FailureLookup 定位一个单子的历史失败。
type FailureLookup struct {
	UserID          int64
	RepoID          int64
	TrackerProvider string
	ExternalKey     string
}

// InsertFailureDigest 追加一条失败记忆。
//
// 失败记忆是排障信息，不是主流程的产物（与 InsertAgentEvents 同一立场），
// 调用方落库失败只该告警 —— 它绝不能反过来成为任务失败的第二个原因。
func (s *Store) InsertFailureDigest(ctx context.Context, d FailureDigest, l FailureLookup) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO failure_digests
			(user_id, repo_id, task_id, external_key, tracker_provider, failure_stage, failure_reason, excerpt)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), NULLIF($8,''))`,
		l.UserID, l.RepoID, d.TaskID, l.ExternalKey, l.TrackerProvider, d.Stage, d.Reason, d.Excerpt)
	if err != nil {
		return fmt.Errorf("store: 落失败记忆失败（task %d）: %w", d.TaskID, err)
	}
	return nil
}

// LatestFailureDigest 取指定单子最近一次失败。
//
// 查不到时返回 (nil, nil) —— 「没失败过」是正常状态而非错误，调用方
// （prompt 注入）据此让提示词保持不变。用 ErrNoRows 表达这个平常事
// 会把每个新建任务都变成一条要判错的路径。
//
// excludeTaskID 非零时排除该任务自己的记录：重试走的是原任务行，拿它
// 自己上一次的失败当「历史记忆」是同义反复 —— agent 已经在续跑路径上
// 拿到了那份失败原因（ResumeFixPrompt 就带着它），再注入一遍是重复。
// 0 表示不排除（新建任务场景）。
func (s *Store) LatestFailureDigest(ctx context.Context, l FailureLookup, excludeTaskID int64) (*FailureDigest, error) {
	var (
		d        FailureDigest
		stage    *string
		reason   *string
		excerpt  *string
		atString string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, task_id, failure_stage, failure_reason, excerpt,
		       to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI')
		FROM failure_digests
		WHERE user_id = $1 AND repo_id = $2 AND tracker_provider = $3
		  AND external_key = $4 AND ($5 = 0 OR task_id <> $5)
		ORDER BY at DESC, id DESC
		LIMIT 1`,
		l.UserID, l.RepoID, l.TrackerProvider, l.ExternalKey, excludeTaskID,
	).Scan(&d.ID, &d.TaskID, &stage, &reason, &excerpt, &atString)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询失败记忆失败: %w", err)
	}

	d.Stage = derefString(stage)
	d.Reason = derefString(reason)
	d.Excerpt = derefString(excerpt)
	d.At = atString
	return &d, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

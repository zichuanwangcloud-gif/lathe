package tracker

import (
	"context"
	"fmt"

	"github.com/Clouditera/lathe/internal/store"
)

// LocalTracker 是 Tracker 的内置实现：需求载体是 Lathe 自己的
// issues / issue_comments 表（docs/09-internal-issues.md）。
//
// 与 Linear 实现的对齐点只有两处，但都是硬约束：
//   - Issue() 返回的 Issue 拼 Context() 时格式与 Linear 完全一致
//     （同一个 Context 函数，天然一致）；
//   - Comment() 写库后，重试时 Issue() 必须能读回来 ——
//     blocked_spec 提问回路（问 → 人答 → 重试重读）由此闭合。
type LocalTracker struct {
	store  *store.Store
	owner  int64  // 工单属主（隔离边界；构造时来自任务属主，不信任入参）
	actor  string // 评论署名（"task-<id>"）；空 = 未署名（人通过 API 发的评论不走这里）
}

// NewLocalTracker 构造某属主视角的内置 tracker。
func NewLocalTracker(st *store.Store, ownerUserID int64) *LocalTracker {
	return &LocalTracker{store: st, owner: ownerUserID}
}

// WithActor 实现 AttributedTracker：返回以 actor 署名评论的句柄。
// pipeline 在建出 runCtx 后用它把任务 id 挂上去（"task-<id>"）。
func (t *LocalTracker) WithActor(actor string) Tracker {
	cp := *t
	cp.actor = actor
	return &cp
}

// Issue 按 key（LT-1042）拉取工单及其评论。
//
// ref 只接受 key —— 内置任务没有第二标识（tasks.external_id 为 NULL，
// 见 migration 0021 注释）。找不到时返回 store.ErrIssueNotFound，
// 由 pipeline 的失败路径翻译成人话（与 Linear 拉不到 issue 同待遇）。
func (t *LocalTracker) Issue(ctx context.Context, ref string) (*Issue, error) {
	it, err := t.store.GetIssueByKey(ctx, t.owner, ref)
	if err != nil {
		return nil, err
	}
	comments, err := t.store.ListComments(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	out := &Issue{
		ID:          it.Key, // 对内置实现来说 key 就是 ID
		Identifier:  it.Key,
		Title:       it.Title,
		Description: it.Description,
		StateName:   it.State,
		Priority:    it.Priority,
	}
	for _, c := range comments {
		out.Comments = append(out.Comments, Comment{
			ID:       fmt.Sprintf("%d", c.ID),
			Body:     c.Body,
			UserName: c.AuthorName,
		})
	}
	return out, nil
}

// Comment 在工单下追加一条署名评论。返回评论 id 的字符串形式。
func (t *LocalTracker) Comment(ctx context.Context, ref, body string) (string, error) {
	it, err := t.store.GetIssueByKey(ctx, t.owner, ref)
	if err != nil {
		return "", err
	}
	actor := t.actor
	if actor == "" {
		actor = "lathe"
	}
	c, err := t.store.AddComment(ctx, it.ID, nil, &actor, body)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", c.ID), nil
}

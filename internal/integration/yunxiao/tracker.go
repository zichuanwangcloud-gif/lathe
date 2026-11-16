package yunxiao

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

// ErrStatusNotFound 表示工作流里找不到用户配置的状态名（调用方告警，不重试）。
var ErrStatusNotFound = errors.New("yunxiao: 工作流里找不到配置的状态名")

// StatusMapping 是三个同步时刻各自要推到的状态名（显示名）；空 = 该时刻不同步。
type StatusMapping struct {
	Started string
	PROpen  string
	Merged  string
}

func (m StatusMapping) forEvent(ev tracker.Lifecycle) string {
	switch ev {
	case tracker.LifecycleStarted:
		return m.Started
	case tracker.LifecyclePROpen:
		return m.PROpen
	case tracker.LifecycleMerged:
		return m.Merged
	}
	return ""
}

// Tracker 是 tracker.Tracker + tracker.StatusSyncer 的云效实现。
// ref 是工作项 id（任务行的 external_id）。
type Tracker struct {
	c *Client
	m StatusMapping
}

var (
	_ tracker.Tracker      = (*Tracker)(nil)
	_ tracker.StatusSyncer = (*Tracker)(nil)
)

// NewTracker 用客户端与状态映射构造 tracker。
func NewTracker(c *Client, m StatusMapping) *Tracker { return &Tracker{c: c, m: m} }

// Issue 拉工作项与评论，映射成平台无关的 tracker.Issue。
//
// URL 留空：OpenAPI 不返回网页链接，网页地址的拼法官方没写，不猜。
func (t *Tracker) Issue(ctx context.Context, ref string) (*tracker.Issue, error) {
	wi, err := t.c.GetWorkitem(ctx, ref)
	if err != nil {
		return nil, err
	}
	cs, err := t.c.ListComments(ctx, wi.ID)
	if err != nil {
		return nil, err
	}
	iss := &tracker.Issue{
		ID: wi.ID, Identifier: wi.Key(), Title: wi.Subject,
		Description: ToPlainText(wi.Description, wi.FormatType),
		StateName:   firstNonEmpty(wi.Status.DisplayName, wi.Status.Name),
		AssigneeID:  wi.AssignedTo.ID,
	}
	for _, l := range wi.Labels {
		iss.Labels = append(iss.Labels, l.Name)
	}
	for _, c := range cs {
		iss.Comments = append(iss.Comments, tracker.Comment{ID: c.ID, Body: c.Content, UserName: c.User.Name})
	}
	return iss, nil
}

// Comment 以 MARKDOWN 发评论。
func (t *Tracker) Comment(ctx context.Context, ref, body string) (string, error) {
	return t.c.CreateComment(ctx, ref, body)
}

// SyncStatus 把工作项推到 ev 对应的状态（按名字配置、按 id 调用）。
//
// 映射为空直接返回 nil、不发任何请求；工作项已是目标状态时跳过 PUT
// （省一次写，也不触发对端自动化）。
func (t *Tracker) SyncStatus(ctx context.Context, ref string, ev tracker.Lifecycle) error {
	name := strings.TrimSpace(t.m.forEvent(ev))
	if name == "" {
		return nil
	}
	wf, err := t.c.Workflow(ctx, ref)
	if err != nil {
		return err
	}
	var target *Status
	for i := range wf.Statuses {
		if MatchStatus(wf.Statuses[i], name) {
			target = &wf.Statuses[i]
			break
		}
	}
	if target == nil || target.ID == "" {
		return fmt.Errorf("%w（%q）", ErrStatusNotFound, name)
	}
	wi, err := t.c.GetWorkitem(ctx, ref)
	if err != nil {
		return err
	}
	if wi.Status.ID == target.ID {
		return nil
	}
	return t.c.UpdateStatus(ctx, ref, target.ID)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

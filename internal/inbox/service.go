package inbox

import (
	"context"
	"errors"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 分页常量：与 ListTasks 的上限 200 一致。
const (
	defaultLimit = 100
	maxLimit     = 200
)

// 过滤取值。
const (
	FilterUnread = "unread"
	FilterAll    = "all"
)

// Mark 的错误：handler 据此映射状态码。
var (
	// ErrNotFound 主体不存在或不属于本人（两者刻意不分，统一 404）。
	ErrNotFound = errors.New("inbox: 事项不存在")
	// ErrStale 主体属于本人，但这条事项的停留已经结束（409，附当前事项）。
	ErrStale = errors.New("inbox: 该事项已更新")
	// ErrNotDismissible 该种类不可忽略（409）。
	ErrNotDismissible = errors.New("inbox: 该事项不可忽略")
)

// Store 是 Service 依赖的持久化面（*store.Store 实现）。
type Store interface {
	InboxTaskCandidates(ctx context.Context, userID int64) ([]store.InboxTaskRow, error)
	InboxPRDCandidates(ctx context.Context, userID int64) ([]store.InboxPRDRow, error)
	InboxMarks(ctx context.Context, userID int64) (map[string]store.InboxMark, error)
	UpsertInboxMark(ctx context.Context, userID int64, key string, subj store.InboxSubject, op store.InboxMarkOp) error
	MarkInboxRead(ctx context.Context, userID int64, items []store.InboxKeySubject) error
	InboxSubjectOwned(ctx context.Context, userID int64, subj store.InboxSubject) (bool, error)
}

// Service 组合派生与标记。无后台 goroutine、无进程内缓存：每次请求都从库里
// 派生，多节点读到的天然一致。
type Service struct {
	store  Store
	redact Redactor
}

// NewService 构造 Service。red 为 nil 时报错：收件箱会把失败原因聚合到一页，
// 没有脱敏就不该起来（启动即失败，不静默降级）。
func NewService(st Store, red Redactor) (*Service, error) {
	if st == nil {
		return nil, errors.New("inbox: Store 不能为空")
	}
	if red == nil {
		return nil, errors.New("inbox: Redactor 不能为空（收件箱展示的原因必须先脱敏）")
	}
	return &Service{store: st, redact: red}, nil
}

// Filter 是 Items 的筛选与分页参数。
type Filter struct {
	// Show 为 FilterUnread 或 FilterAll；空串按 FilterAll。
	Show string
	// Kind 非空时只返回该种类。
	Kind   Kind
	Limit  int
	Offset int
}

// Page 是 Items 的返回。Unread / Total 是不受筛选与分页影响的全量计数
// （徽标与页签用）。
type Page struct {
	Items  []Item `json:"items"`
	Unread int    `json:"unread"`
	Total  int    `json:"total"`
}

// 桩（先红后绿第一次提交）：实现见下一次提交。

// Items 返回本人的事项列表（桩）。
func (s *Service) Items(ctx context.Context, userID int64, f Filter) (*Page, error) {
	return &Page{Items: []Item{}}, nil
}

// Count 返回未读数与总数（桩）。
func (s *Service) Count(ctx context.Context, userID int64) (unread, total int, err error) {
	return 0, 0, nil
}

// StaleError 携带 409 时该主体当前的事项（可能为 nil）。
type StaleError struct {
	Current *Item
}

func (e *StaleError) Error() string { return ErrStale.Error() }

// Unwrap 让 errors.Is(err, ErrStale) 成立。
func (e *StaleError) Unwrap() error { return ErrStale }

// Mark 对本人的一条事项执行 read / unread / dismiss（桩）。
func (s *Service) Mark(ctx context.Context, userID int64, rawKey string, op store.InboxMarkOp) (*Item, error) {
	return nil, nil
}

// ReadAll 把全部未读事项标为已读（桩）。
func (s *Service) ReadAll(ctx context.Context, userID int64) (int, error) {
	return 0, nil
}

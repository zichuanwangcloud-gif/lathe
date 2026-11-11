package inbox

import (
	"context"
	"errors"
	"sort"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
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

// derive 是 Items / Count / Mark / ReadAll 共用的唯一派生路径：候选 + 标记
// 合并 → 去掉已忽略的 → 按 Since 倒序。不另写计数 SQL，免得两边口径漂移。
func (s *Service) derive(ctx context.Context, userID int64) ([]Item, error) {
	tasks, err := s.store.InboxTaskCandidates(ctx, userID)
	if err != nil {
		return nil, err
	}
	prds, err := s.store.InboxPRDCandidates(ctx, userID)
	if err != nil {
		return nil, err
	}
	marks, err := s.store.InboxMarks(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := make([]Item, 0, len(tasks)+len(prds))
	for _, r := range tasks {
		if it, ok := TaskItem(r, s.redact); ok {
			out = append(out, it)
		}
	}
	for _, r := range prds {
		// 解析失败当作空文档：一份坏快照不该拖垮整个收件箱
		doc, err := prd.ParseDocument(r.Document)
		if err != nil {
			doc = nil
		}
		if it, ok := PRDItem(r, doc, s.redact); ok {
			out = append(out, it)
		}
	}

	kept := out[:0]
	for _, it := range out {
		m, ok := marks[it.Key]
		if ok && m.DismissedAt != nil {
			continue
		}
		it.Read = ok && m.ReadAt != nil
		kept = append(kept, it)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].Since.Equal(kept[j].Since) {
			return kept[i].Since.After(kept[j].Since)
		}
		return kept[i].Key < kept[j].Key
	})
	return kept, nil
}

func counts(items []Item) (unread, total int) {
	for _, it := range items {
		if !it.Read {
			unread++
		}
	}
	return unread, len(items)
}

// Items 返回本人的事项列表。
func (s *Service) Items(ctx context.Context, userID int64, f Filter) (*Page, error) {
	all, err := s.derive(ctx, userID)
	if err != nil {
		return nil, err
	}
	unread, total := counts(all)

	sel := make([]Item, 0, len(all))
	for _, it := range all {
		if f.Show == FilterUnread && it.Read {
			continue
		}
		if f.Kind != "" && it.Kind != f.Kind {
			continue
		}
		sel = append(sel, it)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset := max(f.Offset, 0)
	if offset > len(sel) {
		offset = len(sel)
	}
	end := min(offset+limit, len(sel))
	return &Page{Items: sel[offset:end], Unread: unread, Total: total}, nil
}

// Count 返回未读数与总数（徽标轮询用），与 Items 走同一条派生路径。
func (s *Service) Count(ctx context.Context, userID int64) (unread, total int, err error) {
	all, err := s.derive(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	unread, total = counts(all)
	return unread, total, nil
}

// StaleError 携带 409 时该主体当前的事项（可能为 nil）。
type StaleError struct {
	Current *Item
}

func (e *StaleError) Error() string { return ErrStale.Error() }

// Unwrap 让 errors.Is(err, ErrStale) 成立。
func (e *StaleError) Unwrap() error { return ErrStale }

// Mark 对本人的一条事项执行 read / unread / dismiss，返回标记后的事项。
//
//  1. 键不合法 → ErrBadKey。
//  2. 重新派生本人的当前事项，按键查找。
//  3. 找不到：主体属于本人 → *StaleError（附当前事项）；否则 → ErrNotFound。
//     「不存在」与「属于别人」在这一步之前不做任何区分，也不写任何行。
//  4. dismiss 只允许 Dismissible 的事项。
//
// 已忽略的事项不在派生结果里，对它的再次操作同样落到第 3 步（409）。
func (s *Service) Mark(ctx context.Context, userID int64, rawKey string, op store.InboxMarkOp) (*Item, error) {
	switch op {
	case store.InboxMarkRead, store.InboxMarkUnread, store.InboxMarkDismiss:
	default:
		return nil, errors.New("inbox: 未知的标记操作")
	}
	key, err := ParseKey(rawKey)
	if err != nil {
		return nil, ErrBadKey
	}

	all, err := s.derive(ctx, userID)
	if err != nil {
		return nil, err
	}
	var hit *Item
	var current *Item
	for i := range all {
		if all[i].Key == key.Raw {
			hit = &all[i]
			break
		}
		if all[i].SubjectType == key.SubjectType && all[i].SubjectID == key.SubjectID {
			current = &all[i]
		}
	}

	if hit == nil {
		owned, err := s.store.InboxSubjectOwned(ctx, userID, key.Subject())
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, ErrNotFound
		}
		return nil, &StaleError{Current: current}
	}

	if op == store.InboxMarkDismiss && !hit.Dismissible {
		return nil, ErrNotDismissible
	}
	if err := s.store.UpsertInboxMark(ctx, userID, key.Raw, key.Subject(), op); err != nil {
		return nil, err
	}
	it := *hit
	switch op {
	case store.InboxMarkRead:
		it.Read = true
	case store.InboxMarkUnread:
		it.Read = false
	}
	return &it, nil
}

// ReadAll 把当前派生出的、未忽略的全部未读事项标为已读，返回标记的条数。
func (s *Service) ReadAll(ctx context.Context, userID int64) (int, error) {
	all, err := s.derive(ctx, userID)
	if err != nil {
		return 0, err
	}
	batch := make([]store.InboxKeySubject, 0, len(all))
	for _, it := range all {
		if it.Read {
			continue
		}
		subj := store.InboxSubject{TaskID: it.SubjectID}
		if it.SubjectType == SubjectPRD {
			subj = store.InboxSubject{PRDID: it.SubjectID}
		}
		batch = append(batch, store.InboxKeySubject{Key: it.Key, Subject: subj})
	}
	if err := s.store.MarkInboxRead(ctx, userID, batch); err != nil {
		return 0, err
	}
	return len(batch), nil
}

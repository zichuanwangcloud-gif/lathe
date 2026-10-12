package prd

import (
	"context"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 本文件把 *store.Store 适配成 prd.Store。
//
// 为什么需要一层适配：RepoForPRD 要返回 prd.RepoInfo（含 prd.Limits），
// 若直接让 store 提供这个方法，store 就得 import prd —— 而 prd 已经
// import store，成环。同 flow/service.go 用 json.RawMessage 绕开
// flow → runner 依赖的做法：包依赖方向只允许单向。
//
// 顺带一个好处：三个仓库阈值的「谁消费它」在这里一目了然
// （repos.prd_task_max_lines / _max_files + system_settings.flow_max_chain_length）。

// StoreAdapter 让 *store.Store 满足 Service.Store。
//
// 嵌入而非逐个转发：prds.go 上的那些方法签名本来就已经与 Service.Store
// 一致，只有 RepoForPRD 需要真的适配。
type StoreAdapter struct {
	*store.Store
}

// NewStoreAdapter 包一个 store。
func NewStoreAdapter(s *store.Store) *StoreAdapter {
	return &StoreAdapter{Store: s}
}

// RepoForPRD 取 PRD 的目标仓库与量级阈值。
//
// 链长上限来自系统设置而非仓库列：它是编排图的全局约束（07 §F3.3），
// 与「单任务多大」不是一个维度的东西。取不到时用默认值 4 而不是报错 ——
// 超链长本来就只是警告，不该因为读不到设置就卡住整份 PRD 的自检。
func (a *StoreAdapter) RepoForPRD(ctx context.Context, prdID, userID int64) (RepoInfo, error) {
	row, err := a.Store.GetPRD(ctx, prdID, userID)
	if err != nil {
		return RepoInfo{}, err
	}
	repo, err := a.Store.GetRepo(ctx, row.RepoID, userID)
	if err != nil {
		return RepoInfo{}, err
	}

	chain, err := a.Store.FlowMaxChainLength(ctx)
	if err != nil {
		chain = store.DefaultFlowMaxChainLength
	}

	return RepoInfo{
		ProviderRepo:  repo.ProviderRepo,
		DefaultBranch: repo.DefaultBranch,
		Limits: Limits{
			MaxLines: repo.PRDTaskMaxLines,
			MaxFiles: repo.PRDTaskMaxFiles,
			MaxChain: chain,
		},
	}, nil
}

// 编译期断言：适配器必须满足 Service 要的全部能力。少一个方法在这里报错，
// 而不是等装配 main.go 时才发现。
var _ Store = (*StoreAdapter)(nil)

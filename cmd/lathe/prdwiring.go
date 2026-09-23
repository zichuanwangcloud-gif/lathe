package main

import (
	"context"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
)

// 规划管线的装配（docs/10）。放在 cmd 层而不是 internal/prd 里，是为了让
// prd 包不依赖 runner —— 规划只要「在某个目录里只读地看代码」这一件能力，
// 把整个 WorktreeManager 拖进去会让 prd 的单测跟着背上 git 与文件系统。

// prdWorktrees 把 runner.WorktreeManager 收窄成 prd.Worktrees。
//
// 只转发三个方法：准备镜像、只读检出、回收。规划 agent 不建分支、不提交、
// 不推送，那些能力一个都不该露给它 —— 接口收窄本身就是「只读」的第一道闸门
// （第二道是 PermissionMode="plan"）。
type prdWorktrees struct {
	wm *runner.WorktreeManager
}

func (p prdWorktrees) EnsureMirror(ctx context.Context, providerRepo, cloneURL string) (string, error) {
	return p.wm.EnsureMirror(ctx, providerRepo, cloneURL)
}

func (p prdWorktrees) CreateDetached(ctx context.Context, providerRepo, base, name string) (*prd.Checkout, error) {
	wt, err := p.wm.CreateDetached(ctx, providerRepo, base, name)
	if err != nil {
		return nil, err
	}
	// 只带 Path 与 Mirror：Mirror 是回收时 git 的工作目录，少了它
	// worktree remove 会在 serve 的 cwd 里跑。分支名不带 —— detached
	// 检出没有分支，露出来只会引出不该有的删分支动作。
	return &prd.Checkout{Path: wt.Path, Mirror: wt.Mirror}, nil
}

func (p prdWorktrees) RemoveDetached(ctx context.Context, c *prd.Checkout) error {
	if c == nil || c.Path == "" {
		return nil
	}
	// force=true：detached 检出里不该有改动，但规划 agent 万一落了个临时
	// 文件（写个草稿、跑个 go build 留下产物），不能因此卡住回收 ——
	// D10-8 要的是「用完必删」，不是「干净才删」。
	//
	// Branch 留空是刻意的：Remove 在 force 分支里会 `branch -D wt.Branch`，
	// 而 detached 检出没有分支。空串让那条命令失败并被忽略（它本就是
	// best-effort 的 `_, _ =`），不会误删任何东西。
	return p.wm.Remove(ctx, &runner.Worktree{Path: c.Path, Mirror: c.Mirror}, true)
}

// 编译期断言：适配器必须满足规划管线要的全部能力。
var _ prd.Worktrees = prdWorktrees{}

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut 在指定目录跑 git 并返回输出。
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s 失败: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// ★安全回归：clone --mirror 后必须解除镜像推送模式。
//
// 若 remote.origin.mirror 残留为 true，一次 git push 就会把本地所有 ref
// 推上远端，包括 dev/test/main —— 与「永不推受保护分支」直接冲突。
func TestEnsureMirrorDisablesMirrorPush(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)

	mirror, err := m.EnsureMirror(context.Background(), "acme/demo", src)
	if err != nil {
		t.Fatalf("EnsureMirror 失败: %v", err)
	}

	out, _ := exec.Command("git", "-C", mirror, "config", "--get", "remote.origin.mirror").Output()
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("remote.origin.mirror 应已被解除，实际为 %q —— 会导致镜像推送覆盖受保护分支", got)
	}

	fetch := gitOut(t, mirror, "config", "--get", "remote.origin.fetch")
	if fetch != "+refs/heads/*:refs/heads/*" {
		t.Errorf("fetch refspec = %q，期望只取分支", fetch)
	}
}

func TestPushTaskBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix,
		IssueKey: "CR-1", Title: "push me",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 造一个提交，否则没东西可推
	if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, wt.Path, "add", ".")
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "改动")

	if err := m.Push(ctx, wt, repo); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}

	// 源仓库里应出现该任务分支
	branches := gitOut(t, src, "branch", "--list", wt.Branch)
	if !strings.Contains(branches, wt.Branch) {
		t.Errorf("源仓库应出现任务分支 %s，实际分支列表: %q", wt.Branch, branches)
	}
}

// 推送前必须拦住受保护分支。
func TestPushRejectsProtectedBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "t",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	for _, bad := range []string{"dev", "test", "main"} {
		tampered := *wt
		tampered.Branch = bad // 模拟配置错误或调用方传错

		err := m.Push(ctx, &tampered, repo)
		if err == nil {
			t.Errorf("推送到 %q 应被拒绝", bad)
			continue
		}
		var pe ErrProtectedBranch
		if !errors.As(err, &pe) {
			t.Errorf("推送 %q 的错误类型应为 ErrProtectedBranch，得到 %T: %v", bad, err, err)
		}
	}

	if err := m.Push(ctx, nil, repo); err == nil {
		t.Error("传 nil 工作区应报错")
	}
}

// 一次 push 只能动任务分支自己，绝不能顺带推其他 ref。
func TestPushOnlyTouchesTaskBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "isolated",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 在 mirror 里伪造一条本地 dev 提交：若是镜像推送，它会被推到源仓库
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "任务提交")
	devBefore := gitOut(t, src, "rev-parse", "dev")

	// 直接在 mirror 上把 dev 向前推进（模拟本地 dev 与远端分叉）
	gitOut(t, wt.Mirror, "update-ref", "refs/heads/dev", gitOut(t, wt.Path, "rev-parse", "HEAD"))

	if err := m.Push(ctx, wt, repo); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}

	devAfter := gitOut(t, src, "rev-parse", "dev")
	if devBefore != devAfter {
		t.Errorf("源仓库的 dev 被改动了（%s → %s）—— 推送泄漏到了受保护分支", devBefore[:8], devAfter[:8])
	}
}

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if fetch != mirrorFetchRefspec {
		t.Errorf("fetch refspec = %q，期望 %q（远端分支必须取到独立命名空间，否则 prune 会剪掉在途任务分支）", fetch, mirrorFetchRefspec)
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

	if err := m.Push(ctx, wt, repo, nil); err != nil {
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

		err := m.Push(ctx, &tampered, repo, nil)
		if err == nil {
			t.Errorf("推送到 %q 应被拒绝", bad)
			continue
		}
		var pe ErrProtectedBranch
		if !errors.As(err, &pe) {
			t.Errorf("推送 %q 的错误类型应为 ErrProtectedBranch，得到 %T: %v", bad, err, err)
		}
	}

	if err := m.Push(ctx, nil, repo, nil); err == nil {
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

	if err := m.Push(ctx, wt, repo, nil); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}

	devAfter := gitOut(t, src, "rev-parse", "dev")
	if devBefore != devAfter {
		t.Errorf("源仓库的 dev 被改动了（%s → %s）—— 推送泄漏到了受保护分支", devBefore[:8], devAfter[:8])
	}
}

// 推送错误分类：明确永久的立即失败，其余（含一切网络抖动）都应重试。
func TestPushErrClassification(t *testing.T) {
	// 可重试（非永久）—— 网络断连类。第一条是任务 #1551 的真实报错：
	// kex 被掐时 git 会附带 "access rights" 兜底提示，若把它判成永久，
	// 网络抖动就永远一次判死。
	transient := []string{
		"git push --set-upstream origin refs/heads/fix/x:refs/heads/fix/x: exit status 128（kex_exchange_identification: Connection closed by remote host\r\nConnection closed by 140.82.113.36 port 443\r\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.）",
		"git push: exit status 128（ssh: connect to host github.com port 22: Connection timed out）",
		"git push: exit status 128（fatal: unable to access 'https://github.com/x/y/': Could not resolve hostname: github.com）",
		"git push: exit status 128（fatal: unable to access 'https://github.com/x/y/': The requested URL returned error: 502）",
		"git push: exit status 128（error: RPC failed; HTTP 503 curl 22 The requested URL returned error: 503）",
		"git push: exit status 128（fatal: The remote end hung up unexpectedly）",
		"git push: exit status 128（ssh_exchange_identification: read: Connection reset by peer）",
		"git push: signal: killed", // 超时/被杀：原因未知，按可重试处理
	}
	for _, msg := range transient {
		if isPermanentPushErr(errors.New(msg)) {
			t.Errorf("应判为可重试（非永久）: %s", msg)
		}
	}

	// 永久 —— 重试同样的参数只会得到同样的拒绝，必须立即失败。
	permanent := []string{
		"git push: exit status 128（! [rejected]  fix/x -> fix/x (non-fast-forward)\nerror: failed to push some refs to 'github.com:x/y'\nhint: Updates were rejected because the tip of your current branch is behind\nhint: its remote counterpart. Integrate the remote changes (e.g.\nhint: 'git pull ...') before pushing again.）",
		"git push: exit status 128（git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository.）",
		"git push: exit status 128（remote: Permission to x/y.git denied to bot.\nfatal: unable to access 'https://github.com/x/y/': The requested URL returned error: 403）",
		"git push: exit status 128（ERROR: Repository not found.\nfatal: Could not read from remote repository.）",
		"git push: exit status 128（remote: error: GH006: Protected branch update failed for refs/heads/dev.\n! [remote rejected] dev (protected branch hook declined)）",
		"git push: exit status 128（remote: error: GH013: Repository rule violations found for refs/heads/fix/x.\nremote: - GITHUB PUSH PROTECTION: Push cannot contain secrets）",
		"git push: exit status 128（fatal: 'https://github.com/x/y' does not appear to be a git repository）",
		"git push: exit status 128（fatal: Authentication failed for 'https://github.com/x/y/'）",
	}
	for _, msg := range permanent {
		if !isPermanentPushErr(errors.New(msg)) {
			t.Errorf("应判为永久（立即失败）: %s", msg)
		}
	}
}

// 重试循环：抖动两次后成功 → 3 次尝试，通知依次为 重试/重试/成功。
func TestPushWithRetryRecoversFromFlake(t *testing.T) {
	backoff := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	flake := errors.New("exit status 128（kex_exchange_identification: Connection closed by remote host）")

	calls := 0
	var notes []PushProgress
	err := pushWithRetry(context.Background(), "fix/x", func() error {
		calls++
		if calls < 3 {
			return flake
		}
		return nil
	}, backoff, func(p PushProgress) { notes = append(notes, p) })

	if err != nil {
		t.Fatalf("抖动能恢复时不应报错: %v", err)
	}
	if calls != 3 {
		t.Errorf("应尝试 3 次，实际 %d", calls)
	}
	if len(notes) != 3 {
		t.Fatalf("应通知 3 次（2 重试 + 1 成功），实际 %d", len(notes))
	}
	for i, n := range notes[:2] {
		if n.Err == nil || n.Wait <= 0 || n.Attempt != i+1 {
			t.Errorf("第 %d 条重试通知形态不对: %+v", i+1, n)
		}
	}
	if last := notes[2]; last.Err != nil || last.Wait != 0 || last.Attempt != 3 {
		t.Errorf("成功通知形态不对: %+v", last)
	}
}

// 首次即成功：零通知 —— 顺利的推送不该在事件流里制造噪音。
func TestPushWithRetryCleanSuccessSilent(t *testing.T) {
	notified := false
	err := pushWithRetry(context.Background(), "fix/x", func() error { return nil },
		[]time.Duration{time.Millisecond}, func(PushProgress) { notified = true })
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if notified {
		t.Error("首次即成功不应产生通知")
	}
}

// 永久错误：一次即败，零通知（没有重试就没有可展示的重试过程）。
func TestPushWithRetryPermanentFailsFast(t *testing.T) {
	calls := 0
	notified := false
	err := pushWithRetry(context.Background(), "fix/x", func() error {
		calls++
		return errors.New("exit status 128（! [rejected] fix/x (non-fast-forward)）")
	}, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		func(PushProgress) { notified = true })

	if err == nil {
		t.Fatal("永久错误应返回错误")
	}
	if calls != 1 {
		t.Errorf("永久错误应只尝试 1 次，实际 %d", calls)
	}
	if notified {
		t.Error("永久错误不重试，不应产生通知")
	}
}

// 抖动不止：尝试次数 = len(backoff)+1，每次都通知，最终报错。
func TestPushWithRetryExhaustsBackoff(t *testing.T) {
	calls := 0
	var notes []PushProgress
	err := pushWithRetry(context.Background(), "fix/x", func() error {
		calls++
		return errors.New("exit status 128（Connection reset by peer）")
	}, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		func(p PushProgress) { notes = append(notes, p) })

	if err == nil {
		t.Fatal("重试耗尽后应返回错误")
	}
	if calls != 4 {
		t.Errorf("应尝试 4 次（1+3 次退避），实际 %d", calls)
	}
	if len(notes) != 3 {
		t.Errorf("应通知 3 次重试，实际 %d", len(notes))
	}
}

// 退避期间 ctx 被取消（关机/超时）：立即返回，不傻等。
func TestPushWithRetryAbortsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- pushWithRetry(ctx, "fix/x", func() error {
			return errors.New("exit status 128（Connection reset by peer）")
		}, []time.Duration{time.Hour}, func(PushProgress) { cancel() })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("取消后应返回原始推送错误")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后仍在退避等待 —— 关机会被拖住")
	}
}

// non-fast-forward（远端分叉）是永久错误：必须失败，且走快速通道 ——
// 不退避重试，否则每次重试都是同样的拒绝，白白拖延反馈。
func TestPushNonFastForwardRejected(t *testing.T) {
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
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "第一版")
	if err := m.Push(ctx, wt, repo, nil); err != nil {
		t.Fatalf("首次 Push 失败: %v", err)
	}

	// 改写本地历史，制造 non-fast-forward
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"commit", "-q", "--amend", "--allow-empty", "-m", "改写历史")

	err = m.Push(ctx, wt, repo, nil)
	if err == nil {
		t.Fatal("改写历史后的推送应被拒绝（non-fast-forward）")
	}
	if !isPermanentPushErr(err) {
		t.Errorf("non-fast-forward 应判为永久错误（立即失败不重试），错误: %v", err)
	}
}

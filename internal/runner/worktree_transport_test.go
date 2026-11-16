package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// gitRecorder 在 PATH 最前面放一个记录 argv 与 GIT_DIR/GIT_CONFIG_* 的包装 git。
//
// 只能看到 Lathe【直接】发起的调用：git 派生内部子命令（gc --auto、maintenance）
// 时会把 exec-path 放到 PATH 最前，不经过这个包装器。所以本文件不用它断言
// 「没有 gc 子进程」（那个断言恒真，证明不了任何事），见 transportBeforeRemove。
type gitRecorder struct {
	log string
}

func newGitRecorder(t *testing.T) *gitRecorder {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("找不到 git")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
{
  printf 'CALL'; for a in "$@"; do printf ' %%s' "$a"; done; printf '\n'
  printf 'ENV GIT_DIR=%%s\n' "$GIT_DIR"
  i=0; n="${GIT_CONFIG_COUNT:-0}"
  while [ "$i" -lt "$n" ]; do eval "printf 'CFG %%s=%%s\n' \"\$GIT_CONFIG_KEY_$i\" \"\$GIT_CONFIG_VALUE_$i\""; i=$((i+1)); done
} >> %s
exec %s "$@"
`, log, real)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &gitRecorder{log: log}
}

type gitCall struct {
	args   string
	gitDir string
	cfg    []string // "key=value"，按 GIT_CONFIG_KEY_n 的下标顺序
}

func (r *gitRecorder) calls(t *testing.T) []gitCall {
	t.Helper()
	b, err := os.ReadFile(r.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []gitCall
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "CALL"):
			out = append(out, gitCall{args: strings.TrimSpace(strings.TrimPrefix(line, "CALL"))})
		case strings.HasPrefix(line, "ENV GIT_DIR=") && len(out) > 0:
			out[len(out)-1].gitDir = strings.TrimPrefix(line, "ENV GIT_DIR=")
		case strings.HasPrefix(line, "CFG ") && len(out) > 0:
			out[len(out)-1].cfg = append(out[len(out)-1].cfg, strings.TrimPrefix(line, "CFG "))
		}
	}
	return out
}

func (r *gitRecorder) reset(t *testing.T) {
	t.Helper()
	_ = os.Remove(r.log)
}

// ---------------------------------------------------------------- AC-5c

// TestZeroAuthKeepsLegacyGitArgv：零值 GitAuth（GitHub）走原路径，git 子命令
// 序列与 cff5332 逐行相同，且从不进 transport。
func TestZeroAuthKeepsLegacyGitArgv(t *testing.T) {
	ctx := context.Background()
	src := sourceRepo(t)
	m := newManager(t)
	repo := DefaultRepoConfig("acme/demo")
	rec := newGitRecorder(t)

	mirror, err := m.EnsureMirror(ctx, repo.RepoKey(), src, vcs.GitAuth{})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-5", Title: "argv", TaskID: 5})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, wt.Path, "x.txt", "x\n")
	rec.reset(t)

	refspec := "refs/heads/" + wt.Branch + ":refs/heads/" + wt.Branch
	steps := []struct {
		name string
		run  func() error
		want []string
	}{
		{"EnsureMirror 刷新", func() error { _, err := m.EnsureMirror(ctx, repo.RepoKey(), src, vcs.GitAuth{}); return err },
			[]string{"config --replace-all remote.origin.fetch " + mirrorFetchRefspec, "fetch --prune --quiet origin"}},
		{"Push", func() error { return m.Push(ctx, wt, repo, vcs.GitAuth{}, nil) },
			[]string{"push --set-upstream origin " + refspec}},
		{"RebaseOnto", func() error {
			base := gitOut(t, "", "--git-dir="+mirror, "rev-parse", MirrorBaseRef("dev"))
			rec.reset(t)
			return m.RebaseOnto(ctx, wt, base, "dev", src, vcs.GitAuth{})
		}, []string{"fetch --prune --quiet origin", "rebase --onto refs/remotes/origin/dev"}},
		{"ForcePush", func() error { return m.ForcePush(ctx, wt, repo, vcs.GitAuth{}, nil) },
			[]string{"push --force-with-lease --set-upstream origin " + refspec}},
		{"Reattach", func() error { return m.Reattach(ctx, repo.RepoKey(), wt.Path, wt.Branch, src, vcs.GitAuth{}) },
			[]string{"fetch --prune --quiet origin", "rev-parse --verify --quiet refs/remotes/origin/" + wt.Branch + "^{commit}",
				"worktree remove --force " + wt.Path, "worktree prune", "branch -f " + wt.Branch + " refs/remotes/origin/" + wt.Branch,
				"worktree add --quiet " + wt.Path + " " + wt.Branch}},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			rec.reset(t)
			if err := st.run(); err != nil {
				t.Fatalf("%s 失败: %v", st.name, err)
			}
			calls := rec.calls(t)
			var got []string
			for _, c := range calls {
				if strings.Contains(c.gitDir, "lathe-git-transport-") {
					t.Errorf("零值 auth 不应进 transport：%s（GIT_DIR=%s）", c.args, c.gitDir)
				}
				got = append(got, c.args)
			}
			if len(got) != len(st.want) {
				t.Fatalf("git 调用序列 =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(st.want, "\n  "))
			}
			for i := range got {
				if !strings.HasPrefix(got[i], st.want[i]) {
					t.Errorf("第 %d 条 = %q，want 以 %q 开头", i, got[i], st.want[i])
				}
			}
		})
	}

	// 首次建 mirror 的序列（另起一个 manager）
	t.Run("EnsureMirror 首次", func(t *testing.T) {
		m2 := newManager(t)
		rec.reset(t)
		if _, err := m2.EnsureMirror(ctx, "acme/demo", src, vcs.GitAuth{}); err != nil {
			t.Fatal(err)
		}
		want := []string{"clone --mirror --quiet " + src + " ", "config --unset-all remote.origin.mirror",
			"config --replace-all remote.origin.fetch " + mirrorFetchRefspec, "fetch --prune --quiet origin"}
		calls := rec.calls(t)
		if len(calls) != len(want) {
			t.Fatalf("调用数 = %d（%v），want %d", len(calls), calls, len(want))
		}
		for i, c := range calls {
			if !strings.HasPrefix(c.args, want[i]) {
				t.Errorf("第 %d 条 = %q，want 以 %q 开头", i, c.args, want[i])
			}
		}
	})
}

// ---------------------------------------------------------------- AC-18

// TestProtectedBranchGateAllProviders：三个平台走同一道闸门，且闸门先于任何 git 调用。
func TestProtectedBranchGateAllProviders(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	rec := newGitRecorder(t)
	tok := vcs.NewGitAuth("oauth2", "tok-gate")
	for _, p := range []vcs.Provider{vcs.ProviderGitHub, vcs.ProviderGitLab, vcs.ProviderGitea} {
		repo := DefaultRepoConfig("acme/demo")
		auth := vcs.GitAuth{}
		if p != vcs.ProviderGitHub {
			repo.VCSProvider, repo.VCSBaseURL, auth = p, "https://"+string(p)+".example.com", tok
		}
		for _, branch := range []string{"dev", "test", "main"} {
			t.Run(string(p)+"/"+branch, func(t *testing.T) {
				wt := &Worktree{Path: t.TempDir(), Branch: branch, BaseBranch: "dev", Mirror: filepath.Join(m.Root(), ".mirrors", "x.git")}
				rec.reset(t)
				for name, push := range map[string]func() error{
					"Push":      func() error { return m.Push(ctx, wt, repo, auth, nil) },
					"ForcePush": func() error { return m.ForcePush(ctx, wt, repo, auth, nil) },
				} {
					var pe ErrProtectedBranch
					if err := push(); !errors.As(err, &pe) {
						t.Errorf("%s(%s) err = %v，want ErrProtectedBranch", name, branch, err)
					}
				}
				if n := len(rec.calls(t)); n != 0 {
					t.Errorf("受保护分支闸门之前不应有任何 git 调用，得到 %d 次", n)
				}
			})
		}
	}
}

// ---------------------------------------------------------------- AC-5d

// TestTransportFetchKeepsMirrorOnlyObjects：transport 里不跑 gc，刷新不删 mirror/worktree 独有对象。
func TestTransportFetchKeepsMirrorOnlyObjects(t *testing.T) {
	ctx := context.Background()
	tok := randToken(t, "tok-")
	stub := newGitHTTPStub(t, "", false, tok)
	const repoPath = "g/keep"
	src := stub.seedRepo(t, repoPath)
	m := newManager(t)
	repo := authRepo(stub, repoPath)
	auth := vcs.NewGitAuth("oauth2", tok)

	wt, err := m.Create(ctx, CreateParams{Repo: repo, CloneURL: repo.CloneURL(), Kind: KindFix, IssueKey: "CR-6", Title: "keep", TaskID: 6, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	// 只被 mirror 的任务分支与 worktree HEAD 引用的提交（含新 blob），从未推送。
	taskCommit := commitIn(t, wt.Path, "only-local.txt", "未推送的在途改动 "+randToken(t, "")+"\n")
	blob := gitOut(t, wt.Path, "rev-parse", "HEAD:only-local.txt")
	agePastPrune(t, wt.Mirror)

	// transport 语境的加速器（测试注入口，固定键必须压过它）。
	transportExtraConfig = [][2]string{{"fetch.unpackLimit", "1"}, {"gc.autoPackLimit", "1"}, {"gc.autoDetach", "false"}}
	t.Cleanup(func() { transportExtraConfig = nil })
	var gcTraces []string
	transportBeforeRemove = func(dir string) {
		for _, f := range []string{"packed-refs", "gc.pid"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				gcTraces = append(gcTraces, f)
			}
		}
	}
	t.Cleanup(func() { transportBeforeRemove = nil })
	// mirror 语境的投毒：只作用于 importRefs 之后那次 mirror 语境的 gc。
	for _, kv := range [][2]string{{"gc.autoPackLimit", "1"}, {"gc.auto", "1"}, {"gc.pruneExpire", "now"}, {"gc.autoDetach", "false"}} {
		gitOut(t, "", "--git-dir="+wt.Mirror, "config", kv[0], kv[1])
	}
	rec := newGitRecorder(t)

	check := func(step string) {
		t.Helper()
		if out, err := gitRun("", "--git-dir="+wt.Mirror, "cat-file", "-t", taskCommit); err != nil || out != "commit" {
			t.Fatalf("%s 后任务提交丢了：%q %v", step, out, err)
		}
		if out, err := gitRun("", "--git-dir="+wt.Mirror, "cat-file", "-t", blob); err != nil || out != "blob" {
			t.Fatalf("%s 后任务 blob 丢了：%q %v", step, out, err)
		}
		if out, err := gitRun(wt.Path, "log", "-1", "--format=%H"); err != nil || out != taskCommit {
			t.Fatalf("%s 后 worktree HEAD 不可用：%q %v", step, out, err)
		}
	}
	for i := 1; i <= 3; i++ {
		stub.upstreamCommit(t, src, repoPath, "dev", fmt.Sprintf("u%d.txt", i), fmt.Sprintf("u%d\n", i))
		if _, err := m.EnsureMirror(ctx, repo.RepoKey(), repo.CloneURL(), auth); err != nil {
			t.Fatalf("第 %d 次刷新失败: %v", i, err)
		}
		check(fmt.Sprintf("第 %d 次刷新", i))
		agePastPrune(t, wt.Mirror)
	}
	oldBase := gitOut(t, "", "--git-dir="+wt.Mirror, "rev-parse", MirrorBaseRef("dev"))
	stub.upstreamCommit(t, src, repoPath, "dev", "u4.txt", "u4\n")
	// RebaseOnto 会改写任务分支：之后检查的是改写后的提交，原提交仍应在（reflog/worktree）。
	if err := m.RebaseOnto(ctx, wt, oldBase, "dev", repo.CloneURL(), auth); err != nil {
		t.Fatalf("RebaseOnto: %v", err)
	}
	taskCommit = gitOut(t, wt.Path, "rev-parse", "HEAD")
	check("RebaseOnto")
	if err := m.Push(ctx, wt, repo, auth, nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := m.Reattach(ctx, repo.RepoKey(), wt.Path, wt.Branch, repo.CloneURL(), auth); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	check("Reattach")

	if out, err := gitRun("", "--git-dir="+wt.Mirror, "fsck", "--connectivity-only"); err != nil ||
		strings.Contains(out, "missing") || strings.Contains(out, "broken") {
		t.Errorf("fsck --connectivity-only 有异常：%v\n%s", err, out)
	}
	if len(gcTraces) != 0 {
		t.Errorf("transport 语境下跑过 gc（目录里出现 %v）", gcTraces)
	}
	fetches := 0
	for _, c := range rec.calls(t) {
		if !strings.Contains(c.gitDir, "lathe-git-transport-") || !strings.HasPrefix(c.args, "fetch") {
			continue
		}
		fetches++
		if !strings.Contains(c.args, "--no-auto-maintenance") {
			t.Errorf("transport 的 fetch 缺 --no-auto-maintenance：%s", c.args)
		}
		last := map[string]int{}
		for i, kv := range c.cfg {
			k, _, _ := strings.Cut(kv, "=")
			last[k] = i
		}
		for _, kv := range []string{"gc.auto=0", "maintenance.auto=false"} {
			k, _, _ := strings.Cut(kv, "=")
			idx, ok := last[k]
			if !ok || c.cfg[idx] != kv {
				t.Errorf("transport fetch 的配置里缺 %s：%v", kv, c.cfg)
				continue
			}
			for _, extra := range transportExtraKeysForTest() {
				if j, ok := last[extra]; ok && j > idx {
					t.Errorf("固定键 %s 应排在注入口键 %s 之后：%v", kv, extra, c.cfg)
				}
			}
		}
	}
	if fetches == 0 {
		t.Error("记录器没有看到任何 transport 里的 fetch")
	}
}

func transportExtraKeysForTest() []string {
	return []string{"fetch.unpackLimit", "gc.autoPackLimit", "gc.autoDetach"}
}

// agePastPrune 把 mirror 对象库下全部松散对象的 mtime 调到 30 天前（老过默认 gc.pruneExpire）。
func agePastPrune(t *testing.T, mirror string) {
	t.Helper()
	old := time.Now().Add(-30 * 24 * time.Hour)
	root := filepath.Join(mirror, "objects")
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if len(strings.Split(rel, string(filepath.Separator))[0]) == 2 { // xx/ 下的松散对象
			_ = os.Chtimes(path, old, old)
		}
		return nil
	})
}

// TestTransportFetchIsIncremental：种了 have 之后刷新是增量的。
func TestTransportFetchIsIncremental(t *testing.T) {
	ctx := context.Background()
	tok := randToken(t, "tok-")
	stub := newGitHTTPStub(t, "", false, tok)
	const repoPath = "g/big"
	src := stub.seedRepo(t, repoPath)
	// 约 2MB、≥200 个文件：对象数超过 fetch.unpackLimit，首次拉取落成 pack。
	gitOut(t, src, "checkout", "-q", "dev")
	for i := 0; i < 220; i++ {
		buf := []byte(strings.Repeat(randToken(t, strconv.Itoa(i)+"-"), 600))
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%03d.txt", i)), buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, src, "add", ".")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "big")
	gitOut(t, src, "push", "--quiet", stub.bare(repoPath), "dev")

	m := newManager(t)
	repo := authRepo(stub, repoPath)
	auth := vcs.NewGitAuth("oauth2", tok)
	mirror, err := m.EnsureMirror(ctx, repo.RepoKey(), repo.CloneURL(), auth)
	if err != nil {
		t.Fatal(err)
	}
	sizes := stub.uploadPackSizes()
	if len(sizes) == 0 {
		t.Fatal("桩没记录到 upload-pack 响应")
	}
	first := sizes[len(sizes)-1]

	for i := 1; i <= 3; i++ {
		before := objectCount(t, mirror)
		stub.upstreamCommit(t, src, repoPath, "dev", fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("small change %d\n", i))
		if _, err := m.EnsureMirror(ctx, repo.RepoKey(), repo.CloneURL(), auth); err != nil {
			t.Fatal(err)
		}
		after := objectCount(t, mirror)
		if d := after - before; d > 10 {
			t.Errorf("第 %d 次刷新新增对象条目 %d，want ≤ 10（上游只新增 3 个）", i, d)
		}
		sz := stub.uploadPackSizes()
		if last := sz[len(sz)-1]; last*10 >= first {
			t.Errorf("第 %d 次刷新 upload-pack 响应 %d 字节，want < 首次 %d 的 10%%", i, last, first)
		}
	}
}

// objectCount 是 count-objects -v 的 count + in-pack。
func objectCount(t *testing.T, gitDir string) int {
	t.Helper()
	out := gitOut(t, "", "--git-dir="+gitDir, "count-objects", "-v")
	n := 0
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, ": ")
		if k == "count" || k == "in-pack" {
			x, _ := strconv.Atoi(strings.TrimSpace(v))
			n += x
		}
	}
	return n
}

// TestTransportSeedHavesDoesNotBreakPrune：种在 refs/lathe-have/* 的旧值不影响 prune。
func TestTransportSeedHavesDoesNotBreakPrune(t *testing.T) {
	ctx := context.Background()
	tok := randToken(t, "tok-")
	stub := newGitHTTPStub(t, "", false, tok)
	const repoPath = "g/prune"
	stub.seedRepo(t, repoPath)
	m := newManager(t)
	repo := authRepo(stub, repoPath)
	auth := vcs.NewGitAuth("oauth2", tok)
	mirror, err := m.EnsureMirror(ctx, repo.RepoKey(), repo.CloneURL(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mirrorRemotes(t, mirror)["tmp"]; !ok {
		t.Fatal("首次刷新后 mirror 应有 origin/tmp")
	}
	gitOut(t, "", "--git-dir="+stub.bare(repoPath), "update-ref", "-d", "refs/heads/tmp")
	if _, err := m.EnsureMirror(ctx, repo.RepoKey(), repo.CloneURL(), auth); err != nil {
		t.Fatal(err)
	}
	if _, ok := mirrorRemotes(t, mirror)["tmp"]; ok {
		t.Error("上游删掉 tmp 后刷新，mirror 的 origin/tmp 应被 prune")
	}
	if extra := refMap(t, mirror, "refs/lathe-have/", ""); len(extra) != 0 {
		t.Errorf("mirror 里不应出现 refs/lathe-have/*：%v", extra)
	}
}

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
)

// 落盘的必须是【完整】输出，而不是进数据库那份 16KB 截断版（T4-AC2）。
//
// 这条是本项的要害：光在 INSERT 里补一列，存进去的仍然是截断过的东西，
// 排障时照样看不到真相。
func TestFileStepLoggerWritesFullOutput(t *testing.T) {
	dir := t.TempDir()
	l := &fileStepLogger{dataDir: dir, rel: filepath.Join(verifyLogRoot, "task-1", "round-0")}

	// 造一份远超 maxStepOutput（16KB）的输出
	big := strings.Repeat("x", maxStepOutput*3)
	ref, err := l.WriteStepLog("build", []byte(big))
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if ref == "" {
		t.Fatal("应返回非空 log_ref")
	}
	// ref 必须是相对路径 —— DataDir 可配，绝对路径写进库会让部署目录一变全失效
	if filepath.IsAbs(ref) {
		t.Errorf("log_ref 应是相对 DataDir 的路径，得到绝对路径 %q", ref)
	}

	got, err := os.ReadFile(filepath.Join(dir, ref))
	if err != nil {
		t.Fatalf("AC2：log_ref 指向的文件应真实存在: %v", err)
	}
	if len(got) != len(big) {
		t.Errorf("AC2：落盘应是完整输出，写入 %d 字节，读回 %d 字节（被截断了？）", len(big), len(got))
	}
}

// 同一轮里重名步骤不互相覆盖（修复回路里同名步骤会反复出现，
// 一轮内也可能有多条同名的复现测试）。
func TestFileStepLoggerDoesNotOverwriteSameStepName(t *testing.T) {
	dir := t.TempDir()
	l := &fileStepLogger{dataDir: dir, rel: "logs"}

	r1, err := l.WriteStepLog("repro_fail", []byte("第一条"))
	if err != nil {
		t.Fatalf("第一次落盘失败: %v", err)
	}
	r2, err := l.WriteStepLog("repro_fail", []byte("第二条"))
	if err != nil {
		t.Fatalf("第二次落盘失败: %v", err)
	}
	if r1 == r2 {
		t.Fatalf("同名步骤的两次落盘不该是同一个文件，都是 %q", r1)
	}
	for ref, want := range map[string]string{r1: "第一条", r2: "第二条"} {
		got, err := os.ReadFile(filepath.Join(dir, ref))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", ref, err)
		}
		if string(got) != want {
			t.Errorf("%s 内容 = %q，期望 %q（被覆盖了？）", ref, got, want)
		}
	}
}

// 不同轮次落在不同目录（AC3）。
//
// 同任务多轮互相覆盖的话，「第一轮为什么挂」这个问题在第二轮跑完之后
// 就永远回答不了了 —— 而那恰恰是修复回路最需要回答的问题。
func TestStepLoggerSeparatesRounds(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{LogDir: dir}

	l0, l1 := p.stepLogger(42, 0), p.stepLogger(42, 1)
	if l0 == nil || l1 == nil {
		t.Fatal("配了 LogDir 就该给出 logger")
	}
	r0, err := l0.WriteStepLog("build", []byte("首轮"))
	if err != nil {
		t.Fatalf("首轮落盘失败: %v", err)
	}
	r1, err := l1.WriteStepLog("build", []byte("修复轮"))
	if err != nil {
		t.Fatalf("修复轮落盘失败: %v", err)
	}
	if filepath.Dir(r0) == filepath.Dir(r1) {
		t.Errorf("不同轮次应落在不同目录，都在 %s", filepath.Dir(r0))
	}
	if !strings.Contains(r0, "task-42") {
		t.Errorf("路径里应含 task id，得到 %q", r0)
	}
	// 两份内容都还在
	for ref, want := range map[string]string{r0: "首轮", r1: "修复轮"} {
		got, err := os.ReadFile(filepath.Join(dir, ref))
		if err != nil || string(got) != want {
			t.Errorf("%s 内容 = %q（err=%v），期望 %q", ref, got, err, want)
		}
	}
}

// LogDir 未配置时整个机制静默关闭：logger 为 nil，log_ref 留空，
// 行为与本项之前完全一致。
func TestStepLoggerDisabledWithoutLogDir(t *testing.T) {
	p := &Pipeline{}
	if l := p.stepLogger(1, 0); l != nil {
		t.Errorf("未配 LogDir 时不该给出 logger，得到 %#v", l)
	}
	// writeStepLog 对 nil logger 必须安全
	if ref := writeStepLog(nil, "build", []byte("x")); ref != "" {
		t.Errorf("nil logger 应返回空 ref，得到 %q", ref)
	}
}

// 落盘失败只降级为空 log_ref，绝不让验证失败（AC6）。
//
// 磁盘满、权限不对都不该把一次本来能通过的验证判死。
type failingLogger struct{ calls int }

func (f *failingLogger) WriteStepLog(step string, output []byte) (string, error) {
	f.calls++
	return "", errors.New("磁盘满")
}

func TestWriteStepLogSwallowsFailure(t *testing.T) {
	fl := &failingLogger{}
	ref := writeStepLog(fl, "build", []byte("x"))
	if ref != "" {
		t.Errorf("落盘失败应返回空 ref，得到 %q", ref)
	}
	if fl.calls != 1 {
		t.Errorf("应尝试一次，实际 %d 次", fl.calls)
	}
}

// 步骤名里的斜杠与空格不能把日志写到意料之外的位置。
//
// 复现测试的步骤名来自测试文件路径，直接拼进路径就是一个目录穿越。
func TestSanitizeLogNameNeverEscapesDirectory(t *testing.T) {
	for _, in := range []string{
		"../../etc/passwd", "a/b/c", "with space", "", "...", "/", "重现测试",
	} {
		got := sanitizeLogName(in)
		if got == "" {
			t.Errorf("输入 %q 规整出空名字", in)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("输入 %q 规整后仍含路径分隔符: %q", in, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("输入 %q 规整后仍含 ..: %q", in, got)
		}
	}
}

// 端到端：真跑一遍流水线，每一步落库的 log_ref 都必须非空，
// 且指向的文件真实存在、含真实输出（AC1/AC2/AC4）。
//
// AC4 特别重要：**通过的步骤也要留日志**。此前只有失败步骤的前 4KB 会进
// agent_events，而排障时最想看的往往正是「上一次通过时是什么样」。
func TestPipelineWritesLogRefForEveryVerifyStep(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	logDir := t.TempDir()

	verifs := &fakeVerifications{}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, gh, manualGateAgent(), &fakeNotifier{})
	p.Verifications = verifs
	p.LogDir = logDir
	p.SettingSources = "project"

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	if len(verifs.rows) == 0 {
		t.Fatal("应有验证步骤落库")
	}
	if len(verifs.logRefs) != len(verifs.rows) {
		t.Fatalf("log_ref 与步骤数不齐：%d vs %d", len(verifs.logRefs), len(verifs.rows))
	}

	passedWithLog := 0
	for i, row := range verifs.rows {
		ref := verifs.logRefs[i]
		if ref == "" {
			t.Errorf("AC1：步骤 %s 的 log_ref 为空", row)
			continue
		}
		// 复现阶段的 ref 是空格分隔的多个路径（每条复现测试各一份完整日志）
		for _, one := range strings.Fields(ref) {
			if _, err := os.Stat(filepath.Join(logDir, one)); err != nil {
				t.Errorf("AC2：步骤 %s 的 log_ref %q 指向的文件不存在: %v", row, one, err)
			}
		}
		if strings.HasSuffix(row, "/passed") {
			passedWithLog++
		}
	}
	if passedWithLog == 0 {
		t.Error("AC4：通过的步骤也必须留日志，实际一条都没有")
	}

	// AC5：日志不在 worktree 里 —— worktree 会被回收，
	// 日志的全部价值就在于「现场没了之后还能查」
	for _, ref := range verifs.logRefs {
		for _, one := range strings.Fields(ref) {
			if strings.Contains(one, "workspaces") {
				t.Errorf("AC5：日志不该落在 worktree 里，得到 %q", one)
			}
		}
	}
}

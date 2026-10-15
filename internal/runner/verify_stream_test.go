package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bigOutputStep 造一条打印 sizeMB 兆输出的步骤：开头一行 HEAD-MARK，
// 中间是填充，最后一行是结论 THE-CONCLUSION（模拟 go test 的 FAIL 汇总
// 在尾部）。exitCode 非零模拟失败。
func bigOutputStep(sizeMB int, exitCode int) Step {
	script := "echo HEAD-MARK; " +
		"head -c " + strconv.Itoa(sizeMB<<20) + " /dev/zero | tr '\\0' 'x'; echo; " +
		"echo THE-CONCLUSION; exit " + strconv.Itoa(exitCode)
	return Step{Name: StepBuild, Cmd: []string{"sh", "-c", script}}
}

// 进摘要（事件流、失败记忆、修复回路提示词）的输出必须带上结尾：
// 构建与测试工具的结论在最后。原来 truncate 只留开头 16KB，输出一长
// 结论就被截掉，修复 agent 看到的是一堆进度条。
func TestRunStepOutputKeepsConclusionAtTail(t *testing.T) {
	v := NewVerifier(30*time.Second, "")
	res := v.runStep(context.Background(), t.TempDir(), bigOutputStep(1, 1), nil)

	if res.Status != StatusFailed {
		t.Fatalf("Status got %v, want %v（err=%v）", res.Status, StatusFailed, res.Err)
	}
	for _, want := range []string{"HEAD-MARK", "THE-CONCLUSION"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("Output 应同时保留开头与结尾，缺 %q", want)
		}
	}
	if len(res.Output) > maxStepOutput+256 {
		t.Errorf("Output 长度 got %d, want ≤ %d 左右", len(res.Output), maxStepOutput)
	}
}

// 超大输出时内存分配有界：原来整份收进 bytes.Buffer（实测 64MB 输出分配
// 320MB：扩容翻倍 + 两次整份拷贝），并发验证时直接叠加。现在边写盘边丢中间。
// 输出取 48MB：低于落盘上限 maxStepLogBytes，才能同时验「磁盘日志完整」。
func TestRunStepMemoryIsBoundedForHugeOutput(t *testing.T) {
	dir := t.TempDir()
	logs := &fileStepLogger{dataDir: dir, rel: "logs"}
	v := NewVerifier(60*time.Second, "")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res := v.runStep(context.Background(), dir, bigOutputStep(48, 0), logs)
	runtime.ReadMemStats(&after)

	if res.Status != StatusPassed {
		t.Fatalf("Status got %v, want passed（err=%v）", res.Status, res.Err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("48MB 输出期间分配了 %d MB，want ≤ 16MB（输出不应整份进内存）", alloc>>20)
	}

	// 完整日志照样在盘上：开头、结尾、长度都对
	if res.LogRef == "" {
		t.Fatal("LogRef 不应为空")
	}
	data, err := os.ReadFile(filepath.Join(dir, res.LogRef))
	if err != nil {
		t.Fatalf("读完整日志失败: %v", err)
	}
	if !strings.HasPrefix(string(data), "HEAD-MARK") || !strings.Contains(string(data[len(data)-64:]), "THE-CONCLUSION") {
		t.Errorf("磁盘日志应是完整输出（头尾都在）")
	}
	if len(data) < 48<<20 {
		t.Errorf("磁盘日志 got %d 字节，want ≥ 48MB", len(data))
	}
}

// 步骤正常退出但留下继承了输出管道的后台进程（构建脚本起的守护进程之类）：
// 必须很快判为通过，并回收后台进程。原来 cmd.Wait 要等管道 EOF，
// 一直挂到步骤超时，把一次通过的验证报成「超时，没跑起来」。
func TestRunStepReturnsWhenBackgroundChildHoldsOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	st := Step{Name: StepBuild, Cmd: []string{"sh", "-c",
		"sleep 120 & echo $! > " + pidFile + "; echo build-ok; exit 0"}}

	v := NewVerifier(20*time.Second, "")
	start := time.Now()
	res := v.runStep(context.Background(), t.TempDir(), st, nil)
	elapsed := time.Since(start)

	if res.Status != StatusPassed {
		t.Fatalf("Status got %v, want passed（err=%v，耗时 %v）", res.Status, res.Err, elapsed)
	}
	if !strings.Contains(res.Output, "build-ok") {
		t.Errorf("Output got %q, 应包含 build-ok", res.Output)
	}
	if elapsed > 10*time.Second {
		t.Errorf("应很快返回，实际耗时 %v", elapsed)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("未写出后台进程 pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Skipf("pid 不可解析: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("后台进程 %d 在步骤结束后仍存活", pid)
}

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// f01（运行中插话）驱动层的 A 类回归红线：改动前写并提交，改动前后都必须绿。

// AC-4b（A 类回归红线）：被过滤的事件仍然喂看门狗。假 CLI 只吐
// command_lifecycle 行 2.4s，静默阈值 1s —— 不得被判 ErrIdle。
func TestFilteredEventsStillTouchIdle(t *testing.T) {
	lc := `{"type":"command_lifecycle","command_uuid":"c","state":"started","uuid":"u","session_id":"s"}`
	bin := fakeClaude(t, fmt.Sprintf(`
cat >/dev/null &
for i in 1 2 3 4 5 6 7 8; do echo '%s'; sleep 0.3; done
echo '%s'
`, lc, resultOKLine))
	cases := []struct {
		name string
		p    RunParams
	}{
		{"位置参数模式", RunParams{Prompt: "x", SessionID: "s"}},
	}
	cases = append(cases, streamInputIdleCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Driver{bin: bin, timeout: 30 * time.Second, idle: time.Second}
			res, err := d.Run(context.Background(), tc.p)
			if errors.Is(err, ErrIdle) {
				t.Fatalf("被过滤的事件也必须喂看门狗，got ErrIdle: %v", err)
			}
			if err != nil {
				t.Fatalf("Run 报错: %v", err)
			}
			if res == nil || !res.Success {
				t.Errorf("got=%+v want 成功 result", res)
			}
		})
	}
}

// AC-5 搭配的 A 类红线：不开流式时参数逐字不变。
func TestBuildArgsDefaultModeUnchanged(t *testing.T) {
	d := NewDriver("claude", time.Minute)
	got := strings.Join(d.buildArgs(RunParams{Prompt: "x"}), " ")
	if want := "--print --output-format stream-json --verbose x"; got != want {
		t.Errorf("buildArgs got=%q want=%q", got, want)
	}
}

// AC-6 / AC-6b 搭配的 A 类红线：读协程不等下游。OnEvent 把事件交给一个
// 每批写入 sleep 200ms 的 BatchSink（形同慢 DB 的 AgentEventRecorder），
// Run 的耗时应≈CLI 自身耗时，而不是「事件数 × 200ms」。
func TestRunDoesNotWaitForSink(t *testing.T) {
	var lines strings.Builder
	for i := 0; i < 30; i++ {
		lines.WriteString("echo '" + assistantLine + "'\n")
	}
	bin := fakeClaude(t, lines.String()+"echo '"+resultOKLine+"'\n")

	var writes atomic.Int32
	sink := NewBatchSink(context.Background(), func(ctx context.Context, entries []Entry) error {
		writes.Add(1)
		time.Sleep(200 * time.Millisecond)
		return nil
	}, "slow-sink-test")
	defer sink.Close()

	d := NewDriver(bin, 30*time.Second)
	start := time.Now()
	_, err := d.Run(context.Background(), RunParams{Prompt: "x", SessionID: "s", OnEvent: sink.OnEvent})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if limit := 1500 * time.Millisecond; elapsed > limit {
		t.Errorf("Run 耗时 got=%v want<%v（读协程不该等下游写入）", elapsed, limit)
	}
}

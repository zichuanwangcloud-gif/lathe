package agent

import (
	"testing"
	"time"
)

// 空闲看门狗：连续超过阈值没有输出就触发，有一行输出就清零。
// 阈值是分钟级、秒级轮询，测试里用毫秒级阈值同样的代码路径。
func TestIdleWatchFiresAfterSilence(t *testing.T) {
	t.Run("一直没有输出 → 到点触发", func(t *testing.T) {
		w := newIdleWatch(120 * time.Millisecond)
		defer w.stop()
		select {
		case <-w.fired:
		case <-time.After(3 * time.Second):
			t.Fatal("静默超过阈值却没有触发：卡死的轮次会一直挂到总超时")
		}
		if !w.firedNow() {
			t.Error("firedNow 应报告已触发（Run 据它区分「卡死」与「超时」）")
		}
	})

	t.Run("持续有输出 → 不触发", func(t *testing.T) {
		w := newIdleWatch(300 * time.Millisecond)
		defer w.stop()
		deadline := time.After(900 * time.Millisecond)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				w.touch()
			case <-deadline:
				if w.firedNow() {
					t.Fatal("一直在输出却判了卡死")
				}
				return
			}
		}
	})
}

// stop 之后不再触发，且重复 stop 不 panic（defer 里会和 killed 竞争）。
func TestIdleWatchStopIsIdempotent(t *testing.T) {
	w := newIdleWatch(50 * time.Millisecond)
	w.stop()
	w.stop()
	select {
	case <-w.fired:
		t.Fatal("stop 之后不该再触发")
	case <-time.After(200 * time.Millisecond):
	}
	if w.firedNow() {
		t.Error("stop 之后 firedNow 应为 false")
	}
}

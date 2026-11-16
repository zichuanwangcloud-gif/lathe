package runner

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestFakeNotifierConcurrentNotify：fakeNotifier 必须能被并发调用。
//
// 为什么要这条测试：Pipeline.Execute 在生产里是并发的（cmd/lathe/queue.go 的
// work 起一池 goroutine 各自 ClaimReady → runOneClaimed → Execute），而
// fail() 里的推送通知会并发打到同一个 Notifier 实例上，所以测试假件也得扛
// 并发 —— 否则用真 Notifier 跑绿的生产代码，会在测试里因假件自己的
// append 竞态被 -race 判红。
//
// 这条测试的失败形态是【确定性】的：不加锁的 append 在同一片底层数组上
// 并发写，-race 必报 DATA RACE（见 vcs_pipeline_test.go
// TestFailureExcerptRedactsPerTaskVCSToken 曾在 make test-race 下偶发命中）。
// 断言的语义面则是这假件的契约本身：并发写入的每条消息都要完整留下，
// 一条不丢、一条不重。
func TestFakeNotifierConcurrentNotify(t *testing.T) {
	const writers = 16

	n := &fakeNotifier{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 让所有 goroutine 尽量同时冲进 Notify
			if err := n.Notify(context.Background(), fmt.Sprintf("msg-%02d", i)); err != nil {
				t.Errorf("Notify(%d) 返回错误: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := len(n.msgs); got != writers {
		t.Errorf("并发 Notify 应记下 %d 条消息，实际 %d 条：%v", writers, got, n.msgs)
	}
	seen := make(map[string]int, writers)
	for _, m := range n.msgs {
		seen[m]++
	}
	for i := 0; i < writers; i++ {
		want := fmt.Sprintf("msg-%02d", i)
		if seen[want] != 1 {
			t.Errorf("消息 %q 应恰好出现 1 次，实际 %d 次（got 全量 %v）", want, seen[want], n.msgs)
		}
	}
}

package agent

import (
	"sync"
	"time"
)

// idleWatch 是「多久没输出算卡死」的看门狗。
//
// 与 context 的 deadline 分工：deadline 管「整体跑多久」，它管「还动不动」。
// 两者的失败要能分开报 —— 实测规划第 4 轮卡住时，人看到的只有「超时」，
// 而真实原因是进程十分钟一个字都没吐。
//
// 不用 time.AfterFunc 链式重置：每次事件都重置一次定时器的话，高频事件
// （thinking_tokens 每秒好几条）会不停创建/销毁定时器，且 reset 与 fire
// 之间存在竞态窗口。这里用一个每秒轮询的协程比对「最后一次事件时刻」，
// 精度是秒级 —— 判死阈值是分钟级，秒级精度足够，且没有竞态。
type idleWatch struct {
	timeout time.Duration
	fired   chan struct{}

	mu       sync.Mutex
	last     time.Time
	isFired  bool
	stopOnce sync.Once
	done     chan struct{}
}

func newIdleWatch(timeout time.Duration) *idleWatch {
	w := &idleWatch{
		timeout: timeout,
		fired:   make(chan struct{}),
		last:    time.Now(),
		done:    make(chan struct{}),
	}
	go w.loop()
	return w
}

// touch 记一次「有输出」。在读协程里调用，必须廉价。
func (w *idleWatch) touch() {
	w.mu.Lock()
	w.last = time.Now()
	w.mu.Unlock()
}

// firedNow 报告看门狗是否已经触发过（读协程收尾后由 Run 判错用）。
func (w *idleWatch) firedNow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.isFired
}

func (w *idleWatch) stop() { w.stopOnce.Do(func() { close(w.done) }) }

func (w *idleWatch) loop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			w.mu.Lock()
			silent := time.Since(w.last)
			fire := !w.isFired && silent >= w.timeout
			if fire {
				w.isFired = true
			}
			w.mu.Unlock()
			if fire {
				close(w.fired)
				return
			}
		}
	}
}

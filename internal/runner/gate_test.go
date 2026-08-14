package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyGates_IndependentLanes(t *testing.T) {
	g := NewVerifyGates(2, 1)
	ctx := context.Background()

	// light 占满不影响 heavy
	r1, _ := g.Acquire(ctx, TierLight)
	r2, _ := g.Acquire(ctx, TierLight)
	defer r1()
	defer r2()

	heavyDone := make(chan struct{})
	go func() {
		r, err := g.Acquire(ctx, TierHeavy)
		if err != nil {
			t.Errorf("heavy 获取失败: %v", err)
		}
		r()
		close(heavyDone)
	}()
	select {
	case <-heavyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("light 占满不应阻塞 heavy —— 两条通道独立配额（§6.2）")
	}

	// light 第三个必须阻塞
	blocked := make(chan struct{})
	go func() {
		r, err := g.Acquire(ctx, TierLight)
		if err == nil {
			r()
		}
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("light 第三个获取应阻塞（容量 2）")
	case <-time.After(100 * time.Millisecond):
	}
	r1() // 释放一个后应放行
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("释放槽位后应放行")
	}
}

func TestVerifyGates_ConcurrentHeavyNeverExceedsCap(t *testing.T) {
	g := NewVerifyGates(2, 1)
	ctx := context.Background()

	var inFlight int32
	var maxSeen int32
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			r, err := g.Acquire(ctx, TierHeavy)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer r()
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxSeen)
				if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if maxSeen > 1 {
		t.Fatalf("heavy 通道并发峰值 = %d，超过容量 1 —— 配额被穿透", maxSeen)
	}
}

func TestVerifyGates_ContextCancel(t *testing.T) {
	g := NewVerifyGates(0, 0) // 通道关闭
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := g.Acquire(ctx, TierLight); err == nil {
		t.Fatal("通道关闭且 ctx 到期应返回错误，而非永久阻塞")
	}
}

func TestVerifyGates_NilPassthrough(t *testing.T) {
	var g *VerifyGates
	r, err := g.Acquire(context.Background(), TierHeavy)
	if err != nil {
		t.Fatalf("nil 闸门应直接放行: %v", err)
	}
	r()
}

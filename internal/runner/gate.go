package runner

import "context"

// gate.go 实现 docs/02-design.md §6.2 的双通道限流。
//
// light 与 heavy 各自独立配额，不共用一个数字 —— 资源画像差一个量级
// （heavy 要跑完整测试套件，light 只做构建与静态检查）。
//
// 一个有意为之的设计点：闸门落在【验证阶段】而非派发时。§5.1 规定档位
// 在 diff 产出后才可判定，派发时根本没有 tier 可用；因此任务可以并发地
// 分诊与实现，进入验证时按定档结果排队。空转的实现并发不是浪费 ——
// 真正稀缺的是验证阶段的机器资源。

// VerifyGates 是按档位的验证准入闸门。
type VerifyGates struct {
	slots map[VerifyTier]chan struct{}
}

// NewVerifyGates 构造闸门；light/heavy 分别是两条通道的并发上限。
// 任一档位给 0 表示该通道关闭（验证会阻塞到 ctx 结束），给负数表示不限。
func NewVerifyGates(light, heavy int) *VerifyGates {
	mk := func(n int) chan struct{} {
		if n < 0 {
			n = 1 << 30 // 视为不限
		}
		return make(chan struct{}, n)
	}
	return &VerifyGates{slots: map[VerifyTier]chan struct{}{
		TierLight: mk(light),
		TierHeavy: mk(heavy),
	}}
}

// Acquire 占用 tier 通道的一个槽位，阻塞直到有空位或 ctx 结束。
// 返回的 release 必须被调用（通常 defer）。gates 为 nil 时直接放行，
// 便于测试与单任务模式。
func (g *VerifyGates) Acquire(ctx context.Context, tier VerifyTier) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	ch, ok := g.slots[tier]
	if !ok {
		// 未知档位按 light 处理：它便宜，阻塞代价小
		ch = g.slots[TierLight]
	}

	select {
	case ch <- struct{}{}:
		var once bool
		return func() {
			if !once {
				once = true
				<-ch
			}
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Capacity 返回某档位的通道容量（展示与测试用）。
func (g *VerifyGates) Capacity(tier VerifyTier) int {
	if g == nil {
		return 0
	}
	if ch, ok := g.slots[tier]; ok {
		return cap(ch)
	}
	return 0
}

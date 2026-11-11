package agent

import (
	"context"
	"errors"
	"testing"
)

// AC-1b：codex 的 turn.completed.usage → agent.Usage 映射（与 claude 口径同义）。
func TestUsageFromCodex(t *testing.T) {
	line6 := CodexUsage{InputTokens: 11, CachedInputTokens: 0, CacheWriteInputTokens: 0, OutputTokens: 3, ReasoningOutputTokens: 0}
	line15 := CodexUsage{InputTokens: 61, CachedInputTokens: 0, CacheWriteInputTokens: 0, OutputTokens: 12, ReasoningOutputTokens: 0}
	cases := []struct {
		name        string
		turns       []CodexUsage
		wantPresent bool
		wantQuad    usageQuad
	}{
		{"单 turn（transcript 第 6 行原文）", []CodexUsage{line6}, true, usageQuad{11, 3, 0, 0}},
		{"含缓存：input 减去两项缓存", []CodexUsage{{InputTokens: 100, CachedInputTokens: 30, CacheWriteInputTokens: 20, OutputTokens: 5}}, true, usageQuad{50, 5, 30, 20}},
		{"缓存大于输入：下限 0 不出负数", []CodexUsage{{InputTokens: 10, CachedInputTokens: 30}}, true, usageQuad{0, 0, 30, 0}},
		{"两个 turn 逐条相加", []CodexUsage{line6, line15}, true, usageQuad{72, 15, 0, 0}},
		{"没有 usage：Present=false 四项 0", nil, false, usageQuad{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UsageFromCodex(tc.turns)
			if got.Present != tc.wantPresent {
				t.Errorf("Present got %v, want %v", got.Present, tc.wantPresent)
			}
			if q := quadOf(got); q != tc.wantQuad {
				t.Errorf("In/Out/CR/CC got %+v, want %+v", q, tc.wantQuad)
			}
			if got.Models != nil {
				t.Errorf("Models got %+v, want nil（codex 不报模型名）", got.Models)
			}
		})
	}
}

type recordingUsage struct {
	recs []RunRecord
	err  error
	// ctxErrs 是写入那一刻 ctx.Err() 的快照（RecordRun 返回后其超时 ctx 已被 cancel）。
	ctxErrs []error
}

func (r *recordingUsage) RecordAgentRun(ctx context.Context, rec RunRecord) error {
	r.recs = append(r.recs, rec)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return r.err
}

// RecordRun：nil 记录器什么都不做；调用方 ctx 已取消也照写（WithoutCancel）；
// 落库失败不向上传播。
func TestRecordRun(t *testing.T) {
	t.Run("nil 记录器不 panic", func(t *testing.T) {
		RecordRun(context.Background(), nil, RunRecord{Kind: "task"})
	})
	t.Run("调用方 ctx 已取消仍写，且写入 ctx 未取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := &recordingUsage{}
		RecordRun(ctx, rec, RunRecord{Kind: "task", Phase: "implement"})
		if got := len(rec.recs); got != 1 {
			t.Fatalf("记录数 got %d, want 1", got)
		}
		if err := rec.ctxErrs[0]; err != nil {
			t.Errorf("写入 ctx.Err() got %v, want nil", err)
		}
	})
	t.Run("落库失败只告警", func(t *testing.T) {
		rec := &recordingUsage{err: errors.New("boom")}
		RecordRun(context.Background(), rec, RunRecord{Kind: "task"})
		if got := len(rec.recs); got != 1 {
			t.Errorf("记录数 got %d, want 1", got)
		}
	})
}

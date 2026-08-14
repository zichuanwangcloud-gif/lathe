package store

import (
	"context"
	"fmt"
)

// InsertVerification 落一条验证步骤结果（docs/02-design.md §4 verifications）。
//
// heavy 档的 repro_fail → repro_pass 是「红-绿证明」的落痕：PR 描述会
// 引用结论，但这张表才是可审计的证据本体。任务详情页直接展示。
func (s *Store) InsertVerification(ctx context.Context, taskID int64, tier, step, status string, durationMS int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO verifications (task_id, tier, step, status, duration_ms)
		VALUES ($1, $2, $3, $4, $5)`,
		taskID, tier, step, status, durationMS)
	if err != nil {
		return fmt.Errorf("store: 记录验证步骤 %s/%s 失败: %w", tier, step, err)
	}
	return nil
}

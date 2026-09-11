package store

import (
	"context"
	"fmt"
)

// InsertVerification 落一条验证步骤结果（docs/02-design.md §4 verifications）。
//
// heavy 档的 repro_fail → repro_pass 是「红-绿证明」的落痕：PR 描述会
// 引用结论，但这张表才是可审计的证据本体。任务详情页直接展示。
// logRef 是完整输出的落盘引用（相对 LATHE_DATA_DIR 的路径，T4）。
// 空串落 NULL —— 这一列可空，「没有日志」与「日志是空字符串」不是一回事，
// 读侧的 *string 靠 NULL 区分二者。
func (s *Store) InsertVerification(ctx context.Context, taskID int64, tier, step, status string, durationMS int64, logRef string) error {
	var ref *string
	if logRef != "" {
		ref = &logRef
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO verifications (task_id, tier, step, status, duration_ms, log_ref)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		taskID, tier, step, status, durationMS, ref)
	if err != nil {
		return fmt.Errorf("store: 记录验证步骤 %s/%s 失败: %w", tier, step, err)
	}
	return nil
}

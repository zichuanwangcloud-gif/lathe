package store

import (
	"context"
	"fmt"
)

// ClaimDelivery 登记一次 webhook 投递，返回是否为首次。
//
// 幂等的实现基础（docs/02-design.md §3 约束③）：webhook 会重投递，
// 靠 delivery_id 主键冲突把重复请求挡在业务处理之前。
// 调用方拿到 false 时应直接返回 200，不再重复建任务。
func (s *Store) ClaimDelivery(ctx context.Context, deliveryID, source string) (bool, error) {
	if deliveryID == "" {
		return false, fmt.Errorf("store: webhook delivery ID 为空")
	}
	if source == "" {
		source = "linear"
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (delivery_id, source)
		VALUES ($1, $2)
		ON CONFLICT (delivery_id) DO NOTHING`, deliveryID, source)
	if err != nil {
		return false, fmt.Errorf("store: 登记 webhook 投递失败: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinishDelivery 标记投递处理完毕；errMsg 非空表示处理失败。
//
// 失败也记录下来而不是删除记录：保留失败痕迹便于排障，
// 同时避免 Linear 重投递时又跑一遍已知会失败的处理。
func (s *Store) FinishDelivery(ctx context.Context, deliveryID, errMsg string) error {
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries
		SET processed_at = now(), error = $2
		WHERE delivery_id = $1`, deliveryID, errPtr); err != nil {
		return fmt.Errorf("store: 更新 webhook 投递状态失败: %w", err)
	}
	return nil
}

// DeliveryStatus 是一次投递的处理状态。
type DeliveryStatus struct {
	DeliveryID string
	Processed  bool
	Error      string
}

// Delivery 读取某次投递的状态，主要供排障使用。
func (s *Store) Delivery(ctx context.Context, deliveryID string) (*DeliveryStatus, error) {
	var (
		st        DeliveryStatus
		processed *string
		errMsg    *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT delivery_id, processed_at::text, error
		FROM webhook_deliveries WHERE delivery_id = $1`, deliveryID,
	).Scan(&st.DeliveryID, &processed, &errMsg)
	if err != nil {
		return nil, fmt.Errorf("store: 读取 webhook 投递失败: %w", err)
	}
	st.Processed = processed != nil
	if errMsg != nil {
		st.Error = *errMsg
	}
	return &st, nil
}

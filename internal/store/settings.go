package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// 系统设置（管理员级）的键名。消费方：预览环境资源闸门
// （internal/preview.Manager 在每次启动前现取，改完即刻生效）。
const (
	SettingPreviewMemThreshold  = "preview_mem_threshold"
	SettingPreviewDiskThreshold = "preview_disk_threshold"
)

// 阈值默认值：留 10% 余量给系统与构建尖峰。
const (
	DefaultPreviewMemThreshold  = 90
	DefaultPreviewDiskThreshold = 90
)

// SettingFlowMaxChainLength 是链长约束（PRD 07 §F3.3）的系统设置键名：
// 一条 depends_on 链允许的最大深度。消费方：internal/flow.Service —— 建
// 图时（CreateFlow）现取这个值，超限只警告、不拒绝创建（F3.3-AC1 的
// "UI 警告"本次没有 UI，落点是 CreateFlow 返回结果里的 warnings 字段，
// 供未来 M5 的画布 UI 消费）。
const SettingFlowMaxChainLength = "flow_max_chain_length"

// DefaultFlowMaxChainLength 是链长上限未配置时的默认值
// （docs/07-prd-orchestration.md §8.2 U2 已定：4，仅警告）。
const DefaultFlowMaxChainLength = 4

// ErrSettingNotFound 表示该键尚未配置（调用方应回退默认值）。
var ErrSettingNotFound = errors.New("store: 设置项不存在")

// Setting 读取一个系统设置。
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM system_settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSettingNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: 读取设置 %s 失败: %w", key, err)
	}
	return v, nil
}

// SetSetting 写入（或覆盖）一个系统设置。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO system_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	if err != nil {
		return fmt.Errorf("store: 写入设置 %s 失败: %w", key, err)
	}
	return nil
}

// ValidateThreshold 校验百分比阈值：1..100。
// 100 是合法值 —— 语义为「不启用该闸门」。
func ValidateThreshold(v int) error {
	if v < 1 || v > 100 {
		return fmt.Errorf("阈值须在 1..100 之间，得到 %d", v)
	}
	return nil
}

// PreviewThresholds 现取预览资源阈值；未配置或值损坏时回退默认值
// （设置页写入口径已校验，损坏只可能来自手工改库 —— 不该因此
// 把预览功能整体打挂）。
func (s *Store) PreviewThresholds(ctx context.Context) (mem, disk int, err error) {
	mem, disk = DefaultPreviewMemThreshold, DefaultPreviewDiskThreshold
	if v, e := s.Setting(ctx, SettingPreviewMemThreshold); e == nil {
		if n, e2 := strconv.Atoi(v); e2 == nil && ValidateThreshold(n) == nil {
			mem = n
		}
	}
	if v, e := s.Setting(ctx, SettingPreviewDiskThreshold); e == nil {
		if n, e2 := strconv.Atoi(v); e2 == nil && ValidateThreshold(n) == nil {
			disk = n
		}
	}
	return mem, disk, nil
}

// FlowMaxChainLength 现取链长上限（PRD F3.3-AC2）；未配置该键（表里没有
// 这一行）或值损坏时回退默认值 4 —— 与 PreviewThresholds 同一原则：
// 设置页写入口径本该已校验，损坏只可能来自手工改库，不该因此让建图
// 整体报错。error 始终为 nil，保留返回值是为了跟 PreviewThresholds 的
// 调用形状一致，给调用方（internal/flow.Service）留出以后收紧校验的口子。
func (s *Store) FlowMaxChainLength(ctx context.Context) (int, error) {
	v, err := s.Setting(ctx, SettingFlowMaxChainLength)
	if err != nil {
		return DefaultFlowMaxChainLength, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return DefaultFlowMaxChainLength, nil
	}
	return n, nil
}

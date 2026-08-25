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

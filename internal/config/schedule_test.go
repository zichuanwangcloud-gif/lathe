package config

import (
	"strings"
	"testing"
	"time"
)

// 定时建单的两个配置项（docs/features/scheduled-issue.md §3.7）。
func TestSchedulerConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.SchedulerEnabled() || cfg.ScheduleCatchUpWindow != 6*time.Hour {
			t.Errorf("默认: got enabled=%v window=%v, want true/6h", cfg.SchedulerEnabled(), cfg.ScheduleCatchUpWindow)
		}
	})

	enabled := []struct {
		val     string
		want    bool
		wantErr bool
	}{
		{"", true, false},
		{"true", true, false},
		{"false", false, false},
		{"yes", false, true},
	}
	for _, tc := range enabled {
		t.Run("LATHE_SCHEDULER_ENABLED="+tc.val, func(t *testing.T) {
			t.Setenv("LATHE_SCHEDULER_ENABLED", tc.val)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load: got err=%v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && cfg.SchedulerEnabled() != tc.want {
				t.Errorf("SchedulerEnabled: got %v, want %v", cfg.SchedulerEnabled(), tc.want)
			}
		})
	}

	window := []struct {
		val     string
		want    time.Duration
		errHas  string
		wantErr bool
	}{
		{"0", 0, "", false},
		{"168h", 168 * time.Hour, "", false},
		{"1d", 0, "d", true},
		{"169h", 0, "168h", true},
		{"-1h", 0, "0", true},
	}
	for _, tc := range window {
		t.Run("LATHE_SCHEDULE_CATCHUP_WINDOW="+tc.val, func(t *testing.T) {
			t.Setenv("LATHE_SCHEDULE_CATCHUP_WINDOW", tc.val)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load: got err=%v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), tc.errHas) || !strings.Contains(err.Error(), "LATHE_SCHEDULE_CATCHUP_WINDOW") {
					t.Errorf("错误应点名变量并含 %q，got %q", tc.errHas, err.Error())
				}
				return
			}
			if cfg.ScheduleCatchUpWindow != tc.want {
				t.Errorf("window: got %v, want %v", cfg.ScheduleCatchUpWindow, tc.want)
			}
		})
	}
}

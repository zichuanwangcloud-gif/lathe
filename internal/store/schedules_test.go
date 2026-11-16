package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 定时建单的 store 层契约（docs/features/scheduled-issue.md）。
// fixture 的 email 与名称带 UnixNano 随机量（AGENTS.md §5.5）。

func scheduleUser(t *testing.T, st *Store, tag string) int64 {
	t.Helper()
	var id int64
	email := fmt.Sprintf("sched-%s-%d@example.com", tag, time.Now().UnixNano())
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	})
	return id
}

func dailyParams(name string, next time.Time) ScheduleParams {
	minute := 9 * 60
	return ScheduleParams{
		Name: name, Enabled: true, Kind: "daily", MinuteOfDay: &minute, Timezone: "UTC",
		LinearTeamID: "team-1", LinearTeamKey: "ENG", IntakeMode: "assign",
		TitleTemplate: "每日 {{date}}", NextFireAt: &next, Now: next.Add(-time.Hour),
	}
}

func TestSchedulesCRUDAndOwnerFilter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	owner := scheduleUser(t, st, "owner")
	other := scheduleUser(t, st, "other")
	nonce := fmt.Sprint(time.Now().UnixNano())
	next := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

	sc, err := st.CreateSchedule(ctx, owner, dailyParams("r-"+nonce, next))
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if !sc.Enabled || sc.NextFireAt == nil || !sc.NextFireAt.Equal(next) {
		t.Errorf("新建规则: got enabled=%v next=%v, want true/%v", sc.Enabled, sc.NextFireAt, next)
	}

	if _, err := st.CreateSchedule(ctx, owner, dailyParams("r-"+nonce, next)); !errors.Is(err, ErrScheduleNameTaken) {
		t.Errorf("同名: got %v, want ErrScheduleNameTaken", err)
	}

	if _, err := st.GetSchedule(ctx, sc.ID, other); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("非属主 Get: got %v, want ErrScheduleNotFound", err)
	}
	list, err := st.ListSchedules(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == sc.ID {
			t.Errorf("非属主列表里出现了别人的规则 %d", sc.ID)
		}
	}
	list, _ = st.ListSchedules(ctx, owner)
	if len(list) != 1 || list[0].ID != sc.ID {
		t.Errorf("属主列表: got %d 条, want 1 条且 id=%d", len(list), sc.ID)
	}
	if n, _ := st.CountSchedules(ctx, owner); n != 1 {
		t.Errorf("CountSchedules: got %d, want 1", n)
	}

	p := dailyParams("r2-"+nonce, next)
	p.Enabled = false
	p.NextFireAt = nil
	up, err := st.UpdateSchedule(ctx, sc.ID, owner, p)
	if err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	if up.Enabled || up.NextFireAt != nil || up.Name != "r2-"+nonce {
		t.Errorf("停用后: got enabled=%v next=%v name=%q", up.Enabled, up.NextFireAt, up.Name)
	}
	if _, err := st.UpdateSchedule(ctx, sc.ID, other, p); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("非属主 Update: got %v, want ErrScheduleNotFound", err)
	}
	if err := st.DeleteSchedule(ctx, sc.ID, other); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("非属主 Delete: got %v, want ErrScheduleNotFound", err)
	}
	if err := st.DeleteSchedule(ctx, sc.ID, owner); err != nil {
		t.Errorf("属主 Delete: %v", err)
	}
}

func TestScheduleRunSlotUnique(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	owner := scheduleUser(t, st, "slot")
	slot := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	sc, err := st.CreateSchedule(ctx, owner, dailyParams(fmt.Sprint("slot-", time.Now().UnixNano()), slot))
	if err != nil {
		t.Fatal(err)
	}

	run, err := st.InsertManualRun(ctx, sc.ID, owner, slot, testUUID(), "n1", testUUID(), time.Minute)
	if err != nil || run == nil {
		t.Fatalf("第一次插入: run=%v err=%v", run, err)
	}
	if run.Status != "pending" || run.Trigger != "manual" || run.Attempts != 1 {
		t.Errorf("手动 run: got status=%s trigger=%s attempts=%d", run.Status, run.Trigger, run.Attempts)
	}
	again, err := st.InsertManualRun(ctx, sc.ID, owner, slot, testUUID(), "n1", testUUID(), time.Minute)
	if !errors.Is(err, ErrRunSlotTaken) || again != nil {
		t.Errorf("同一时刻第二次: got run=%v err=%v, want nil/ErrRunSlotTaken", again, err)
	}
	other := scheduleUser(t, st, "slot-other")
	if _, err := st.InsertManualRun(ctx, sc.ID, other, slot.Add(time.Minute), testUUID(), "n1", testUUID(), time.Minute); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("非属主插入: got %v, want ErrScheduleNotFound", err)
	}
}

// testUUID 生成测试用 UUID v4（本仓不引 google/uuid，见 prd/uuid.go）。
func testUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// pgCode 取出 23514 等 SQLSTATE 与约束名。
func pgCode(err error) (string, string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

func TestSchedulesEnabledNextCheck(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	owner := scheduleUser(t, st, "chk")
	cases := []struct {
		name    string
		enabled bool
		next    any
		wantErr bool
	}{
		{"enabled_without_next_rejected", true, nil, true},
		{"disabled_with_next_rejected", false, time.Now(), true},
		{"enabled_with_next_ok", true, time.Now(), false},
		{"disabled_without_next_ok", false, nil, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.pool.Exec(ctx, `
				INSERT INTO schedules (user_id, name, enabled, kind, minute_of_day, linear_team_id, title_template, next_fire_at)
				VALUES ($1, $2, $3, 'daily', 540, 't', 'x', $4)`,
				owner, fmt.Sprintf("chk-%d-%d", i, time.Now().UnixNano()), tc.enabled, tc.next)
			code, cons := pgCode(err)
			if tc.wantErr {
				if code != "23514" || cons != "schedules_enabled_next" {
					t.Errorf("got code=%q constraint=%q err=%v, want 23514/schedules_enabled_next", code, cons, err)
				}
			} else if err != nil {
				t.Errorf("合法组合写入失败: %v", err)
			}
		})
	}
}

func TestScheduleRunPendingLeasedCheck(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	owner := scheduleUser(t, st, "lease")
	sc, err := st.CreateSchedule(ctx, owner, dailyParams(fmt.Sprint("lease-", time.Now().UnixNano()), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		status  string
		wantErr bool
	}{
		{"pending_without_lease_rejected", "pending", true},
		{"skipped_without_lease_ok", "skipped", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.pool.Exec(ctx, `
				INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status, linear_issue_uuid)
				VALUES ($1, $2, $3, 'schedule', $4, $5)`,
				sc.ID, owner, time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC), tc.status, testUUID())
			code, cons := pgCode(err)
			if tc.wantErr {
				if code != "23514" || cons != "schedule_runs_pending_leased" {
					t.Errorf("got code=%q constraint=%q err=%v, want 23514/schedule_runs_pending_leased", code, cons, err)
				}
			} else if err != nil {
				t.Errorf("合法写入失败: %v", err)
			}
		})
	}
}

package task

import (
	"context"
	"testing"
	"time"
)

// f06：执行 agent 在建任务那刻从仓库钉死；之后改仓库配置不影响在途任务；
// 0048 之前的老任务（NULL）三种读法都读成 claude。
func TestCreatePinsRepoAgentDriver(t *testing.T) {
	cases := []struct {
		name string
		// repoDriver 是建任务那一刻仓库的驱动
		repoDriver string
		// nullOut 为真时建完把任务行的 agent_driver 置 NULL（模拟老任务）
		nullOut bool
		want    string
	}{
		{name: "给定仓库是 codex 断言新任务钉死为 codex 且仓库改回 claude 后仍是 codex", repoDriver: "codex", want: "codex"},
		{name: "给定仓库是默认 claude 断言新任务是 claude", repoDriver: "claude", want: "claude"},
		{name: "给定任务行 agent_driver 为 NULL（老任务）断言读成 claude", repoDriver: "codex", nullOut: true, want: "claude"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			m := NewMachine(pool)
			userID, repoID := fixture(t, pool)
			ctx := context.Background()

			if _, err := pool.Exec(ctx, `UPDATE repos SET agent_driver = $2 WHERE id = $1`, repoID, tc.repoDriver); err != nil {
				t.Fatalf("设置仓库驱动失败: %v", err)
			}
			drainForeignQueue(t, m, ctx)
			tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID,
				ExternalKey: "CR-F06-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+i))})
			if err != nil {
				t.Fatalf("建任务失败: %v", err)
			}
			if !tc.nullOut && tk.AgentDriver != tc.repoDriver {
				t.Errorf("Create 返回的 AgentDriver got %q want %q", tk.AgentDriver, tc.repoDriver)
			}
			// 之后把仓库改成另一个驱动：在途任务不该跟着变
			other := "claude"
			if tc.repoDriver == "claude" {
				other = "codex"
			}
			if _, err := pool.Exec(ctx, `UPDATE repos SET agent_driver = $2 WHERE id = $1`, repoID, other); err != nil {
				t.Fatal(err)
			}
			if tc.nullOut {
				if _, err := pool.Exec(ctx, `UPDATE tasks SET agent_driver = NULL WHERE id = $1`, tk.ID); err != nil {
					t.Fatal(err)
				}
			}

			got, err := m.Get(ctx, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.AgentDriver != tc.want {
				t.Errorf("Get 的 AgentDriver got %q want %q", got.AgentDriver, tc.want)
			}
			claimed, err := m.ClaimReady(ctx, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if claimed == nil || claimed.ID != tk.ID {
				t.Fatalf("ClaimReady 应领到本用例的任务 %d，got %+v", tk.ID, claimed)
			}
			if claimed.AgentDriver != tc.want {
				t.Errorf("ClaimReady 的 AgentDriver got %q want %q", claimed.AgentDriver, tc.want)
			}
			moved, err := m.Transition(ctx, tk.ID, StateTriaging, "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if moved.AgentDriver != tc.want {
				t.Errorf("Transition 返回的 AgentDriver got %q want %q", moved.AgentDriver, tc.want)
			}
		})
	}
}

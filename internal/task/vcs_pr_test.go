package task

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// AC-17：按 PR 找任务多一道平台过滤；两处属主过滤原样保留。
func TestTasksByVCSPRFiltersProvider(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	ctx := context.Background()

	userA, _ := fixture(t, pool) // A 名下已有一个 github acme/demo
	userB, _ := fixture(t, pool)

	// A 另登记一个 gitlab 仓库（路径不同：唯一约束是 (user_id, provider_repo)）。
	path := "grp-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/demo"
	var glRepo int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO repos (user_id, provider_repo, vcs_provider, vcs_base_url)
		VALUES ($1, $2, 'gitlab', 'https://gitlab.example.com') RETURNING id`, userA, path).Scan(&glRepo); err != nil {
		t.Fatalf("建 gitlab repo 失败: %v", err)
	}
	tk, err := m.Create(ctx, CreateParams{UserID: userA, RepoID: glRepo, ExternalKey: "CR-VCS-5"})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if err := m.SetPRNumber(ctx, tk.ID, 5); err != nil {
		t.Fatalf("SetPRNumber 失败: %v", err)
	}

	cases := []struct {
		name     string
		user     int64
		provider string
		repo     string
		wantIDs  []int64
	}{
		{"按 github 查 gitlab 仓库的 PR → 空", userA, "github", path, nil},
		{"按 gitlab 查 → 命中", userA, "gitlab", path, []int64{tk.ID}},
		{"按 gitea 查 → 空", userA, "gitea", path, nil},
		{"用户 B 同参数查 → 空（属主过滤）", userB, "gitlab", path, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.TasksByVCSPR(ctx, tc.user, tc.provider, tc.repo, 5)
			if err != nil {
				t.Fatalf("TasksByVCSPR 失败: %v", err)
			}
			var ids []int64
			for _, x := range got {
				ids = append(ids, x.ID)
			}
			if len(ids) != len(tc.wantIDs) || (len(ids) > 0 && ids[0] != tc.wantIDs[0]) {
				t.Errorf("got %v，want %v", ids, tc.wantIDs)
			}
		})
	}
}

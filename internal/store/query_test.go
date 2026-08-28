package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// testUser 造一个用于仓库配置测试的用户，清理时级联删除其名下的仓库
// （repos.user_id ON DELETE CASCADE）。
func testUser(t *testing.T, st *Store) int64 {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("baseline-test-%d@example.com", time.Now().UnixNano())
	u, err := st.NewUsers().Create(ctx, email, "x", "member")
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	t.Cleanup(func() { _ = st.NewUsers().Delete(context.Background(), u.ID) })
	return u.ID
}

func TestCreateRepoBaselineDirDefaultsEmpty(t *testing.T) {
	st := testStore(t)
	userID := testUser(t, st)
	ctx := context.Background()

	repo, err := st.CreateRepo(ctx, userID, CreateRepoParams{ProviderRepo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.BaselineDir != "" {
		t.Errorf("新建仓库未指定基线目录，应为空串，得到 %q", repo.BaselineDir)
	}
}

// UpdateRepo 的 BaselineDir 是三态：nil=不动，空串=清空，非空=设置新值——
// 与既有的 VerifyTierOverride 完全同一套写法（同一处 CASE 表达式）。
func TestUpdateRepoBaselineDirTriState(t *testing.T) {
	st := testStore(t)
	userID := testUser(t, st)
	ctx := context.Background()

	repo, err := st.CreateRepo(ctx, userID, CreateRepoParams{ProviderRepo: "acme/tristate"})
	if err != nil {
		t.Fatal(err)
	}

	// 1. nil：不动（仍为空）
	repo, err = st.UpdateRepo(ctx, repo.ID, userID, UpdateRepoParams{})
	if err != nil {
		t.Fatal(err)
	}
	if repo.BaselineDir != "" {
		t.Errorf("BaselineDir 传 nil 不应改变现状，得到 %q", repo.BaselineDir)
	}

	// 2. 设置新值
	dir := "/opt/CloudRouter"
	repo, err = st.UpdateRepo(ctx, repo.ID, userID, UpdateRepoParams{BaselineDir: &dir})
	if err != nil {
		t.Fatal(err)
	}
	if repo.BaselineDir != dir {
		t.Fatalf("设置基线目录失败，得到 %q", repo.BaselineDir)
	}

	// 3. nil：再次不动，应保留上一步设的值
	repo, err = st.UpdateRepo(ctx, repo.ID, userID, UpdateRepoParams{})
	if err != nil {
		t.Fatal(err)
	}
	if repo.BaselineDir != dir {
		t.Errorf("BaselineDir 传 nil 不应清空已设置的值，得到 %q", repo.BaselineDir)
	}

	// 4. 空串：显式清空
	empty := ""
	repo, err = st.UpdateRepo(ctx, repo.ID, userID, UpdateRepoParams{BaselineDir: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if repo.BaselineDir != "" {
		t.Errorf("BaselineDir 传空串应清空，得到 %q", repo.BaselineDir)
	}
}

func TestGetRepoIsolatesByUser(t *testing.T) {
	st := testStore(t)
	owner := testUser(t, st)
	other := testUser(t, st)
	ctx := context.Background()

	repo, err := st.CreateRepo(ctx, owner, CreateRepoParams{ProviderRepo: "acme/isolated"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetRepo(ctx, repo.ID, owner); err != nil {
		t.Errorf("属主应能读到自己的仓库配置: %v", err)
	}
	if _, err := st.GetRepo(ctx, repo.ID, other); err == nil {
		t.Error("非属主读取应失败（对非属主隐瞒存在），却成功了")
	}
}

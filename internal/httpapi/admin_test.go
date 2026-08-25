package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/Clouditera/lathe/internal/store"
)

// asAdmin 建一个管理员并登录，返回其账号。
func (f *accountFixture) asAdmin(t *testing.T) *store.User {
	t.Helper()
	email := f.email(t, "admin")
	u := f.mkUser(t, email, "admin-password-1", store.RoleAdmin)
	f.login(t, email, "admin-password-1")
	return u
}

func TestAdminListsUsersWithTaskCounts(t *testing.T) {
	f := newAccountFixture(t)
	f.asAdmin(t)
	target := f.mkUser(t, f.email(t, "counted"), "member-password-1", store.RoleMember)

	// 造一个仓库和两条任务，验证聚合确实按用户算
	var repoID int64
	if err := f.st.Pool().QueryRow(context.Background(), `
		INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
		target.ID, "acme/"+t.Name()).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"merged", "failed"} {
		if _, err := f.st.Pool().Exec(context.Background(), `
			INSERT INTO tasks (user_id, repo_id, linear_issue_key, state)
			VALUES ($1, $2, $3, $4)`,
			target.ID, repoID, "T-"+state+"-"+t.Name(), state); err != nil {
			t.Fatal(err)
		}
	}

	resp, body := f.req(t, "GET", "/api/admin/users", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("管理员应能读用户列表，得到 %d", resp.StatusCode)
	}

	users, _ := body["users"].([]any)
	var found map[string]any
	for _, raw := range users {
		u, _ := raw.(map[string]any)
		if u["id"] == float64(target.ID) {
			found = u
		}
	}
	if found == nil {
		t.Fatal("列表里应包含刚建的用户")
	}
	if found["taskTotal"] != float64(2) {
		t.Errorf("任务总数应为 2，得到 %v", found["taskTotal"])
	}
	if found["taskOk"] != float64(1) {
		t.Errorf("成功数应为 1，得到 %v", found["taskOk"])
	}
	if found["taskFailed"] != float64(1) {
		t.Errorf("失败数应为 1，得到 %v", found["taskFailed"])
	}
}

func TestAdminCannotActOnSelf(t *testing.T) {
	f := newAccountFixture(t)
	me := f.asAdmin(t)

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/admin/users/{id}/disable"},
		{"DELETE", "/api/admin/users/{id}"},
	} {
		path := replaceID(tc.path, me.ID)
		resp, _ := f.req(t, tc.method, path, "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s 应 409，得到 %d", tc.method, path, resp.StatusCode)
		}
	}
}

// 代重置密码：返回明文一次、强制对方改密、踢光其在线会话。
func TestAdminResetPassword(t *testing.T) {
	f := newAccountFixture(t)
	f.asAdmin(t)

	email := f.email(t, "victim")
	target := f.mkUser(t, email, "old-password-1", store.RoleMember)

	// 让目标先登录，拿一个独立的会话，稍后验证它被踢掉
	victim := newAccountFixtureClient(t, f)
	victim.login(t, email, "old-password-1")
	if resp, _ := victim.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusOK {
		t.Fatal("目标用户重置前应能访问")
	}

	resp, body := f.req(t, "POST", replaceID("/api/admin/users/{id}/password", target.ID), `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重置应成功，得到 %d: %v", resp.StatusCode, body)
	}
	newPW, _ := body["password"].(string)
	if newPW == "" {
		t.Fatal("响应里应带一次性的明文密码")
	}
	if body["generated"] != true {
		t.Error("未指定密码时应标记为自动生成")
	}

	if resp, _ := victim.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("重置后目标的旧会话应失效，得到 %d", resp.StatusCode)
	}

	// 新密码可登录，且被要求先改密
	resp = victim.login(t, email, newPW)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("新密码应能登录，得到 %d", resp.StatusCode)
	}
	if r, _ := victim.req(t, "GET", "/api/protected", ""); r.StatusCode != http.StatusConflict {
		t.Fatalf("代重置的密码应强制改密（409），得到 %d", r.StatusCode)
	}
}

func TestAdminDeleteUserCascades(t *testing.T) {
	f := newAccountFixture(t)
	f.asAdmin(t)
	target := f.mkUser(t, f.email(t, "gone"), "member-password-1", store.RoleMember)

	var repoID int64
	if err := f.st.Pool().QueryRow(context.Background(), `
		INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
		target.ID, "acme/"+t.Name()).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool().Exec(context.Background(), `
		INSERT INTO tasks (user_id, repo_id, linear_issue_key, state)
		VALUES ($1, $2, $3, 'queued')`, target.ID, repoID, "T-del-"+t.Name()); err != nil {
		t.Fatal(err)
	}

	resp, _ := f.req(t, "DELETE", replaceID("/api/admin/users/{id}", target.ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应成功，得到 %d", resp.StatusCode)
	}

	// 外键级联应当把仓库与任务一并带走
	for _, q := range []string{
		`SELECT count(*) FROM tasks WHERE user_id = $1`,
		`SELECT count(*) FROM repos WHERE user_id = $1`,
		`SELECT count(*) FROM users WHERE id = $1`,
	} {
		var n int
		if err := f.st.Pool().QueryRow(context.Background(), q, target.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s 应为 0，得到 %d", q, n)
		}
	}
}

func TestAdminEnableDisableRoundTrip(t *testing.T) {
	f := newAccountFixture(t)
	f.asAdmin(t)
	target := f.mkUser(t, f.email(t, "toggle"), "member-password-1", store.RoleMember)

	if resp, _ := f.req(t, "POST", replaceID("/api/admin/users/{id}/disable", target.ID), ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("停用应成功，得到 %d", resp.StatusCode)
	}
	u, err := f.users.ByID(context.Background(), target.ID)
	if err != nil || !u.Disabled() {
		t.Fatal("停用后 disabled_at 应非空")
	}

	if resp, _ := f.req(t, "POST", replaceID("/api/admin/users/{id}/enable", target.ID), ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("启用应成功，得到 %d", resp.StatusCode)
	}
	u, err = f.users.ByID(context.Background(), target.ID)
	if err != nil || u.Disabled() {
		t.Fatal("启用后 disabled_at 应为空")
	}
}

func TestAdminSetRole(t *testing.T) {
	f := newAccountFixture(t)
	f.asAdmin(t)
	target := f.mkUser(t, f.email(t, "promote"), "member-password-1", store.RoleMember)

	resp, _ := f.req(t, "POST", replaceID("/api/admin/users/{id}/role", target.ID), `{"role":"admin"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("提升角色应成功，得到 %d", resp.StatusCode)
	}
	u, _ := f.users.ByID(context.Background(), target.ID)
	if !u.IsAdmin() {
		t.Fatal("角色应已变为管理员")
	}

	resp, _ = f.req(t, "POST", replaceID("/api/admin/users/{id}/role", target.ID), `{"role":"nonsense"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法角色应 400，得到 %d", resp.StatusCode)
	}
}

// 系统设置：预览资源阈值的读写与校验（管理员限定；member 应被挡）。
// 注意：测试库是共享的，阈值是全局单例 —— 读回断言用「保存→读回」
// 闭环，不断言出厂默认值；测试结束恢复原值，不给别的用例留现场。
func TestAdminSettingsThresholds(t *testing.T) {
	f := newAccountFixture(t)
	f.asAdmin(t)

	// 记录原值，测试结束恢复
	resp, body := f.req(t, "GET", "/api/admin/settings", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读设置应 200，得到 %d", resp.StatusCode)
	}
	origMem := int(body["previewMemThreshold"].(float64))
	origDisk := int(body["previewDiskThreshold"].(float64))
	t.Cleanup(func() {
		f.req(t, "PUT", "/api/admin/settings",
			fmt.Sprintf(`{"previewMemThreshold":%d,"previewDiskThreshold":%d}`, origMem, origDisk))
	})

	// 保存合法值并读回
	resp, _ = f.req(t, "PUT", "/api/admin/settings", `{"previewMemThreshold":85,"previewDiskThreshold":80}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存应 200，得到 %d", resp.StatusCode)
	}
	_, body = f.req(t, "GET", "/api/admin/settings", "")
	if body["previewMemThreshold"] != float64(85) || body["previewDiskThreshold"] != float64(80) {
		t.Errorf("保存后读回不符: %v", body)
	}

	// 越界值 → 400
	resp, _ = f.req(t, "PUT", "/api/admin/settings", `{"previewMemThreshold":0,"previewDiskThreshold":80}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("阈值 0 应 400，得到 %d", resp.StatusCode)
	}
	resp, _ = f.req(t, "PUT", "/api/admin/settings", `{"previewMemThreshold":85,"previewDiskThreshold":101}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("阈值 101 应 400，得到 %d", resp.StatusCode)
	}
}

func TestAdminSettingsRequireAdmin(t *testing.T) {
	f := newAccountFixture(t)
	f.mkUser(t, f.email(t, "member"), "member-password-1", store.RoleMember)
	f.login(t, f.email(t, "member"), "member-password-1")

	resp, _ := f.req(t, "GET", "/api/admin/settings", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("member 读系统设置应 403，得到 %d", resp.StatusCode)
	}
}

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// AC-30：管理员删账号时的团队 owner 守卫。
func TestAdminDeleteLastTeamOwnerBlocked(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// setup 返回团队 id；target 是被删的人
		setup      func(f *accountFixture, target int64) int64
		wantCode   int
		wantUser   bool // 删完账号是否仍在
		wantTeam   bool // 删完团队是否仍在
		wantOwners int  // 团队仍在时 owner 数
	}{
		{"owner 且团队有其他成员 → 409", func(f *accountFixture, target int64) int64 {
			other := f.mkUser(t, f.email(t, "other"), "member-password-1", store.RoleMember)
			return teamWith(t, f.st, map[int64]string{target: "owner", other.ID: "member"})
		}, http.StatusConflict, true, true, 1},
		{"团队唯一成员 → 200，团队同事务删除", func(f *accountFixture, target int64) int64 {
			return teamWith(t, f.st, map[int64]string{target: "owner"})
		}, http.StatusOK, false, false, 0},
		{"只是 member → 200，团队与 owner 不变", func(f *accountFixture, target int64) int64 {
			other := f.mkUser(t, f.email(t, "keeper"), "member-password-1", store.RoleMember)
			return teamWith(t, f.st, map[int64]string{other.ID: "owner", target: "member"})
		}, http.StatusOK, false, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAccountFixture(t)
			f.asAdmin(t)
			target := f.mkUser(t, f.email(t, "target"), "member-password-1", store.RoleMember)
			team := c.setup(f, target.ID)

			resp, _ := f.req(t, "DELETE", replaceID("/api/admin/users/{id}", target.ID), "")
			if resp.StatusCode != c.wantCode {
				t.Fatalf("got %d want %d", resp.StatusCode, c.wantCode)
			}
			var n int
			_ = f.st.Pool().QueryRow(ctx, `SELECT count(*) FROM users WHERE id=$1`, target.ID).Scan(&n)
			if (n == 1) != c.wantUser {
				t.Errorf("账号存在 got %v want %v", n == 1, c.wantUser)
			}
			_ = f.st.Pool().QueryRow(ctx, `SELECT count(*) FROM teams WHERE id=$1`, team).Scan(&n)
			if (n == 1) != c.wantTeam {
				t.Errorf("团队存在 got %v want %v", n == 1, c.wantTeam)
			}
			if c.wantTeam {
				_ = f.st.Pool().QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id=$1 AND role='owner'`, team).Scan(&n)
				if n != c.wantOwners {
					t.Errorf("owner 数 got %d want %d", n, c.wantOwners)
				}
			}
		})
	}
}

func teamWith(t *testing.T, st *store.Store, members map[int64]string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := st.Pool().QueryRow(ctx, `INSERT INTO teams (name) VALUES ($1) RETURNING id`, "f12-adm-"+teamRand()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.Pool().Exec(context.Background(), `DELETE FROM teams WHERE id=$1`, id) })
	for uid, role := range members {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO team_members (team_id,user_id,role) VALUES ($1,$2,$3)`, id, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// AC-30b：AdminAPI.Store 为 nil 时删账号 fail-closed（503），不回落到 Users.Delete。
func TestAdminRemoveWithoutStoreFailsClosed(t *testing.T) {
	f := newAccountFixture(t)
	admin := f.asAdmin(t)
	target := f.mkUser(t, f.email(t, "nostore"), "member-password-1", store.RoleMember)

	a := NewAuth(apiTestToken)
	a.Bootstrap = admin
	mux := http.NewServeMux()
	(&AdminAPI{Users: f.users, Sessions: f.st.NewSessions(), Resets: f.st.NewResets(), Auth: a}).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := do(t, srv, "DELETE", replaceID("/api/admin/users/{id}", target.ID), "", true)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %d want 503", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["error"] != "团队守卫未装配，拒绝删除账号" {
		t.Errorf("error got %v", body["error"])
	}
	if _, err := f.users.ByID(context.Background(), target.ID); err != nil {
		t.Errorf("账号应仍在: %v", err)
	}
}

// AC-54 ②③：全站只有一个任务读归属 helper，且 taskEvents 调它、不内联属主判定。
func TestOwnedTaskIsTheOnlyTaskReadHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	defs := 0
	var events string
	defRe := regexp.MustCompile(`(?m)^func \(a \*API\) ownedTask\(`)
	evRe := regexp.MustCompile(`(?ms)^func \(a \*API\) taskEvents\(.*?^}`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		defs += len(defRe.FindAll(b, -1))
		if m := evRe.Find(b); m != nil {
			events = string(m)
		}
	}
	if defs != 1 {
		t.Errorf("ownedTask 定义数 got %d want 1", defs)
	}
	if !strings.Contains(events, "a.ownedTask(") {
		t.Errorf("taskEvents 应调用 a.ownedTask(")
	}
	if strings.Contains(events, "tk.UserID != CurrentUser(r).ID") {
		t.Errorf("taskEvents 不应内联属主判定")
	}
}

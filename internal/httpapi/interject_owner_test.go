package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// AC-10：用户 B 对用户 A 的任务 POST /interject 与 GET /interjections → 404
// （JSON {"error":"任务不存在"}，不是 mux 的纯文本 404），且库里无新增行。
func TestInterjectNonOwner404(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	ctx := context.Background()
	var ownerID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	tk, err := m.Create(ctx, task.CreateParams{UserID: ownerID, RepoID: repoID, ExternalKey: uniqueIssueKey("CR-IJ404")})
	if err != nil {
		t.Fatal(err)
	}
	_ = wireInterjectorForOwnerTest(t, api)

	// 换成另一个用户登录
	otherID := mustUser(t, st, "ij-other-"+uniqueIssueKey("u")+"@example.com")
	api.Auth = authAs(otherID, "other@example.com")
	srv := apiServer(t, api)

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/tasks/" + itoa(tk.ID) + "/interject", `{"message":"别动 migration"}`},
		{"GET", "/api/tasks/" + itoa(tk.ID) + "/interjections?after_rev=0", ""},
	} {
		t.Run(c.method, func(t *testing.T) {
			resp := do(t, srv, c.method, c.path, c.body, true)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status got=%d want=404", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type got=%q want=application/json（路由不存在时 mux 回 text/plain）", ct)
			}
			if body := decode(t, resp); body["error"] != "任务不存在" {
				t.Errorf("body got=%v want error=任务不存在", body)
			}
		})
	}
	assertNoInterjectionRows(t, st, tk.ID)
}

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// ---------------------------------------------------------------- AC-5 保存路径

// POST 含 ghp_ 的技能 → 400，带行号不带 token；库里无新版本行。
func TestSaveRejectsCredentialInSkill(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "cred")
	token := "ghp_" + strings.Repeat("Q9w8", 6)
	code, body := u.create(t, "acme-testing", "第一行\n用法 "+token)
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 got %d, want 400（body %v）", code, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "第 ") || !strings.Contains(msg, "行") {
		t.Errorf("错误应含行号，got %q", msg)
	}
	if strings.Contains(msg, token) || strings.Contains(msg, "Q9w8") {
		t.Errorf("错误不得回显 token，got %q", msg)
	}
	if n := e.count(t, `SELECT count(*) FROM team_skill_versions v JOIN team_skills s ON s.id=v.skill_id WHERE s.repo_id=$1`, u.repoID); n != 0 {
		t.Errorf("被拦下的技能不应落库，got %d 行", n)
	}
}

// 用户已保存的集成凭据（不像任何形态）抄进技能 → 400（层 1 的第 2 类登记值生效）；
// 新版本与恢复两个写入路径同样拦。
func TestSaveRejectsCredentialFromUserIntegration(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "integ")
	val := "plainvalue" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := e.secrets.Save(context.Background(), u.id, store.KindLinear, val); err != nil {
		t.Fatal(err)
	}
	if code, body := u.create(t, "acme-testing", "抄进来的 "+val); code != http.StatusBadRequest {
		t.Fatalf("新建：状态码 got %d, want 400（body %v）", code, body)
	}
	code, body := u.create(t, "acme-testing", "干净的")
	if code != http.StatusCreated {
		t.Fatalf("干净内容新建 got %d (%v)", code, body)
	}
	sid := skillID(t, body)
	resp := do(t, u.srv, "POST", fmt.Sprintf("%s/%d/versions", u.base(), sid), versionJSON("acme-testing", "又抄 "+val), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("新版本：状态码 got %d, want 400", resp.StatusCode)
	}
}

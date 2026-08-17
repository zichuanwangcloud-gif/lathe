package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/store"
)

// 注册 → 自动登录 → 访问受保护接口，全链路走 Cookie。
func TestRegisterThenAccess(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "reg")

	resp, body := f.req(t, "POST", "/api/register",
		`{"email":"`+email+`","password":"good-password-1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("注册应成功，得到 %d: %v", resp.StatusCode, body)
	}
	t.Cleanup(func() {
		_, _ = f.st.Pool().Exec(context.Background(), `DELETE FROM users WHERE email = $1`, email)
	})

	user, _ := body["user"].(map[string]any)
	if user["role"] != store.RoleMember {
		t.Fatalf("新注册用户应为普通成员，得到 %v", user["role"])
	}

	// 注册直接签发了会话，cookie jar 里应该已经有它
	resp, body = f.req(t, "GET", "/api/protected", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("注册后应能访问受保护接口，得到 %d", resp.StatusCode)
	}
	// actor 必须是真实用户 id，不能再是写死的 user:admin
	if actor, _ := body["actor"].(string); actor == "user:admin" || actor == "system" {
		t.Fatalf("审计 actor 应为真实用户，得到 %q", actor)
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "dup")
	f.mkUser(t, email, "existing-password-1", store.RoleMember)

	resp, _ := f.req(t, "POST", "/api/register",
		`{"email":"`+email+`","password":"another-password-1"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复邮箱应返回 409，得到 %d", resp.StatusCode)
	}
}

// 「邮箱不存在」与「密码错误」必须给出一字不差的响应，
// 否则登录接口就成了枚举已注册邮箱的预言机。
func TestLoginDoesNotDistinguishMissingUser(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "exists")
	f.mkUser(t, email, "right-password-1", store.RoleMember)

	respA, bodyA := f.req(t, "POST", "/api/login",
		`{"email":"`+email+`","password":"wrong-password-1"}`)
	respB, bodyB := f.req(t, "POST", "/api/login",
		`{"email":"`+f.email(t, "nobody")+`","password":"wrong-password-1"}`)

	if respA.StatusCode != http.StatusUnauthorized || respB.StatusCode != http.StatusUnauthorized {
		t.Fatalf("两者都应 401，得到 %d 与 %d", respA.StatusCode, respB.StatusCode)
	}
	if bodyA["error"] != bodyB["error"] {
		t.Fatalf("两者错误文案必须一致，得到 %q 与 %q", bodyA["error"], bodyB["error"])
	}
}

func TestLoginRejectsDisabledUser(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "disabled")
	u := f.mkUser(t, email, "right-password-1", store.RoleMember)

	if err := f.users.SetDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatal(err)
	}
	resp := f.login(t, email, "right-password-1")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("停用账号应返回 403，得到 %d", resp.StatusCode)
	}
}

// 停用必须立刻掐断在线会话，否则「停用」只是个显示状态。
func TestDisableKillsLiveSession(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "kick")
	u := f.mkUser(t, email, "right-password-1", store.RoleMember)

	if resp := f.login(t, email, "right-password-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应成功，得到 %d", resp.StatusCode)
	}
	if resp, _ := f.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusOK {
		t.Fatal("停用前应能访问")
	}

	if err := f.users.SetDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.st.NewSessions().DeleteUser(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}

	if resp, _ := f.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("停用后旧会话应立刻失效，得到 %d", resp.StatusCode)
	}
}

// 即使漏删了会话行，Lookup 里的 disabled_at 判定也要兜住 ——
// 防的是有人直接改库停用某人。
func TestSessionLookupRejectsDisabledUserEvenIfSessionRemains(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "dbkick")
	u := f.mkUser(t, email, "right-password-1", store.RoleMember)

	f.login(t, email, "right-password-1")
	// 只改状态，故意不删会话
	if err := f.users.SetDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("会话仍在但账号已停用，应拒绝，得到 %d", resp.StatusCode)
	}
}

func TestMemberCannotReachAdminRoutes(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "member")
	f.mkUser(t, email, "right-password-1", store.RoleMember)
	f.login(t, email, "right-password-1")

	for _, path := range []string{"/api/admin-only", "/api/admin/users"} {
		resp, _ := f.req(t, "GET", path, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s 对普通成员应 403，得到 %d", path, resp.StatusCode)
		}
	}
}

// 未改初始密码前，除改密自身外一律挡住。
func TestMustChangePasswordGate(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "mcp")
	u := f.mkUser(t, email, "initial-password-1", store.RoleMember)

	hash := u.PasswordHash
	if err := f.users.SetPassword(context.Background(), u.ID, hash, true); err != nil {
		t.Fatal(err)
	}
	f.login(t, email, "initial-password-1")

	resp, body := f.req(t, "GET", "/api/protected", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("强制改密时应挡住其它接口（409），得到 %d", resp.StatusCode)
	}
	if body["mustChangePassword"] != true {
		t.Fatal("响应应带 mustChangePassword 标记，供前端跳转")
	}

	// /api/me 在白名单里，必须放行 —— 否则前端连状态都读不到
	if resp, _ := f.req(t, "GET", "/api/me", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me 应放行，得到 %d", resp.StatusCode)
	}

	// 改密后闸门解除
	resp, body = f.req(t, "POST", "/api/password/change",
		`{"currentPassword":"initial-password-1","newPassword":"changed-password-2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改密应成功，得到 %d: %v", resp.StatusCode, body)
	}
	if resp, _ := f.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("改密后应放行，得到 %d", resp.StatusCode)
	}
}

func TestChangePasswordRequiresCurrentPassword(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "chpw")
	f.mkUser(t, email, "right-password-1", store.RoleMember)
	f.login(t, email, "right-password-1")

	resp, _ := f.req(t, "POST", "/api/password/change",
		`{"currentPassword":"wrong-password-1","newPassword":"new-password-2"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧密码不对应 401，得到 %d", resp.StatusCode)
	}
}

// 忘记密码对「存在」与「不存在」的邮箱必须给出完全相同的响应。
func TestForgotPasswordDoesNotLeakExistence(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "fp")
	f.mkUser(t, email, "right-password-1", store.RoleMember)

	respA, bodyA := f.req(t, "POST", "/api/password/forgot", `{"email":"`+email+`"}`)
	respB, bodyB := f.req(t, "POST", "/api/password/forgot",
		`{"email":"`+f.email(t, "ghost")+`"}`)

	if respA.StatusCode != http.StatusOK || respB.StatusCode != http.StatusOK {
		t.Fatalf("两者都应 200，得到 %d 与 %d", respA.StatusCode, respB.StatusCode)
	}
	if bodyA["status"] != bodyB["status"] || len(bodyA) != len(bodyB) {
		t.Fatalf("两者响应体必须一致，得到 %v 与 %v", bodyA, bodyB)
	}

	// 存在的那个应当真的发了信，不存在的不该发
	select {
	case m := <-f.mail.sent:
		if m.to != email {
			t.Fatalf("信应发给 %s，实际 %s", email, m.to)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("应当发出一封重置邮件")
	}
	select {
	case m := <-f.mail.sent:
		t.Fatalf("不存在的邮箱不该发信，却发给了 %s", m.to)
	case <-time.After(300 * time.Millisecond):
	}
}

// 完整链路：发信 → 从正文抽链接 → 重置 → 新密码可用、旧密码失效、令牌不可复用。
func TestResetPasswordFlow(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "reset")
	f.mkUser(t, email, "old-password-1", store.RoleMember)

	f.req(t, "POST", "/api/password/forgot", `{"email":"`+email+`"}`)

	var mail sentMail
	select {
	case mail = <-f.mail.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到重置邮件")
	}

	// 链接必须用配置的 BaseURL 拼，而不是从请求 Host 推导 ——
	// 后者可被伪造，会把令牌送到攻击者的域名
	if !regexp.MustCompile(`https://lathe\.test/reset-password\?token=`).MatchString(mail.body) {
		t.Fatalf("邮件里的链接前缀不对: %s", mail.body)
	}
	tok := regexp.MustCompile(`token=([0-9a-f]{64})`).FindStringSubmatch(mail.body)
	if tok == nil {
		t.Fatalf("邮件里找不到重置令牌: %s", mail.body)
	}

	resp, body := f.req(t, "POST", "/api/password/reset",
		`{"token":"`+tok[1]+`","password":"brand-new-password-3"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重置应成功，得到 %d: %v", resp.StatusCode, body)
	}

	// 同一令牌第二次必须失败
	resp, _ = f.req(t, "POST", "/api/password/reset",
		`{"token":"`+tok[1]+`","password":"yet-another-password-4"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("令牌只能用一次，第二次应 400，得到 %d", resp.StatusCode)
	}

	if resp := f.login(t, email, "brand-new-password-3"); resp.StatusCode != http.StatusOK {
		t.Fatalf("新密码应能登录，得到 %d", resp.StatusCode)
	}
	if resp := f.login(t, email, "old-password-1"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效，得到 %d", resp.StatusCode)
	}
}

func TestResetTokenExpires(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "expire")
	u := f.mkUser(t, email, "old-password-1", store.RoleMember)

	// 正常签发后把过期时间改到过去。
	// 不能用负的 TTL —— Create 把 ttl<=0 当作「用默认值」，那样测的是空气。
	if err := f.st.NewResets().Create(context.Background(), u.ID, "expired-token-xyz", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool().Exec(context.Background(),
		`UPDATE password_reset_tokens SET expires_at = now() - interval '1 minute'
		 WHERE user_id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}

	resp, _ := f.req(t, "POST", "/api/password/reset",
		`{"token":"expired-token-xyz","password":"brand-new-password-3"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("过期令牌应 400，得到 %d", resp.StatusCode)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "logout")
	f.mkUser(t, email, "right-password-1", store.RoleMember)
	f.login(t, email, "right-password-1")

	if resp, _ := f.req(t, "POST", "/api/logout", ""); resp.StatusCode != http.StatusOK {
		t.Fatal("登出应成功")
	}
	if resp, _ := f.req(t, "GET", "/api/protected", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后应 401，得到 %d", resp.StatusCode)
	}
}

// hasLinearToken 是「Linear 任务」菜单的显隐依据，登录响应与 /api/me
// 都得如实带出 —— 前端在登录当刻就要渲染菜单，等不到第二次请求。
func TestMeReportsLinearTokenBinding(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "lintok")
	f.mkUser(t, email, "right-password-1", store.RoleMember)

	// 未注入判定函数时按未绑定处理（比如单测里的裸 Auth）
	f.login(t, email, "right-password-1")
	_, body := f.req(t, "GET", "/api/me", "")
	user, _ := body["user"].(map[string]any)
	if got, _ := user["hasLinearToken"].(bool); got {
		t.Fatal("未注入 HasLinearToken 时应报告未绑定")
	}

	f.auth.HasLinearToken = func(_ context.Context, userID int64) bool { return userID > 0 }
	resp, body := f.req(t, "GET", "/api/me", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me 应 200，得到 %d", resp.StatusCode)
	}
	user, _ = body["user"].(map[string]any)
	if got, _ := user["hasLinearToken"].(bool); !got {
		t.Fatal("已绑定时令牌标志应为 true")
	}

	// 登录响应同样要带 —— 前端登录后直接用这份 user 渲染菜单
	f.auth.HasLinearToken = func(context.Context, int64) bool { return true }
	resp, body = f.req(t, "POST", "/api/login",
		`{"email":"`+email+`","password":"right-password-1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应成功，得到 %d", resp.StatusCode)
	}
	user, _ = body["user"].(map[string]any)
	if got, _ := user["hasLinearToken"].(bool); !got {
		t.Fatal("登录响应也应带 hasLinearToken")
	}
}

func TestSetNotifyEmail(t *testing.T) {
	f := newAccountFixture(t)
	email := f.email(t, "notify")
	f.mkUser(t, email, "right-password-1", store.RoleMember)
	f.login(t, email, "right-password-1")

	// 初始未设置：/api/me 应给 null，语义是回退登录邮箱
	_, body := f.req(t, "GET", "/api/me", "")
	user, _ := body["user"].(map[string]any)
	if v, ok := user["notifyEmail"]; !ok || v != nil {
		t.Fatalf("初始通知邮箱应为 null，得到 %v (ok=%v)", v, ok)
	}

	// 设置合法地址
	resp, body := f.req(t, "PUT", "/api/me/notify-email", `{"email":"Alerts@Example.com"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("设置通知邮箱应 200，得到 %d: %v", resp.StatusCode, body)
	}
	user, _ = body["user"].(map[string]any)
	if got, _ := user["notifyEmail"].(string); got != "alerts@example.com" {
		t.Fatalf("通知邮箱应归一化为小写，得到 %q", got)
	}

	// 落库后再查 /api/me 仍在 —— 不是只改了响应形状
	_, body = f.req(t, "GET", "/api/me", "")
	user, _ = body["user"].(map[string]any)
	if got, _ := user["notifyEmail"].(string); got != "alerts@example.com" {
		t.Fatalf("通知邮箱应持久化，得到 %q", got)
	}

	// 非法地址 400，且不许顺手清掉旧值
	resp, _ = f.req(t, "PUT", "/api/me/notify-email", `{"email":"not-an-email"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法邮箱应 400，得到 %d", resp.StatusCode)
	}
	_, body = f.req(t, "GET", "/api/me", "")
	user, _ = body["user"].(map[string]any)
	if got, _ := user["notifyEmail"].(string); got != "alerts@example.com" {
		t.Fatalf("失败后不应改动旧值，得到 %q", got)
	}

	// 空串清除，回退登录邮箱
	resp, body = f.req(t, "PUT", "/api/me/notify-email", `{"email":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清除通知邮箱应 200，得到 %d", resp.StatusCode)
	}
	user, _ = body["user"].(map[string]any)
	if v := user["notifyEmail"]; v != nil {
		t.Fatalf("清除后应为 null，得到 %v", v)
	}
}

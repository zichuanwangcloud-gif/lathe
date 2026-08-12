package linear

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestVerifySuccess(t *testing.T) {
	var gotQuery string

	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		gotQuery = string(body[:n])
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"user-uuid-1","name":"张子川","email":"z@example.com"}}}`))
	})

	v, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if v.ID != "user-uuid-1" || v.Name != "张子川" || v.Email != "z@example.com" {
		t.Errorf("账号信息不符: %+v", v)
	}
	if !strings.Contains(gotQuery, "viewer") {
		t.Errorf("应查询 viewer: %s", gotQuery)
	}
}

// 验证时拿到的 ID 正是「只接指派给我的 issue」所需的用户标识，
// 这样就不用让人去 Linear 里手工翻自己的 user id。
func TestVerifyReturnsUserIDForAssignmentFilter(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"the-user-id","name":"n","email":"e"}}}`))
	})

	v, err := c.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.ID == "" {
		t.Fatal("必须返回用户 ID —— 接单判定要用它")
	}

	// 这个 ID 应能直接喂给 IsAssignedTo
	ev := &WebhookEvent{Action: "create", Type: "Issue"}
	ev.Data.AssigneeID = v.ID
	if !ev.IsAssignedTo(v.ID) {
		t.Error("验证得到的 ID 应能直接用于接单判定")
	}
}

func TestVerifyInvalidToken(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
	})

	_, err := c.Verify(context.Background())
	if err == nil {
		t.Fatal("无效令牌应报错")
	}
	if !strings.Contains(err.Error(), "Personal API keys") {
		t.Errorf("错误应给出可操作的指引，得到: %v", err)
	}
}

func TestVerifyEmptyViewer(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":null}}`))
	})
	if _, err := c.Verify(context.Background()); err == nil {
		t.Error("viewer 为空应报错，而不是当作验证通过")
	}
}

func TestClassifyVerifyError(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"linear: GraphQL 错误: Authentication required", "Personal API keys"},
		{"linear: HTTP 401: unauthorized", "Personal API keys"},
		{"linear: 请求失败: dial tcp: no such host", "连不上"},
		{"linear: 请求失败: context deadline exceeded", "连不上"},
	}
	for _, tc := range cases {
		got := classifyVerifyError(errString(tc.in))
		if !strings.Contains(got.Error(), tc.want) {
			t.Errorf("classify(%q) = %v，期望含 %q", tc.in, got, tc.want)
		}
	}

	// 无法归类的错误应原样透出，不要丢失信息
	other := errString("linear: 某个没见过的错误")
	if !strings.Contains(classifyVerifyError(other).Error(), "没见过") {
		t.Error("未知错误应原样透出")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gqlReq 是测试桩收到的一次 GraphQL 请求。
type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// stubLinear 起一个只回固定载荷的 GraphQL 桩，并记录收到的请求。
func stubLinear(t *testing.T, status int, payload string) (*Client, *[]gqlReq) {
	t.Helper()
	var got []gqlReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var q gqlReq
		_ = json.Unmarshal(raw, &q)
		got = append(got, q)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClientWithURL("tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c, &got
}

func TestCreateIssue(t *testing.T) {
	const okPayload = `{"data":{"issueCreate":{"success":true,"issue":{"id":"0b0e6b3a-1111-4222-8333-444455556666","identifier":"ENG-7","url":"https://linear.app/x/issue/ENG-7"}}}}`

	cases := []struct {
		name         string
		in           CreateIssueInput
		wantAssignee bool
		wantLabels   bool
	}{
		{"assign_mode", CreateIssueInput{ID: "0b0e6b3a-1111-4222-8333-444455556666", TeamID: "team-1",
			Title: "每周依赖升级", Description: "正文", Priority: 2, AssigneeID: "user-1"}, true, false},
		{"label_mode", CreateIssueInput{ID: "0b0e6b3a-1111-4222-8333-444455556666", TeamID: "team-1",
			Title: "每周依赖升级", Description: "正文", Priority: 0, LabelIDs: []string{"label-1"}}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, reqs := stubLinear(t, http.StatusOK, okPayload)
			got, err := c.CreateIssue(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("CreateIssue: %v", err)
			}
			if got.Identifier != "ENG-7" || got.URL != "https://linear.app/x/issue/ENG-7" || got.ID != tc.in.ID {
				t.Errorf("解析结果: got %+v, want identifier=ENG-7 url=… id=%s", got, tc.in.ID)
			}
			if len(*reqs) != 1 {
				t.Fatalf("请求次数: got %d, want 1", len(*reqs))
			}
			q := (*reqs)[0]
			if !strings.Contains(q.Query, "issueCreate") {
				t.Errorf("应调用 issueCreate，got query %q", q.Query)
			}
			input, ok := q.Variables["input"].(map[string]any)
			if !ok {
				t.Fatalf("variables.input 缺失: %v", q.Variables)
			}
			for _, k := range []string{"id", "teamId", "title", "description", "priority"} {
				if _, ok := input[k]; !ok {
					t.Errorf("input 缺字段 %s: got %v", k, input)
				}
			}
			if input["id"] != tc.in.ID {
				t.Errorf("input.id: got %v, want %s", input["id"], tc.in.ID)
			}
			if _, has := input["assigneeId"]; has != tc.wantAssignee {
				t.Errorf("assigneeId 出现: got %v, want %v", has, tc.wantAssignee)
			}
			if _, has := input["labelIds"]; has != tc.wantLabels {
				t.Errorf("labelIds 出现: got %v, want %v", has, tc.wantLabels)
			}
		})
	}

	t.Run("success_false_is_error", func(t *testing.T) {
		c, _ := stubLinear(t, http.StatusOK, `{"data":{"issueCreate":{"success":false,"issue":null}}}`)
		if _, err := c.CreateIssue(context.Background(), CreateIssueInput{ID: "x", TeamID: "t", Title: "a"}); err == nil {
			t.Error("success=false 应返回错误，got nil")
		}
	})
}

func TestTeams(t *testing.T) {
	c, _ := stubLinear(t, http.StatusOK, `{"data":{"teams":{"nodes":[{"id":"t1","key":"ENG","name":"Engineering"},{"id":"t2","key":"OPS","name":"Ops"}]}}}`)
	got, err := c.Teams(context.Background())
	if err != nil {
		t.Fatalf("Teams: %v", err)
	}
	if len(got) != 2 || got[0] != (Team{ID: "t1", Key: "ENG", Name: "Engineering"}) {
		t.Errorf("got %+v, want 两个团队且第一个是 ENG", got)
	}
}

func TestLabelIDByName(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
		wantErr bool
	}{
		{"team_label_wins_over_workspace",
			`{"data":{"issueLabels":{"nodes":[{"id":"ws","name":"lathe:go","team":null},{"id":"other","name":"lathe:go","team":{"id":"t9"}},{"id":"mine","name":"lathe:go","team":{"id":"t1"}}]}}}`,
			"mine", false},
		{"workspace_label_fallback",
			`{"data":{"issueLabels":{"nodes":[{"id":"other","name":"lathe:go","team":{"id":"t9"}},{"id":"ws","name":"lathe:go","team":null}]}}}`,
			"ws", false},
		{"not_found", `{"data":{"issueLabels":{"nodes":[{"id":"other","name":"lathe:go","team":{"id":"t9"}}]}}}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, reqs := stubLinear(t, http.StatusOK, tc.payload)
			got, err := c.LabelIDByName(context.Background(), "t1", "lathe:go")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err: got %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("label id: got %q, want %q", got, tc.want)
			}
			if len(*reqs) != 1 || (*reqs)[0].Variables["name"] != "lathe:go" {
				t.Errorf("请求: got %v, want 1 次且 variables.name=lathe:go", *reqs)
			}
		})
	}
}

func TestIssueByID(t *testing.T) {
	const id = "0b0e6b3a-1111-4222-8333-444455556666"
	cases := []struct {
		name      string
		status    int
		payload   string
		wantFound bool
		wantErr   bool
		errHas    string
	}{
		{"found", 200, `{"data":{"issue":{"id":"` + id + `","identifier":"ENG-1","url":"https://linear.app/x/issue/ENG-1"}}}`, true, false, ""},
		{"not_found_entity_code", 200, `{"data":{"issue":null},"errors":[{"message":"Entity not found: Issue","extensions":{"code":"ENTITY_NOT_FOUND"}}]}`, false, false, ""},
		{"not_found_invalid_input", 200, `{"data":{"issue":null},"errors":[{"message":"Entity not found: Issue - Could not find referenced Issue.","extensions":{"type":"invalid input"}}]}`, false, false, ""},
		{"null_without_errors", 200, `{"data":{"issue":null}}`, false, false, ""},
		{"http_500", 500, `boom`, false, true, "HTTP 500"},
		{"graphql_auth_error", 200, `{"data":{"issue":null},"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`, false, true, "AUTHENTICATION_ERROR"},
		{"graphql_mixed", 200, `{"data":{"issue":null},"errors":[{"message":"Entity not found: Issue","extensions":{"code":"ENTITY_NOT_FOUND"}},{"message":"slow down","extensions":{"code":"RATELIMITED"}}]}`, false, true, "RATELIMITED"},
		{"graphql_no_extensions", 200, `{"errors":[{"message":"Something went wrong"}]}`, false, true, "Something went wrong"},
		{"bad_json", 200, `<html>nope</html>`, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, reqs := stubLinear(t, tc.status, tc.payload)
			got, err := c.IssueByID(context.Background(), id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err: got %v, wantErr %v", err, tc.wantErr)
			}
			if tc.errHas != "" && err != nil && !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("err 串应含 %q，got %q", tc.errHas, err.Error())
			}
			if (got != nil) != tc.wantFound {
				t.Fatalf("found: got %+v, want found=%v", got, tc.wantFound)
			}
			if tc.wantFound {
				want := CreatedIssue{ID: id, Identifier: "ENG-1", URL: "https://linear.app/x/issue/ENG-1"}
				if *got != want {
					t.Errorf("got %+v, want %+v", *got, want)
				}
			}
			if len(*reqs) != 1 || (*reqs)[0].Variables["id"] != id {
				t.Errorf("请求变量 id: got %v, want %s", *reqs, id)
			}
		})
	}
}

// 定时建单产生的 webhook 形状必须命中现有接单闸门（AC-11）：
// 建单时直接带 assigneeId = 属主自己，或带系统接单标签。
func TestIsAssignedToMatchesScheduledCreatePayload(t *testing.T) {
	body := []byte(`{"action":"create","type":"Issue","data":{"id":"0b0e6b3a-1111-4222-8333-444455556666","identifier":"ENG-7","title":"每周依赖升级","assigneeId":"viewer-1","labels":[{"id":"l1","name":"lathe:go"}],"labelIds":["l1"]}}`)
	ev, err := ParseWebhook("s", body, sign(t, "s", body))
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsAssignedTo("viewer-1") {
		t.Error("assign 模式：action=create + assigneeId=本人 应命中 IsAssignedTo")
	}
	if !ev.IsLabelTriggered("lathe:go") {
		t.Error("label 模式：action=create + 带接单标签 应命中 IsLabelTriggered")
	}
}

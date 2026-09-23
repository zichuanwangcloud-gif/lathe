package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// testStoreForAPI 连接测试库；本地连不上就跳过，CI 下（LATHE_TEST_REQUIRE_DB）直接失败。
func testStoreForAPI(t *testing.T) *store.Store {
	t.Helper()

	ctx, cancel := testsupport.ConnectContext(t)
	defer cancel()

	st, err := store.Open(ctx, testsupport.DSN())
	if err != nil {
		testsupport.SkipOrFail(t, "打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// httptestServer 把测试服务器与被测 API 绑在一起，简化请求写法。
type httptestServer struct {
	srv *httptest.Server
	api *CredentialAPI
	// store 与 userID 供多用户隔离测试构造「另一个人的视角」。
	store  *store.Store
	userID int64
}

func newTestServer(t *testing.T, mux *http.ServeMux) *httptestServer {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &httptestServer{srv: srv}
}

func (s *httptestServer) do(t *testing.T, method, path, body string, auth bool) *http.Response {
	t.Helper()

	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, s.srv.URL+path, nil)
	} else {
		r, err = http.NewRequest(method, s.srv.URL+path, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		r.Header.Set("Authorization", "Bearer "+apiTestToken)
	}

	resp, err := s.srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (s *httptestServer) decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return v
}

func (s *httptestServer) raw(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return string(b)
}

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/store"
)

// testStoreForAPI 连接测试库；连不上则跳过。
func testStoreForAPI(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（先 make dev-infra && make migrate）: %v", err)
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

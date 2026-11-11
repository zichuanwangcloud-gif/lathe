package yunxiao

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// captureSlog 把默认 logger 换成写进缓冲区的 Debug 级 JSON handler。
func captureSlog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// AC-22（客户端部分）
func TestYunxiaoClientNoSecretInLogs(t *testing.T) {
	t.Run("error_paths", func(t *testing.T) {
		logs := captureSlog(t)
		ctx := context.Background()
		for _, status := range []int{200, 401, 403, 404, 429, 500, 418, 302} {
			srv, _ := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
				if status == 302 {
					w.Header().Set("Location", "http://127.0.0.1:1/"+sentinel)
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `["`+sentinel+`"]`)
			})
			c := testClient(t, srv, "ORG")
			_, _ = c.GetWorkitem(ctx, "WI")
			_, _ = c.ListComments(ctx, "WI")
			_, _ = c.CreateComment(ctx, "WI", "body")
			_, _ = c.CurrentUser(ctx)
		}
		out := logs()
		if out == "" {
			t.Fatal("没捕获到任何日志：错误路径至少应有 Debug 级的响应长度记录")
		}
		for _, frag := range []string{testToken, sentinel} {
			if strings.Contains(out, frag) {
				t.Errorf("日志含敏感片段 %q:\n%s", frag, out)
			}
		}
	})
}

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// AC-5：验证日志读取 —— 归属、穿越、脱敏、分块、重试覆盖。

type logFixture struct {
	api    *API
	url    string
	tk     *task.Task
	m      *task.Machine
	repoID int64
	dir    string
}

func newLogFixture(t *testing.T, secrets ...string) *logFixture {
	t.Helper()
	api, _, m, repoID := apiFixture(t)
	api.LogDir = t.TempDir()
	api.KnownSecrets = secrets
	srv := apiServer(t, api)
	tk := replayTask(t, api, m, repoID, "f04-ac5")
	return &logFixture{api: api, url: srv.URL, tk: tk, m: m, repoID: repoID, dir: api.LogDir}
}

// rel 返回本任务日志目录下的相对路径（与 fileStepLogger 的真实格式一致）。
func (f *logFixture) rel(round int, name string) string {
	return "verify-logs/task-" + itoa(f.tk.ID) + "/round-" + strconv.Itoa(round) + "/" + name
}

func (f *logFixture) write(t *testing.T, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// insertVerification 直接插一条验证行（log_ref 可为 nil），返回 id 与库里的 at。
func insertVerification(t *testing.T, api *API, taskID int64, step, status string, logRef *string) (int64, time.Time) {
	t.Helper()
	var id int64
	var at time.Time
	if err := api.Store.Pool().QueryRow(context.Background(),
		`INSERT INTO verifications (task_id, tier, step, status, duration_ms, log_ref)
		 VALUES ($1, 'heavy', $2, $3, 1, $4) RETURNING id, at`,
		taskID, step, status, logRef).Scan(&id, &at); err != nil {
		t.Fatalf("插验证行失败: %v", err)
	}
	return id, at
}

func strp(s string) *string { return &s }

func (f *logFixture) get(t *testing.T, vid int64, query string) (int, map[string]any, string) {
	t.Helper()
	return getLog(t, f.url, f.tk.ID, vid, query)
}

func getLog(t *testing.T, base string, taskID, vid int64, query string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/tasks/"+itoa(taskID)+"/verifications/"+itoa(vid)+"/log"+query, nil)
	req.Header.Set("Authorization", "Bearer "+apiTestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body, string(raw)
}

// readAll 沿 nextOffset 把一段日志读到 eof，返回每一块的 text 与对应 nextOffset。
func (f *logFixture) readAll(t *testing.T, vid int64, extra string) (chunks []string, nexts []int64) {
	t.Helper()
	offset := int64(0)
	for i := 0; i < 200; i++ {
		code, body, raw := f.get(t, vid, "?offset="+strconv.FormatInt(offset, 10)+extra)
		if code != http.StatusOK {
			t.Fatalf("offset=%d 状态码 got %d, want 200: %s", offset, code, raw)
		}
		chunks = append(chunks, body["text"].(string))
		if body["eof"] == true {
			return chunks, nexts
		}
		n, ok := body["nextOffset"].(float64)
		if !ok {
			t.Fatalf("非 eof 却没有 nextOffset: %s", raw)
		}
		nexts = append(nexts, int64(n))
		offset = int64(n)
	}
	t.Fatal("读不到 eof")
	return nil, nil
}

func TestVerificationLogNormalChunks(t *testing.T) {
	f := newLogFixture(t, "f04RegisteredSecretValue-"+rhex(t))
	var b strings.Builder
	for i := 0; b.Len() < 300<<10; i++ {
		b.WriteString("第 " + strconv.Itoa(i) + " 行：编译输出 ok —— 中文多字节字符混排 abcdefghij\n")
	}
	data := b.String()
	ref := f.rel(0, "01-build.log")
	f.write(t, ref, []byte(data))
	vid, _ := insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))

	chunks, nexts := f.readAll(t, vid, "&limit=65536")
	if len(chunks) < 4 {
		t.Fatalf("300KiB / 64KiB 应至少 4 块, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Errorf("第 %d 块不是合法 UTF-8", i)
		}
	}
	_ = nexts // 块终点落在换行之后是安全切点的性质，提交 B 起断言
	if got, want := strings.Join(chunks, ""), runner.RedactForDisplay(data, f.api.KnownSecrets); got != want {
		t.Errorf("逐块拼接 != 整份脱敏: got len %d, want len %d", len(got), len(want))
	}
}

func TestVerificationLogMultiPart(t *testing.T) {
	f := newLogFixture(t)
	a, b := f.rel(0, "09-repro_fail.log"), f.rel(0, "10-repro_fail.log")
	f.write(t, a, []byte("PART-A"))
	f.write(t, b, []byte("PART-B"))
	vid, _ := insertVerification(t, f.api, f.tk.ID, "repro_fail", "passed", strp(a+" "+b))

	for _, c := range []struct {
		part, want string
	}{{"0", "PART-A"}, {"1", "PART-B"}} {
		t.Run("给定 part="+c.part+" 断言 text="+c.want+" 且 parts=2", func(t *testing.T) {
			code, body, raw := f.get(t, vid, "?part="+c.part)
			if code != http.StatusOK || body["text"] != c.want || body["parts"] != float64(2) {
				t.Errorf("got %d %s, want 200 text=%s parts=2", code, raw, c.want)
			}
		})
	}
	t.Run("给定 part=2 越界 断言 404", func(t *testing.T) {
		if code, _, raw := f.get(t, vid, "?part=2"); code != http.StatusNotFound {
			t.Errorf("got %d %s, want 404", code, raw)
		}
	})
	t.Run("给定多段都不在 verify-logs/task-<id>/ 下 断言每段 404", func(t *testing.T) {
		f.write(t, "a.log", []byte("A-CONTENT"))
		f.write(t, "b.log", []byte("B-CONTENT"))
		bad, _ := insertVerification(t, f.api, f.tk.ID, "repro_fail", "passed", strp("a.log b.log"))
		for _, p := range []string{"0", "1"} {
			if code, _, raw := f.get(t, bad, "?part="+p); code != http.StatusNotFound || strings.Contains(raw, "CONTENT") {
				t.Errorf("part=%s got %d %s, want 404 且不含内容", p, code, raw)
			}
		}
	})
}

func TestVerificationLogRedactsSecrets(t *testing.T) {
	reg := "f04-registered-" + rhex(t) + rhex(t)
	f := newLogFixture(t, reg)
	ref := f.rel(0, "01-build.log")
	f.write(t, ref, []byte("token is "+reg+"\nconnect postgres://u:p4ss@h/db failed\n"))
	vid, _ := insertVerification(t, f.api, f.tk.ID, "build", "failed", strp(ref))
	code, _, raw := f.get(t, vid, "")
	if code != http.StatusOK {
		t.Fatalf("got %d %s", code, raw)
	}
	for _, sec := range []string{reg, "p4ss@h"} {
		if strings.Contains(raw, sec) {
			t.Errorf("响应含明文 %q: %s", sec, raw)
		}
	}
}

func TestVerificationLogPathTraversal(t *testing.T) {
	f := newLogFixture(t)
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := f.rel(0, "01-build.log")
	if err := os.MkdirAll(filepath.Join(f.dir, filepath.Dir(link)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.dir, link)); err != nil {
		t.Fatal(err)
	}
	f.write(t, "verify-logs/task-999999999/x.log", []byte("OTHER-TASK-CONTENT"))

	for _, ref := range []string{
		"../../../etc/passwd",
		"/etc/passwd",
		"verify-logs/task-999999999/x.log",
		"verify-logs/task-" + itoa(f.tk.ID) + "/../task-999999999/x.log",
		link, // 符号链接指向 LogDir 之外
	} {
		t.Run("给定 log_ref="+ref+" 断言 404 且不含目标内容", func(t *testing.T) {
			vid, _ := insertVerification(t, f.api, f.tk.ID, "build", "failed", strp(ref))
			code, _, raw := f.get(t, vid, "")
			if code != http.StatusNotFound {
				t.Errorf("got %d, want 404", code)
			}
			for _, leak := range []string{"root:", "OUTSIDE-CONTENT", "OTHER-TASK-CONTENT"} {
				if strings.Contains(raw, leak) {
					t.Errorf("响应含目标文件内容 %q: %s", leak, raw)
				}
			}
		})
	}
}

func TestVerificationLogOwnership(t *testing.T) {
	f := newLogFixture(t)
	ref := f.rel(0, "01-build.log")
	f.write(t, ref, []byte("MINE"))
	vid, _ := insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))

	t.Run("给定另一个用户 断言 404 任务不存在", func(t *testing.T) {
		userB := mustUser(t, f.api.Store, "f04-ac5-b-"+rhex(t)+"@example.com")
		apiB := &API{Store: f.api.Store, Tasks: f.m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com"), LogDir: f.dir}
		srvB := apiServer(t, apiB)
		code, _, raw := getLog(t, srvB.URL, f.tk.ID, vid, "")
		if code != http.StatusNotFound || strings.TrimSpace(raw) != `{"error":"任务不存在"}` {
			t.Errorf("got %d %s, want 404 {\"error\":\"任务不存在\"}", code, raw)
		}
	})
	t.Run("给定 vid 属于另一个任务 断言 404", func(t *testing.T) {
		other := replayTask(t, f.api, f.m, f.repoID, "f04-ac5x")
		code, _, raw := getLog(t, f.url, other.ID, vid, "")
		if code != http.StatusNotFound || strings.Contains(raw, "MINE") {
			t.Errorf("got %d %s, want 404", code, raw)
		}
	})
	t.Run("给定 log_ref IS NULL 断言 404", func(t *testing.T) {
		nv, _ := insertVerification(t, f.api, f.tk.ID, "lint", "skipped", nil)
		if code, _, raw := f.get(t, nv, ""); code != http.StatusNotFound {
			t.Errorf("got %d %s, want 404", code, raw)
		}
	})
	t.Run("给定 LogDir 为空 断言 503", func(t *testing.T) {
		api2 := *f.api
		api2.LogDir = ""
		srv2 := apiServer(t, &api2)
		if code, _, raw := getLog(t, srv2.URL, f.tk.ID, vid, ""); code != http.StatusServiceUnavailable {
			t.Errorf("got %d %s, want 503", code, raw)
		}
	})
	t.Run("给定 limit=10000000 断言单块不超过 256KiB+1MiB", func(t *testing.T) {
		big := f.rel(0, "02-lint.log")
		f.write(t, big, []byte(strings.Repeat("0123456789abcdef\n", 200000)))
		bv, _ := insertVerification(t, f.api, f.tk.ID, "lint", "passed", strp(big))
		code, body, _ := f.get(t, bv, "?offset=0&limit=10000000")
		n, _ := body["nextOffset"].(float64)
		if code != http.StatusOK || n <= 0 || int(n) > 256<<10+1<<20 {
			t.Errorf("got %d nextOffset=%v, want 200 且 0 < nextOffset ≤ %d", code, body["nextOffset"], 256<<10+1<<20)
		}
	})
}

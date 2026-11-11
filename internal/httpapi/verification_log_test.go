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
	for i, n := range nexts {
		if data[n-1] != '\n' {
			t.Errorf("第 %d 块的 nextOffset=%d 前一个字节 got %q, want '\\n'", i, n, data[n-1])
		}
	}
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

// 切分场景：块边界只能落在安全切点上，任何一块都不得出现半截凭据。
func TestVerificationLogChunkNeverSplitsSecret(t *testing.T) {
	const boundary = 65536
	filler := func(n int) string { // n 字节、以 \n 结尾的填充行
		var b strings.Builder
		for b.Len() < n {
			line := "filler line for chunk boundary testing ................................\n"
			if b.Len()+len(line) > n {
				line = strings.Repeat("f", n-b.Len()-1) + "\n"
			}
			b.WriteString(line)
		}
		return b.String()
	}
	tail := filler(4096)

	type tc struct {
		name   string
		build  func(r string) (data, secret string, reg []string)
		assert func(t *testing.T, f *logFixture, vid int64, data, sec string)
	}
	checkHalves := func(t *testing.T, f *logFixture, vid int64, data, sec string) {
		t.Helper()
		chunks, _ := f.readAll(t, vid, "&limit=65536")
		joined := strings.Join(chunks, "")
		if strings.Contains(joined, sec) {
			t.Errorf("拼接结果含凭据明文 %q", sec)
		}
		h1, h2 := sec[:len(sec)/2], sec[len(sec)/2:]
		for i, c := range chunks {
			if strings.Contains(c, h1) || strings.Contains(c, h2) {
				t.Errorf("第 %d 块含半截凭据: got 含 %q 或 %q, want 都不含", i, h1, h2)
			}
		}
		if want := runner.RedactForDisplay(data, f.api.KnownSecrets); joined != want {
			t.Errorf("逐块拼接 != 整份脱敏（got len %d, want len %d）", len(joined), len(want))
		}
	}
	cases := []tc{
		{
			name: "给定登记凭据 DSN 横跨 64KiB 标称边界 断言无半截",
			build: func(r string) (string, string, []string) {
				dsn := "postgres://u:" + r + r + "@db.internal:5432/app"
				line := dsn + " " + strings.Repeat("x", 200-len(dsn)-2) + "\n"
				return filler(65520) + line + tail, dsn, []string{dsn}
			},
			assert: checkHalves,
		},
		{
			name: "给定凭据所在行超过 4KiB 的单行 JSON 断言无半截",
			build: func(r string) (string, string, []string) {
				tok := r + r + r[:8]
				pre := filler(boundary - 5000)
				head := `{"a":"` + strings.Repeat("a", 5000-len(`{"a":"`)-len(`","token":"`)-10) + `","token":"`
				line := head + tok + `","b":"` + strings.Repeat("b", 10<<10-len(head)-len(tok)-10) + `"}` + "\n"
				return pre + line + tail, tok, nil
			},
			assert: checkHalves,
		},
		{
			name: "给定裸值 API_KEY= 横跨边界 断言无半截",
			build: func(r string) (string, string, []string) {
				key := r + r + r[:8]
				pre := filler(boundary - 20)
				return pre + "API_KEY=" + key + "\n" + tail, key, nil
			},
			assert: checkHalves,
		},
		{
			name: "给定跨行形态 password:\\n 换行正好在边界后 断言首块不在该换行处结束",
			build: func(r string) (string, string, []string) {
				sec := r + r[:4]
				pre := filler(boundary - 8) // password: 从 65528 起，\n 落在 65537
				return pre + "password:\n  " + sec + "\n" + tail, sec, nil
			},
			assert: func(t *testing.T, f *logFixture, vid int64, data, sec string) {
				_, body, raw := f.get(t, vid, "?offset=0&limit=65536")
				if n, ok := body["nextOffset"].(float64); ok && int(n) == boundary+2 {
					t.Errorf("首块在 password:\\n 之后结束（nextOffset=%d），这会把跨行凭据切开: %.200s", int(n), raw)
				}
				checkHalves(t, f, vid, data, sec)
			},
		},
		{
			name: "给定超过 1MiB 的单行 断言 withheld 且不返回任何片段",
			build: func(r string) (string, string, []string) {
				sec := "f04-long-line-secret-" + r
				half := strings.Repeat("z", 768<<10)
				return "hello\n" + half + sec + half + "\n" + tail, sec, []string{sec}
			},
			assert: func(t *testing.T, f *logFixture, vid int64, data, sec string) {
				code, body, raw := f.get(t, vid, "?offset=0&limit=65536")
				if code != http.StatusOK || body["withheld"] != true || body["text"] != "" || body["nextOffset"] != nil {
					t.Fatalf("got %d withheld=%v text.len=%d nextOffset=%v, want 200 withheld=true text=\"\" nextOffset=null",
						code, body["withheld"], len(asString(body["text"])), body["nextOffset"])
				}
				if strings.Contains(raw, strings.Repeat("z", 64)) || strings.Contains(raw, sec[:16]) {
					t.Errorf("withheld 响应含该行片段: %.300s", raw)
				}
			},
		},
		{
			name: "给定 offset 指向凭据中间 断言 400 且只含错误信息",
			build: func(r string) (string, string, []string) {
				key := r + r + r[:8]
				return filler(1000) + "API_KEY=" + key + "\n" + tail, key, nil
			},
			assert: func(t *testing.T, f *logFixture, vid int64, data, sec string) {
				mid := strings.Index(data, sec) + len(sec)/2
				code, body, raw := f.get(t, vid, "?offset="+strconv.Itoa(mid))
				if code != http.StatusBadRequest {
					t.Fatalf("got %d %.200s, want 400", code, raw)
				}
				if _, has := body["text"]; has || strings.Contains(raw, sec[len(sec)/2:]) {
					t.Errorf("400 响应不应带内容: %s", raw)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := rhex(t) + rhex(t) // 24 字节随机串
			data, sec, reg := c.build(r)
			f := newLogFixture(t, reg...)
			ref := f.rel(0, "01-build.log")
			f.write(t, ref, []byte(data))
			vid, _ := insertVerification(t, f.api, f.tk.ID, "build", "failed", strp(ref))
			c.assert(t, f, vid, data, sec)
		})
	}
}

func asString(v any) string { s, _ := v.(string); return s }

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

// 重试复用任务行：后一次运行写回同名日志文件，旧 vid 不得读到新内容（铁律 1）。
func TestVerificationLogRejectsLogOverwrittenByLaterRun(t *testing.T) {
	isOverwritten := func(code int, body map[string]any) bool {
		return code == http.StatusNotFound && body["code"] == "log_overwritten"
	}

	t.Run("给定后一次运行已插行 断言旧 vid 404 log_overwritten、新 vid 200", func(t *testing.T) {
		f := newLogFixture(t)
		ref := f.rel(0, "01-build.log")
		f.write(t, ref, []byte("RUN-1"))
		vid1, _ := insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))
		f.write(t, ref, []byte("RUN-2"))
		vid2, _ := insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))

		code, body, raw := f.get(t, vid1, "")
		if !isOverwritten(code, body) || strings.Contains(raw, "RUN-2") {
			t.Errorf("vid1 got %d %s, want 404 log_overwritten 且不含 RUN-2", code, raw)
		}
		code, body, raw = f.get(t, vid2, "")
		if code != http.StatusOK || body["text"] != "RUN-2" {
			t.Errorf("vid2 got %d %s, want 200 RUN-2", code, raw)
		}
	})

	mtimeCase := func(name string, delta time.Duration, wantOverwritten bool) {
		t.Run(name, func(t *testing.T) {
			f := newLogFixture(t)
			ref := f.rel(0, "01-build.log")
			f.write(t, ref, []byte("RUN-X"))
			vid1, at := insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))
			mt := at.Add(delta)
			if err := os.Chtimes(filepath.Join(f.dir, ref), mt, mt); err != nil {
				t.Fatal(err)
			}
			code, body, raw := f.get(t, vid1, "")
			if got := isOverwritten(code, body); got != wantOverwritten {
				t.Errorf("got %d %s, want overwritten=%v", code, raw, wantOverwritten)
			}
			if !wantOverwritten && body["text"] != "RUN-X" {
				t.Errorf("容差内应正常读出: got %s", raw)
			}
		})
	}
	mtimeCase("给定文件 ModTime 晚于 at+10s（后一次运行正在写）断言 404 log_overwritten", 10*time.Second, true)
	mtimeCase("给定文件 ModTime 在 at+2s 容差内 断言 200", 2*time.Second, false)

	t.Run("给定复现多段只被覆盖其中一段 断言 part=0 404、part=1 200", func(t *testing.T) {
		f := newLogFixture(t)
		a, b := f.rel(0, "01-repro_fail.log"), f.rel(0, "02-repro_fail.log")
		f.write(t, a, []byte("A"))
		f.write(t, b, []byte("B"))
		vid1, at := insertVerification(t, f.api, f.tk.ID, "repro_fail", "passed", strp(a+" "+b))
		insertVerification(t, f.api, f.tk.ID, "repro_fail", "passed", strp(a))
		mt := at.Add(-time.Second)
		for _, p := range []string{a, b} {
			if err := os.Chtimes(filepath.Join(f.dir, p), mt, mt); err != nil {
				t.Fatal(err)
			}
		}
		if code, body, raw := f.get(t, vid1, "?part=0"); !isOverwritten(code, body) {
			t.Errorf("part=0 got %d %s, want 404 log_overwritten", code, raw)
		}
		if code, body, raw := f.get(t, vid1, "?part=1"); code != http.StatusOK || body["text"] != "B" {
			t.Errorf("part=1 got %d %s, want 200 B", code, raw)
		}
	})

	t.Run("给定非属主请求被覆盖的 vid 断言只看到任务不存在", func(t *testing.T) {
		f := newLogFixture(t)
		ref := f.rel(0, "01-build.log")
		f.write(t, ref, []byte("RUN-1"))
		vid1, _ := insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))
		f.write(t, ref, []byte("RUN-2"))
		insertVerification(t, f.api, f.tk.ID, "build", "passed", strp(ref))
		userB := mustUser(t, f.api.Store, "f04-ac5o-b-"+rhex(t)+"@example.com")
		srvB := apiServer(t, &API{Store: f.api.Store, Tasks: f.m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com"), LogDir: f.dir})
		code, _, raw := getLog(t, srvB.URL, f.tk.ID, vid1, "")
		if code != http.StatusNotFound || strings.TrimSpace(raw) != `{"error":"任务不存在"}` {
			t.Errorf("got %d %s, want 404 {\"error\":\"任务不存在\"}", code, raw)
		}
	})
}

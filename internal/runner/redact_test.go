package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// tail 是摘要截断的公共实现（落库与注入两侧都用）。它的两个约定都要守住：
// 按**字节**截，且不切断多字节 UTF-8 字符。
func TestTailTruncatesAtRuneBoundary(t *testing.T) {
	// 全中文（每字 3 字节）+ 前缀，构造一个「按字节硬切必落进字符中间」的
	// 输入：n 选得让 len(s)-(n-标记长度) 落在某个字的第 2/3 个字节上。
	body := strings.Repeat("中", 10) // 30 字节
	s := "HEAD" + body              // 34 字节，rune 边界在 4 + 3k（k=0..10）
	const n = 20
	const markLen = len("…(已截断)\n")
	if cut := len(s) - (n - markLen); cut == 0 || utf8.RuneStart(s[cut]) {
		t.Fatalf("夹具没踩到切点：s[%d] 正好是 rune 起始", cut)
	}

	got := tail(s, n)
	if !utf8.ValidString(got) {
		t.Fatalf("tail 切出了非法 UTF-8：%q", got)
	}
	if len(got) > n {
		t.Errorf("tail 结果 = %d 字节，超过上限 %d（上限是含截断标记的总预算）", len(got), n)
	}
	if !strings.Contains(got, "已截断") {
		t.Error("截断时应留下标记（静默截断会让人以为输出本来就这么短）")
	}
	// 短于上限时逐字返回，不额外加标记
	if short := tail("短", n); short != "短" {
		t.Errorf("未超上限时不该改动：got %q", short)
	}
	// 取的是尾部：结尾那几个字必须原样还在
	if !strings.HasSuffix(got, "中") {
		t.Errorf("tail 应保留原串结尾：got %q", got)
	}
}

// 摘要是「输出尾部」：结论（FAIL 行、断言消息）在最后，头被丢掉。
//
// 噪声必须**超过 maxStepOutput**：报告里的 Output 本身已被 verify 侧截到
// 那个上限，只有超出它才真的会触发本函数的截断路径 —— 否则这条用例
// 测的只是「字符串拼接没丢内容」。
func TestFailureExcerptKeepsOutputTail(t *testing.T) {
	r := Report{Results: []StepResult{{
		Step:   Step{Name: "test"},
		Status: StatusFailed,
		Output: strings.Repeat("早期噪声行\n", 5000) + "TAIL-MARKER: 期望 hello 得到 world",
	}}}
	got := failureExcerpt(r)
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Errorf("摘要应保留输出尾部，got:\n%s", got)
	}
	if len(got) > maxDigestStoredExcerpt {
		t.Errorf("摘要长度 = %d 字节，超过上限 %d", len(got), maxDigestStoredExcerpt)
	}
	if !utf8.ValidString(got) {
		t.Error("摘要必须是合法 UTF-8（落库会撞 SQLSTATE 22021）")
	}
}

// 报告里没有失败步骤时返回空串 —— 调用方据此退化为「不带摘要的记忆」，
// 而不是写一条空壳记录。
func TestFailureExcerptEmptyWhenAllPassed(t *testing.T) {
	r := Report{Results: []StepResult{{Step: Step{Name: "build"}, Status: StatusPassed}}}
	if got := failureExcerpt(r); got != "" {
		t.Errorf("全通过时不该产生摘要：%q", got)
	}
}

// 脱敏要盖住两类：本进程环境里的凭据值（子进程回显是真实泄漏形态），
// 以及形态可辨的 token（不来自环境的那些）。
func TestRedactSecretsCoversEnvValuesAndTokenShapes(t *testing.T) {
	const envVal = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	t.Setenv("LATHE_SMOKE_ACCESS_TOKEN", envVal)

	cases := []struct {
		name string
		in   string
	}{
		{"环境值回显", "dsn token=" + envVal},
		{"GitHub PAT", "curl -H 'Authorization: bearer ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789'"},
		{"Linear key", "lin_api_abcdefghijklmnopqrstuvwxyz123456"},
		{"URL 口令段", "postgres://lathe:s3cr3t-pw@127.0.0.1:55432/lathe_a3"},
		{"key=value", `DATABASE_PASSWORD="hunter2hunter2"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactSecrets(c.in, nil)
			if !strings.Contains(got, redactedMark) {
				t.Errorf("应当脱敏，实际未改动：%q", got)
			}
			if strings.Contains(got, envVal) {
				t.Errorf("环境值仍在：%q", got)
			}
			if strings.Contains(got, "s3cr3t-pw") || strings.Contains(got, "hunter2hunter2") ||
				strings.Contains(got, "GhIjKlMnOpQrStUvWxYz") || strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") {
				t.Errorf("凭据片段仍在：%q", got)
			}
		})
	}
}

// 反向：正常输出一个字节都不许动。脱敏误伤（把普通词抹成 [REDACTED]）
// 会让摘要失去排障价值，与不脱敏一样糟。
func TestRedactSecretsLeavesOrdinaryOutputAlone(t *testing.T) {
	in := strings.Join([]string{
		"--- FAIL: TestGreet (0.00s)",
		"    main_test.go:9: 期望 hello 得到 world",
		"FAIL\tdemo\t0.003s",
		"# demo",
		"./fix.go:3:2: undefined: greet",
		"PATH=/usr/local/go/bin:/usr/bin",
		"CI=true",
	}, "\n")
	if got := redactSecrets(in, nil); got != in {
		t.Errorf("普通输出被误改：\n得到:\n%s\n期望:\n%s", got, in)
	}
	// 短值不抹（这是上面那条得以成立的前提之一）
	t.Setenv("LATHE_MIN_SHORT_TOKEN", "true")
	if got := redactSecrets("token=true", nil); got != "token=true" {
		t.Errorf("短值不该脱敏：%q", got)
	}
}

// 两级脱敏会互相看得见：环境值替换留下的标记本身长得像 `token=…` 的值，
// 形态兜底不能再包一层。
func TestRedactSecretsDoesNotDoubleRedact(t *testing.T) {
	const envVal = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	t.Setenv("LATHE_SMOKE_ACCESS_TOKEN", envVal)

	got := redactSecrets("token="+envVal, nil)
	if strings.Contains(got, redactedMark+redactedMark) {
		t.Errorf("不该套两层标记：%q", got)
	}
	if strings.Count(got, redactedMark) != 1 {
		t.Errorf("期望恰好一处标记：%q", got)
	}
	// 顺序无关注（先形态后环境的结果一致）
	if got2 := redactSecrets("token="+envVal+" end", nil); strings.Contains(got2, envVal) {
		t.Errorf("环境值仍在：%q", got2)
	}
}

// 隔离栈的连接串经 StackEnv 灌进验证步骤，输出里回显它是最常见的形态
// （连接失败时整条 DSN 都在错误信息里）。
func TestRedactSecretsCoversStackEnv(t *testing.T) {
	const dsn = "postgres://lathe:stack-pw-9911@127.0.0.1:56001/lathe_task_42"
	got := redactSecrets("连接失败: dial "+dsn+" timeout", map[string]string{"DATABASE_URL": dsn})
	if strings.Contains(got, "stack-pw-9911") {
		t.Errorf("隔离栈口令泄漏：%q", got)
	}
	if !strings.Contains(got, redactedMark) {
		t.Errorf("应当脱敏：%q", got)
	}
}

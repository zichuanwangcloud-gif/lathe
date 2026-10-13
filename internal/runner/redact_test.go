package runner

import (
	"encoding/json"
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
	got := (&Pipeline{}).failureExcerpt(r)
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
	if got := (&Pipeline{}).failureExcerpt(r); got != "" {
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

// 形态表逐条钉住。每格都是一次实测留下的：AWS key id 曾被吐出去一半、
// JSON 形态曾整体漏出去、连接串的口令段曾整条漏。
//
// 断言分两步：**凭据不在**（安全），**上下文还在**（摘要仍然可读）。
// 只断言前者会让「整段抹掉」这种粗暴实现也过，而那种实现把排障信息一起
// 抹了 —— 摘要的价值就在于「哪一步、为什么」。
func TestRedactSecretsShapes(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOut  string // 逐字断言：替换位置也要钉住
		mustGone []string
	}{
		{
			name:    "AWS key id 整段抹掉",
			in:      "aws key AKIAIOSFODNN7EXAMPLE unused",
			wantOut: "aws key [REDACTED] unused",
		},
		{
			name:     "JWT 整段抹掉",
			in:       "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.abc-DEF_012",
			wantOut:  "Bearer [REDACTED]",
			mustGone: []string{"eyJhbGciOiJIUzI1NiJ9"},
		},
		{
			name:    "URL 只抹口令段",
			in:      "postgres://lathe:s3cr3t-pw@127.0.0.1:55432/lathe_a3",
			wantOut: "postgres://lathe:[REDACTED]@127.0.0.1:55432/lathe_a3",
		},
		{
			name:    "无 scheme 的连接串同样抹",
			in:      "dial lathe:s3cr3t-pw@127.0.0.1:55432 timeout",
			wantOut: "dial lathe:[REDACTED]@127.0.0.1:55432 timeout",
		},
		{
			name:    "GitHub PAT 保留前缀",
			in:      "auth=ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
			wantOut: "auth=ghp_[REDACTED]",
		},
		{
			name:    "Linear key 保留前缀",
			in:      "lin_api_abcdefghijklmnopqrstuvwxyz123456",
			wantOut: "lin_api_[REDACTED]",
		},
		{
			name:    "Slack token 保留前缀",
			in:      "xoxb-123456789012-abcdefghij",
			wantOut: "xoxb-[REDACTED]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactSecrets(c.in, nil)
			if got != c.wantOut {
				t.Errorf("got  %q\nwant %q", got, c.wantOut)
			}
			for _, g := range c.mustGone {
				if strings.Contains(got, g) {
					t.Errorf("仍含凭据片段 %q：%q", g, got)
				}
			}
		})
	}
}

// kv / JSON 形态：键带不带引号、冒号后有没有空格、值带不带引号，
// 六种组合都要抹 —— 少一种就是一个漏点。
func TestRedactSecretsKVAndJSONShapes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOut string
	}{
		{"裸键等号", `token=hunter2hunter2`, `token=[REDACTED]`},
		{"裸键冒号", `token: hunter2hunter2`, `token: [REDACTED]`},
		{"裸键冒号无空格", `token:hunter2hunter2`, `token:[REDACTED]`},
		{"裸键带引号值", `token:"hunter2hunter2"`, `token:[REDACTED]`},
		{"引号键无空格", `"token":"hunter2hunter2"`, `"token":[REDACTED]`},
		{"引号键带空格", `"token": "hunter2hunter2"`, `"token": [REDACTED]`},
		{"JSON 对象里的 apiKey", `{"apiKey":"abcd1234abcd1234"}`, `{"apiKey":[REDACTED]}`},
		{"单引号键", `'password': 'hunter2hunter2'`, `'password': [REDACTED]`},
		{"带前缀的变量名", `DATABASE_PASSWORD="hunter2hunter2"`, `DATABASE_PASSWORD=[REDACTED]`},
		// 裸值有长度门槛：`short` 只有 5 字节、不像凭据，不抹（见 minBareValueLen）
		{"裸值过短不抹", `oauth_token=short`, `oauth_token=short`},
		{"裸值够长就抹", `oauth_token=deadbeefcafe`, `oauth_token=[REDACTED]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactSecrets(c.in, nil); got != c.wantOut {
				t.Errorf("got  %q\nwant %q", got, c.wantOut)
			}
		})
	}
}

// ★ 第三轮复审 M-1：按名取值对凭据类变量名的**别名**不能留空白。
//
// `MYSQL_PWD`（MySQL 的标准名）、`LATHE_DB_PASS`、`LATHE_SESSION_KEY`、
// `LATHE_SIGNING_KEY`、`LATHE_GH_PAT` 这些名字下的值都是随机串，形态表
// 认不出来 —— 词根表漏一个就是在库里留一份明文。反过来，收 `AUTH`/笼统
// `URL` 那种宽词会误伤（上一轮刚收窄掉），所以这条同时守着两边。
func TestRedactSecretsCoversEnvAliases(t *testing.T) {
	aliases := map[string]string{
		"MYSQL_PWD":            "mysql-pwd-value-1",
		"LATHE_DB_PWD":         "pg-pwd-value-22",
		"LATHE_DB_PASS":        "db-pass-value-33",
		"LATHE_SESSION_KEY":    "session-key-value-4",
		"LATHE_SIGNING_KEY":    "signing-key-value-5",
		"LATHE_ENCRYPTION_KEY": "encryption-key-val-6",
		"LATHE_GH_PAT":         "gh-pat-value-77777",
	}
	for name, val := range aliases {
		t.Setenv(name, val)
	}
	for name, val := range aliases {
		t.Run(name, func(t *testing.T) {
			// 裸值（只在文本里出现，没有任何键名提示）也要抹掉
			if got := redactSecrets("输出里回显了 "+val+" 这一串", nil); strings.Contains(got, val) {
				t.Errorf("%s 的值泄漏：%q", name, got)
			}
			// 回显成 KEY=value 也要抹
			if got := redactSecrets(name+"="+val, nil); strings.Contains(got, val) {
				t.Errorf("%s 的键值对泄漏：%q", name, got)
			}
		})
	}

	// 反例：非凭据名不许被抹（这是收窄的目的，别在加别名时反弹回去）
	t.Setenv("LATHE_BASE_URL", "https://lathe.example.com")
	t.Setenv("LATHE_AUTH_MODE", "disabled")
	t.Setenv("LATHE_KEYBOARD_LAYOUT", "us-dvorak")
	for _, in := range []string{
		"https://lathe.example.com/tasks/1",
		"auth mode is disabled, expected enabled",
		"layout is us-dvorak by default",
	} {
		if got := redactSecrets(in, nil); got != in {
			t.Errorf("非凭据值被误伤：\nin  %q\nout %q", in, got)
		}
	}
}

// m-2：isRedactedValue 是**防御性**守卫（当前几条形态即使不跳过也恰好
// 得到同样结果），所以它必须被单测直接钉住 —— 否则将来放宽某个形态的
// 字符集时，它会静默失效并叠出 [REDACTED][REDACTED]。
func TestIsRedactedValue(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{redactedMark, true},
		{`"` + redactedMark + `"`, true},
		{`'` + redactedMark + `'`, true},
		{"", false},
		{"[REDACTEDx]", false},
		{"redacted", false},
		{"ghp_" + redactedMark, false},
	}
	for _, c := range cases {
		if got := isRedactedValue(c.in); got != c.want {
			t.Errorf("isRedactedValue(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// 端到端：环境值先被替换成标记后，形态兜底不许再包一层
	const envVal = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	t.Setenv("LATHE_SMOKE_ACCESS_TOKEN", envVal)
	for _, in := range []string{"token=" + envVal, `token:"` + envVal + `"`} {
		got := redactSecrets(in, nil)
		if n := strings.Count(got, redactedMark); n != 1 {
			t.Errorf("期望恰好一处标记，得到 %d 处：%q", n, got)
		}
	}
}

// ★ 第四轮复审（结构调整）：权威层 —— 平台已知的凭据值按字节精确替换。
//
// 这一层不依赖任何形态猜测，所以下面每条都绕开了形态表认不出的情形：
// 被 8KB 尾窗砍头产生的 DSN 残段（不含 `://`）、域名主机、挂在自定义
// 变量名下、以及没有任何键名提示地单独出现。
func TestRedactKnownValuesArePrecise(t *testing.T) {
	const (
		pw  = "EXTREMELY-SECRET-PW-9911"
		dsn = "postgres://lathe:" + pw + "@db.internal:5432/app"
		// 尾窗砍头后的残段：整串精确替换接不上，口令段接得上
		truncated = "e:" + pw + "@db.internal:5432/app"
	)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"整条 DSN", "连接失败: dial " + dsn, "连接失败: dial [REDACTED]"},
		{"被砍头的残段", "残留段 " + truncated, "残留段 e:[REDACTED]@db.internal:5432/app"},
		{"口令单独出现", "口令是 " + pw + " 记住", "口令是 [REDACTED] 记住"},
		{"挂在自定义变量名下", "MYSELF_CUSTOM=" + dsn, "MYSELF_CUSTOM=[REDACTED]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 只给权威层（形态表对下面几条也认不出，这里刻意先只验层 1）
			if got := redactKnown(c.in, knownCredentialValues([]string{dsn}, nil)); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
			// 端到端（层 1 + 层 2）同样不能留明文
			if got := redactText(c.in, []string{dsn}, nil); strings.Contains(got, pw) {
				t.Errorf("端到端仍含口令明文：%q", got)
			}
		})
	}
}

// 权威层的另一个来源：验证步骤的 StackEnv（隔离栈连接串）。
// 它带着口令，而且**不是**进程环境变量，只经 Step.StackEnv 流下来。
func TestRedactKnownCoversStackEnvPassword(t *testing.T) {
	// 口令**不在** registered 里、也不在自己的进程环境里，只存在于
	// Step.StackEnv（隔离栈连接串）—— 于是「关掉 StackEnv 这一路来源」
	// 就必然红。旧版夹具把整条 DSN 当输入喂给形态表，形态表自己就能抹，
	// 于是去掉 StackEnv 照样绿（名不副实）。
	const pw = "stack-password-4242"
	dsn := "postgres://lathe:" + pw + "@db.internal:56001/lathe_task_42"
	// 输入里口令**裸着出现**（没有 `user:pw@host` 结构）：形态表认不出随机串，
	// 只有权威层能抹 —— 这样「关掉 StackEnv 这一路来源」必然红。
	input := "step output: 连接口令是 " + pw + " 请注意"

	// 对照：不传 StackEnv 时口令必须留下（证明下面那次抹除来自 StackEnv）
	if got := redactText(input, nil, nil); !strings.Contains(got, pw) {
		t.Fatalf("对照不成立：无 StackEnv 时口令应留下，得到 %q", got)
	}

	got := redactText(input, nil, map[string]string{"DATABASE_URL": dsn})
	if strings.Contains(got, pw) {
		t.Errorf("StackEnv 里的口令应被权威层抹掉：%q", got)
	}
}

// ★ 第四轮复审点名的必测路径：被砍头的 DSN 残段（不含 `://`），
// 且口令**只存在于 StackEnv**（隔离栈连接串）。
//
// 夹具要点：输入里不带 `://`、主机保持原样，口令段单独拿不到形态证据 ——
// 于是「关掉 StackEnv 这一路来源」就必然红。旧版夹具把整条 DSN 喂进去，
// 形态表自己就能抹，去掉 StackEnv 照样绿（复审点名的假覆盖）。
func TestFailureExcerptRedactsSchemeLessDSNResidual(t *testing.T) {
	const pw = "REALVERYSECRETPW4242"
	dsn := "postgres://lathe:" + pw + "@127.0.0.1:56001/lathe_task_42"
	// 被砍头的残段：不含 `://`。主机是回环地址，但形态表可能认得出来 ——
	// 所以对照断言用「口令裸着出现」（形态表认不出随机串）保证红。
	residual := "lathe:" + pw + "@127.0.0.1:56001/app"
	out := "dial " + residual + " failed\n" + strings.Repeat("z", 4000)

	// 对照一：去掉 StackEnv 来源 → 口令必须留下（残段这里形态表仍可能盖住，
	// 所以另外用「裸口令」那一版对照，见对照二）
	bare := "口令 " + pw + " 被拒绝\n" + strings.Repeat("z", 4000)
	noStack := (&Pipeline{}).failureExcerpt(Report{Results: []StepResult{{
		Step: Step{Name: "test"}, Status: StatusFailed, Output: bare,
	}}})
	if !strings.Contains(noStack, pw) {
		t.Fatalf("对照不成立：无 StackEnv 时口令应留下，得到 %q", firstN(noStack, 200))
	}

	// 对照二：同一段输出，接上 StackEnv（隔离栈在跑）→ 口令必须被抹
	withStack := (&Pipeline{}).failureExcerpt(Report{Results: []StepResult{{
		Step:   Step{Name: "test", StackEnv: map[string]string{"DATABASE_URL": dsn}},
		Status: StatusFailed, Output: bare,
	}}})
	if strings.Contains(withStack, pw) {
		t.Errorf("StackEnv 里的口令应被抹，前 200 字节：%q", firstN(withStack, 200))
	}

	// 主断言：残段形状下（走完整链路 + StackEnv）不留明文
	got := (&Pipeline{}).failureExcerpt(Report{Results: []StepResult{{
		Step:   Step{Name: "test", StackEnv: map[string]string{"DATABASE_URL": dsn}},
		Status: StatusFailed, Output: out,
	}}})
	if !strings.Contains(got, "lathe:") || strings.Contains(got, "://") {
		t.Fatalf("夹具不成立：摘要应含残段且不含 ://，前 120 字节 = %q", firstN(got, 120))
	}
	if strings.Contains(got, pw) {
		t.Errorf("残段泄漏口令，前 200 字节：%q", firstN(got, 200))
	}
}

// ★ 第五轮复审 B5-1：隔离栈在跑时，层 1 不许把主机抹掉，否则层 2 失效、
// 口令明文落库。
//
// 最小复现（唯一变量是隔离栈在不在跑）：
//
//	dial lathe:AGENTSECRETPW9911@127.0.0.1:56001 timeout
//	  有 StackEnv → 旧版把 127.0.0.1 登记进权威层并抹成 [REDACTED]，
//	                层 2 的 `@<host>:<port>` 判据随即失去左邻，口令明文留下
//	  无 StackEnv → 口令被正常抹掉
//
// 修法是收窄 StackEnv 的登记口径（只登记含 `user:pass@` 的 DSN 值与其
// 口令段），顺带也修掉 M5-1（回环地址/用户名这些正常值不再被抹）。
func TestRedactStackEnvDoesNotClobberHost(t *testing.T) {
	const agentPW = "AGENTSECRETPW9911" // 不在权威层（agent 抄进来的）
	stackEnv := map[string]string{
		"DATABASE_URL":  "postgres://lathe:STACKDBPW777@127.0.0.1:56001/lathe_task_42",
		"DATABASE_HOST": "127.0.0.1",
		"DATABASE_PORT": "56001",
		"DATABASE_USER": "lathe",
		"DATABASE_NAME": "lathe_task_42",
	}
	const in = "dial lathe:" + agentPW + "@127.0.0.1:56001 timeout"

	withStack := redactText(in, nil, stackEnv)
	if strings.Contains(withStack, agentPW) {
		t.Errorf("口令明文落库（层 1 抹掉主机导致层 2 失效）：%q", withStack)
	}
	// 注意别写成 `[REDACTED]@`：口径替换后标记本来就紧挨 `@`（那是口令的位置）。
	// 这里要钉的是**主机**还在 —— 旧版会把它替换成 `[REDACTED]:56001`。
	if strings.Contains(withStack, redactedMark+":56001") {
		t.Errorf("主机被权威层抹掉（层 2 随即失效）：%q", withStack)
	}
	if !strings.Contains(withStack, "127.0.0.1:56001") {
		t.Errorf("主机与端口应原样保留（排障要看「连的是哪个库」）：%q", withStack)
	}
	// 与没有隔离栈时结果一致 —— 这是 B5-1 的判据（唯一变量不该改变结论）
	if noStack := redactText(in, nil, nil); noStack != withStack {
		t.Errorf("隔离栈在不在不该改变脱敏结果：\n无 %q\n有 %q", noStack, withStack)
	}

	// M5-1：正常排障信息不许被抹
	for _, msg := range []string{
		"dial tcp 127.0.0.1:56001: connect: connection refused",
		"user=lathe dbname=lathe_task_42",
	} {
		if got := redactText(msg, nil, stackEnv); got != msg {
			t.Errorf("非凭据值被误伤：\nin  %q\nout %q", msg, got)
		}
	}

	// 而隔离栈口令本身仍要被抹（收窄不等于放弃）
	for _, in := range []string{
		"口令 STACKDBPW777 无效",
		"postgres://lathe:STACKDBPW777@127.0.0.1:56001/lathe_task_42 连不上",
	} {
		if got := redactText(in, nil, stackEnv); strings.Contains(got, "STACKDBPW777") {
			t.Errorf("隔离栈口令应被抹：%q", got)
		}
	}
}

// ★ 第五轮复审 M9/M11：passwordOf 的两条分支各要单独被钉住。
//
// 它们此前互为冗余（去掉任一条，用例仍由另一条满足）。两条各有真实覆盖面：
// `url.Parse` 走标准 URL（含百分号解码），regex 走非 URL 形态（`tcp(host)`、
// 被砍头后没有 scheme 的残段）。分别断言。
func TestPasswordOfBothForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// url.Parse 分支：标准 DSN。注意口令里的 %21 会被解码成 `!`，
		// 这是 regex 分支给不出的语义 —— 所以返回解码后的值。
		{"标准 URL（含百分号编码）", "postgres://lathe:s3cr3t%21pw@db.internal:5432/app", "s3cr3t!pw"},
		{"标准 URL（无路径）", "redis://:onlypass@host:6379", "onlypass"},
		// regex 分支：url.Parse 会失败（没有 scheme / 不是合法 URL）
		{"非 URL 的 tcp 形态", "user:mysqlpw@tcp(db.internal:3306)/app", "mysqlpw"},
		{"被砍头后无 scheme 的残段", "lathe:REALVERYSECRETPW@db.internal:5432/app", "REALVERYSECRETPW"},
		{"无口令", "postgres://lathe@db.internal:5432/app", ""},
		{"无关文本", "dial tcp 127.0.0.1:56001: refused", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := passwordOf(c.in); got != c.want {
				t.Errorf("passwordOf(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// tail 在窗口左边界按字节砍，一条 DSN 会被切成不含 `://` 的残段 ——
// 这就是上面那条用例的输入形状从哪来（用未脱敏的 tail 直接验机制本身）。
func TestTailProducesSchemeLessDSNResidual(t *testing.T) {
	const pw = "REALVERYSECRETPW4242"
	dsn := "postgres://lathe:" + pw + "@db.internal:5432/app"
	const mark = "…(已截断)\n"
	tailRoom := maxDigestStoredExcerpt - len(mark)

	// 让 cut 恰好落在 `postgres://` 之后
	filler := strings.Repeat("z", tailRoom-(len(dsn)-len("postgres://")))
	out := strings.Repeat("噪声行 noise line\n", 1500) + dsn + filler

	residual := tail(out, maxDigestStoredExcerpt)
	if !strings.Contains(residual, "lathe:"+pw) || strings.Contains(residual, "://") {
		t.Fatalf("夹具没把边界压在 postgres:// 之后：前 120 字节 = %q", firstN(residual, 120))
	}
	// 而这一形状对形态表仍是可脱敏的（这是 B-1 修的判据）
	if got := redactSecrets(residual, nil); strings.Contains(got, pw) {
		t.Errorf("残段经形态表后仍含口令：%q", firstN(got, 200))
	}
}

// ★ 第四轮复审 B-1：无 scheme 的**域名主机**连接串（整类此前漏）。
//
// 旧版靠「主机必须是 IP 形状」来避邮箱，代价是 `@db.internal` / `@localhost`
// / `@redis.internal` / `@tcp(host)` 全部明文 —— 而本仓 preview 起的容器
// 服务名就是域名（`pg`），属可达路径。现在改成「左侧 userinfo 形状 + 右侧
// 主机带端口/路径/tcp(」来判连接串，邮箱由左侧形状自然排除。
func TestRedactSecretsCoversSchemeLessConnectionStrings(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dial lathe:s3cr3t-pw@db.internal:5432/app", "dial lathe:[REDACTED]@db.internal:5432/app"},
		{"dial lathe:pw@localhost:5432", "dial lathe:[REDACTED]@localhost:5432"},
		{"redis:secretpw@redis.internal:6379", "redis:[REDACTED]@redis.internal:6379"},
		{"user:mysqlpw@tcp(db.internal:3306)/app", "user:[REDACTED]@tcp(db.internal:3306)/app"},
		{"连接 postgres://lathe:pw@db.internal:5432/app 失败", "连接 postgres://lathe:[REDACTED]@db.internal:5432/app 失败"},
		// 尾窗砍头后的残段（左边界被吃掉，只剩 `e:PW@…`）
		{"... e:REALVERYSECRETPW@db.internal:5432/app", "... e:[REDACTED]@db.internal:5432/app"},
		{"dial lathe:pw@10.0.0.5:5432/app", "dial lathe:[REDACTED]@10.0.0.5:5432/app"},
	}
	for _, c := range cases {
		if got := redactSecrets(c.in, nil); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	// 反例：邮箱形态与无冒号的邮箱不抹
	for _, in := range []string{
		`{"user":"a@b.com"}`,
		`{"email":"admin@example.com"}`,
		`a:b@c.com`,
		`noreply@10.0.0.1`,
	} {
		if got := redactSecrets(in, nil); got != in {
			t.Errorf("邮箱形态被误伤：in %q out %q", in, got)
		}
	}
}

// ★ 第四轮复审 B-2：同一段里的**连续多个**键值对都要抹。
//
// 旧版的收尾守卫是消费类（`(?:[\s,;}\])"']|$)`），匹配后从它之后继续扫，
// 于是第二个键永远匹配不上 —— 而「逐行 env 转储」正是 redact.go 自己举的
// 头号泄漏形态，等于第二行起全漏。
func TestRedactSecretsHandlesConsecutivePairs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"逐行 env 转储", "TOKEN=aaaabbbbcccc\nSECRET=eeeeffffgggg", "TOKEN=[REDACTED]\nSECRET=[REDACTED]"},
		{"三行 env 转储", "TOKEN=aaaabbbbcccc\nPASSWORD=dddddddddd\nAPI_KEY=ffffffffffff",
			"TOKEN=[REDACTED]\nPASSWORD=[REDACTED]\nAPI_KEY=[REDACTED]"},
		{"docker run -e", "docker run -e TOKEN=aaaabbbbcccc -e SECRET=eeeeffffgggg",
			"docker run -e TOKEN=[REDACTED] -e SECRET=[REDACTED]"},
		{"docker inspect Env", `"Env": ["PATH=/usr/bin","MYSQL_PWD=aaaabbbbcccc","LATHE_DB_PASS=eeeeffffgggg"]`,
			`"Env": ["PATH=/usr/bin","MYSQL_PWD=[REDACTED]","LATHE_DB_PASS=[REDACTED]"]`},
		{"query string", "?api_key=aaaabbbbcccc&x=2", "?api_key=[REDACTED]&x=2"},
		{"fragment", "/path?secret=aaaabbbbcccc#frag", "/path?secret=[REDACTED]#frag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactSecrets(c.in, nil); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// ★ 第四轮复审 M-1：kv 词根表与环境词根表必须一致 —— 尤其是**文本里出现、
// 值不在 env 中**的那一半。旧用例只从 t.Setenv 取值，把 kv 别名全删掉它
// 照样绿（名不副实）。
func TestRedactSecretsCoversTextLevelKeyAliases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"LATHE_ENCRYPTION_KEY=zzzzzzzzzzzz", "LATHE_ENCRYPTION_KEY=[REDACTED]"},
		{"LATHE_SIGNING_KEY=zzzzzzzzzzzz", "LATHE_SIGNING_KEY=[REDACTED]"},
		{"LATHE_GH_PAT=yyyyyyyyyyyy", "LATHE_GH_PAT=[REDACTED]"},
		{"_PAT=qqqqqqqqqqqq", "_PAT=[REDACTED]"},
		{"MYSQL_PWD=aaaaaaaaaaaa", "MYSQL_PWD=[REDACTED]"},
		{"LATHE_DB_PASS=bbbbbbbbbbbb", "LATHE_DB_PASS=[REDACTED]"},
		{"LATHE_DB_PWD=cccccccccccc", "LATHE_DB_PWD=[REDACTED]"},
	}
	for _, c := range cases {
		// 刻意**不** setenv：这条考的是文本级词根，不是环境值那一级
		if got := redactSecrets(c.in, nil); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}

// ★ 第四轮复审 M-2：PWD/PASS 必须锚定，别把 shell 的工作目录抹掉。
//
// 无界子串会命中 `PWD`/`OLDPWD`（POSIX shell 的工作目录）、`SSH_ASKPASS`、
// `BYPASS_CACHE`、`LATHE_PASSIVE_MODE`、`COMPASS_HOME` —— 而步骤目录
// （`/workspaces/task-313/sub`）正是摘要里「在哪跑」那部分。
func TestRedactSecretsDoesNotEatWorkingDirectory(t *testing.T) {
	t.Setenv("PWD", "/workspaces/task-313/sub")
	t.Setenv("OLDPWD", "/workspaces/task-313")
	t.Setenv("SSH_ASKPASS", "/usr/bin/askpass-helper")
	t.Setenv("LATHE_PASSIVE_MODE", "on")
	t.Setenv("LATHE_WORKSPACE_ROOT", "/workspaces")
	t.Setenv("BYPASS_CACHE", "yes")

	for _, in := range []string{
		"步骤 build（/workspaces/task-313/sub）",
		"root=/workspaces 下",
		"helper=/usr/bin/askpass-helper",
		"PWD=/workspaces/task-313/sub",
		"OLDPWD=/workspaces/task-313",
		"BYPASS_CACHE=yes",
		"LATHE_PASSIVE_MODE=on",
	} {
		if got := redactSecrets(in, nil); got != in {
			t.Errorf("非凭据值被误伤：\nin  %q\nout %q", in, got)
		}
	}
	// 但带前缀的别名仍要抹（这才是真凭据别名）。裸值（文本里只出现值、
	// 没有键名提示）的抹除由权威层负责 —— 这里显式把值登记进去。
	for _, in := range []string{
		"MYSQL_PWD=mysqlpwdvalue123",
		"LATHE_DB_PASS=dbpassvalue12345",
	} {
		if got := redactSecrets(in, nil); !strings.Contains(got, redactedMark) {
			t.Errorf("凭据别名应被抹：%q", got)
		}
	}
	for _, in := range []string{"x mysqlpwdvalue123 y", "看到 mysqlpwdvalue123 这串"} {
		got := redactText(in, []string{"mysqlpwdvalue123"}, nil)
		if strings.Contains(got, "mysqlpwdvalue123") {
			t.Errorf("权威层应精确抹掉登记过的值：%q", got)
		}
	}
}

// ★ 第三轮复审：邮箱不许被 URL 的口令段规则误伤。
//
// 这条在 a061b34 上是通过的，是「URL 规则放开到无 scheme」那一步引入的回归：
// `[^\s/:@]+:([^\s/:@]+)@` 把 `{"user":"a@b.com"}` 里的 `"user":"a` 当成
// `用户:口令`，抹成 `{"user":[REDACTED]@b.com"}` —— 既误伤（邮箱不是凭据），
// 又**损坏结构**（JSON 里的引号被吃掉）。邮箱恰恰是本产品自己的数据形态
// （issue 作者、测试夹具都用 @example.com）。
func TestRedactSecretsLeavesEmailsAlone(t *testing.T) {
	cases := []string{
		`{"user":"a@b.com"}`,
		`{"email":"admin@example.com"}`,
		`{"author":"pipe-TestSomething-1791877309283573574@example.com"}`,
		`mail from a@b.com ok`,
		`Authors: a@b.com, c@d.org`,
		`{"reviewer":"user@lathe.local"}`,
	}
	for _, in := range cases {
		if got := redactSecrets(in, nil); got != in {
			t.Errorf("邮箱被误伤：\nin  %q\nout %q", in, got)
		}
	}
	// 但同一条规则必须继续盖住连接串（含无 scheme 的形态）
	cases2 := []struct{ in, want string }{
		{`lathe:secret@127.0.0.1:5432/lathe`, `lathe:[REDACTED]@127.0.0.1:5432/lathe`},
		{`postgres://lathe:s3cr3t@127.0.0.1:55432/lathe_a3`, `postgres://lathe:[REDACTED]@127.0.0.1:55432/lathe_a3`},
		{`redis://:secretpass@host:6379`, `redis://:[REDACTED]@host:6379`},
		{`postgres://user:pw@127.0.0.1/db`, `postgres://user:[REDACTED]@127.0.0.1/db`},
	}
	for _, c := range cases2 {
		if got := redactSecrets(c.in, nil); got != c.want {
			t.Errorf("连接串口令段应被抹：\nin   %q\ngot  %q\nwant %q", c.in, got, c.want)
		}
	}
}

// ★ 第三轮复审：kv 裸值分支不许吞掉 JSON 的收尾字符，也不许抹结构字面量。
//
// 裸值分支 `[^\s,;]{4,}` 是贪婪的：`,`/`;` 之外一路吃到串尾，于是
// `{"token":null}` 变成 `{"token":[REDACTED]`（右花括号没了）、
// `{"input_token":5000,"output_token":300}` 两个键都被抹。
// 这一条用 encoding/json 反解做断言 —— 「结构损坏」这类问题只靠逐字比对
// 容易被漏掉，能反解出来才是硬约束。
func TestRedactSecretsKeepsJSONStructure(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOut string
	}{
		{"null 值不抹", `{"token":null}`, `{"token":null}`},
		{"true 值不抹", `{"token":true}`, `{"token":true}`},
		{"数字值不抹", `{"input_token":5000,"output_token":300}`, `{"input_token":5000,"output_token":300}`},
		{"结构字面量不抹", `{"credentials":"default"}`, `{"credentials":"default"}`},
		{"字符串凭据仍抹", `{"token":"abcdefgh","next":1}`, `{"token":[REDACTED],"next":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactSecrets(c.in, nil)
			if got != c.wantOut {
				t.Errorf("got  %q\nwant %q", got, c.wantOut)
			}
			// 结构性断言分两类，因为标记本身的形态决定了结果：
			//   - 没抹任何东西时必须仍是合法 JSON（这条抓的是「裸值分支吞掉
			//     收尾字符」那类结构损坏 —— B-2 的原始表现）
			//   - 抹了引号值时 JSON 会变成 [REDACTED] 占位的裸值（不是合法
			//     JSON 字符串字面量），这是**已知且刻意**的：替换进去的标记
			//     不进引号，否则就是在值里嵌引号、把结构改成「看起来完整但
			//     其实变了样」。所以这类只断言**每个键都还在**（结构没被吞）。
			if !strings.Contains(got, redactedMark) {
				var v any
				if err := json.Unmarshal([]byte(got), &v); err != nil {
					t.Errorf("未脱敏的输出应仍是合法 JSON：%v\n%s", err, got)
				}
				return
			}
			for _, key := range jsonTopLevelKeys(c.in) {
				if !strings.Contains(got, `"`+key+`"`) {
					t.Errorf("脱敏后键 %q 丢了（结构被吞）：%q", key, got)
				}
			}
			if n := strings.Count(got, "{") - strings.Count(got, "}"); n != strings.Count(c.in, "{")-strings.Count(c.in, "}") {
				t.Errorf("花括号不再配对：in=%q out=%q", c.in, got)
			}
		})
	}
}

// jsonTopLevelKeys 取一段 JSON 里的顶层键名（夹具都是扁平对象，够用）。
func jsonTopLevelKeys(s string) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ★ 第三轮复审：转义引号里的值不许放行（§7 红线的真泄漏）。
//
// `{"token":"abc\"defghijklmno"}` 此前被抹成 `{"token":[REDACTED]defghijklmno"}`
// —— 看起来脱敏了，口令明文还在。旧版 `token="abc\"x"` 同样漏（存量），
// 这次一并钉住。替换只能动值本身，值以外的字符（含转义序列）保持原样。
func TestRedactSecretsHandlesEscapedQuotes(t *testing.T) {
	cases := []struct {
		in      string
		wantOut string
		mustNot []string
	}{
		{
			in:      `detail: {"password":"\"quoted-pw-1234\""}`,
			wantOut: `detail: {"password":[REDACTED]}`,
			mustNot: []string{"quoted-pw-1234"},
		},
		{
			in:      `{"token":"abc\"defghijklmno"}`,
			wantOut: `{"token":[REDACTED]}`,
			mustNot: []string{"defghijklmno"},
		},
		{
			in:      `{"token":"abc\"def","other":"kept-value"}`,
			wantOut: `{"token":[REDACTED],"other":"kept-value"}`,
			mustNot: []string{"def"},
		},
	}
	for _, c := range cases {
		got := redactSecrets(c.in, nil)
		if got != c.wantOut {
			t.Errorf("got  %q\nwant %q", got, c.wantOut)
		}
		for _, m := range c.mustNot {
			if strings.Contains(got, m) {
				t.Errorf("仍含明文片段 %q：%q", m, got)
			}
		}
	}
}

// 反向的**误伤面**：非凭据的地址/模式值不许被抹。
//
// 早先版本按「变量名里有 URL / AUTH 就抹」取值，实测把
// `got redirect https://…/tasks/1` 与 `auth mode is disabled` 打成
// [REDACTED] —— 那两类输出恰恰是验证失败时最该看的。这条用例把口径钉住：
// 只有真的凭据名（token/secret/password/key 词根、DSN 这一类）才抹。
func TestRedactSecretsLeavesNonSecretConfigValuesAlone(t *testing.T) {
	t.Setenv("LATHE_BASE_URL", "https://lathe.example.com")
	t.Setenv("LATHE_AUTH_MODE", "disabled")
	t.Setenv("DATABASE_URL", "postgres://lathe:lathe@127.0.0.1:55432/lathe_a3")

	cases := []string{
		"got redirect https://lathe.example.com/tasks/1",
		"auth mode is disabled, expected enabled",
		"base url is https://lathe.example.com",
	}
	for _, in := range cases {
		if got := redactSecrets(in, nil); got != in {
			t.Errorf("非凭据值被误伤：\nin  %q\nout %q", in, got)
		}
	}
	// 但 DSN 里的口令段仍然要抹（LATHE_BASE_URL 收窄了，DATABASE_URL 没有）
	got := redactSecrets("connect postgres://lathe:lathe@127.0.0.1:55432/lathe_a3 failed", nil)
	if strings.Contains(got, ":lathe@") {
		t.Errorf("DSN 口令段泄漏：%q", got)
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
	// 两级脱敏各有自己的门槛，这里把边界钉住：
	//   - 环境值那一级：短值不抹（抹 `true`/`linux` 会把输出打成筛子）
	//   - kv 引号值：不看长度（键名 + 引号两重信号）
	//   - kv 裸值：长度 >= minBareValueLen 且像随机串才抹
	t.Setenv("LATHE_MIN_SHORT_TOKEN", "true")
	if got := redactSecrets("值就三个字母 true", nil); got != "值就三个字母 true" {
		t.Errorf("环境值那一级应受长度门槛保护，不抹短串：%q", got)
	}
	if got := redactSecrets(`token:"x"`, nil); got != `token:[REDACTED]` {
		t.Errorf("引号值不看长度：%q", got)
	}
	if got := redactSecrets("token=x", nil); got != "token=x" {
		t.Errorf("裸值过短不抹：%q", got)
	}
	if got := redactSecrets("token=deadbeefcafe", nil); got != "token=[REDACTED]" {
		t.Errorf("裸值够长且像随机串应抹：%q", got)
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
	// 同 TestRedactKnownCoversStackEnvPassword：口令只存在于 StackEnv，
	// 「关掉 StackEnv 来源」必须红。输入**不含 `://`**，形态表在主机是
	// 域名且无端口证据时接不住。
	const pw = "stack-pw-9911"
	dsn := "postgres://lathe:" + pw + "@db.internal:56001/lathe_task_42"
	input := "连接失败：口令 " + pw + " 无效"

	if got := redactSecrets(input, nil); !strings.Contains(got, pw) {
		t.Fatalf("对照不成立：无 StackEnv 时口令应留下，得到 %q", got)
	}
	got := redactSecrets(input, map[string]string{"DATABASE_URL": dsn})
	if strings.Contains(got, pw) {
		t.Errorf("StackEnv 口令应被抹：%q", got)
	}
	if !strings.Contains(got, redactedMark) {
		t.Errorf("应当脱敏：%q", got)
	}
}

// firstN 截前 n 个字节（失败信息里不打 8KB）。
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

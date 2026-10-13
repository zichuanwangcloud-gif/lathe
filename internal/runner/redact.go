package runner

// redact.go 「验证输出摘要」落库前的脱敏（§3.6 第 16 条的一部分）。
//
// ---------------------------------------------------------------------------
// 为什么必须有这一层
// ---------------------------------------------------------------------------
//
// 失败记忆要把失败步骤的**输出**落库、并在下次派发时注入提示词。而验证
// 命令跑在宿主环境里（verify.go 的 stepEnv 直接 append(os.Environ(), ...)，
// 隔离栈的连接串又经 StackEnv 灌进每一步），输出里完全可能出现：
//
//   - 子进程回显的环境变量（`t.Logf("dsn=%s", os.Getenv("DATABASE_URL"))`）
//   - 命令行/日志里带出的 token（curl -H "Authorization: Bearer ..."）
//   - 断言消息里被 agent 抄进去的凭据
//
// AGENTS.md §7 的红线是「日志/界面/事件流永不出现明文凭据」，而这份摘要
// 恰好三处都去（库、提示词、任务详情页）—— 所以脱敏必须在**写入侧**做，
// 一次做对，后面每一处消费方都安全。
//
// ---------------------------------------------------------------------------
// 为什么复用不了既有实现
// ---------------------------------------------------------------------------
//
// 已有的三处都不是这一层能用的：secret.Mask 是「单个已知凭据 → 掩码」
// （输入是凭据本身，这里输入是自由文本）；preview 的 passwordClassRe 只认
// 变量名（用来决定自动生成口令），不扫文本；config.Redacted 是结构化配置
// 的字段白名单。三者都假定「已知要藏什么」，而这里要回答的是「这段自由
// 文本里混进了什么」—— 只能靠「平台知道的环境值」比对 + 形态识别兜底。
//
// 环境值比对是主力：它不依赖凭据长什么样，覆盖的正是最真实的那类泄漏
// （子进程回显 env）。形态表只兜那些不来自本进程环境的值。

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// redactedMark 是脱敏后留下的替换标记。
//
// 留标记而不是删空：删空会让「这里本来有个值」这个事实消失（摘要看起来
// 就像输出本来就是空的），而排障时需要知道「日志里有一段被平台抹掉了」。
const redactedMark = "[REDACTED]"

// secretKeyRe 命中「变量名本身说明它是敏感值」的环境变量。
//
// 与 preview 的 passwordClassRe 同一套词根，但刻意各写一份：那一份服务的是
// 「哪些必填变量由平台自动生成口令」，改它的词表不该牵动脱敏，反之亦然。
// 注意 AUTH 会连 LATHE_AUTH_* 一起收进来 —— 这正是想要的（会话密钥、
// 重置令牌的签名材料都在那一族里）。
var secretKeyRe = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIALS?|ACCESS_?KEY|AUTH)`)

// urlishKeyRe 命中值形如 URL/DSN 的变量名：`postgres://user:pass@host/db`
// 的口令段不由上面那组词根命名，但一样是凭据。
var urlishKeyRe = regexp.MustCompile(`(?i)(DSN|URL|URI|DATABASE|REDIS|AMQP|BROKER)`)

// minSecretLen 是「值得脱敏的环境值」的最短长度。
//
// 低于它的值不抹：抹 1–7 字节的短串（"1"、"yes"、"true"、"linux"）会把正常
// 输出打成筛子，而真实凭据（PAT、JWT、32 位十六进制口令、带口令的 DSN）
// 都远长于此。
const minSecretLen = 8

// tokenPatterns 是凭据形态的兜底表。每条都只替换捕获组里的值，前缀
// （ghp_、lin_api_…）原样保留 —— 「这里有个 ghp_ 开头的凭据」本身就是
// 有用的排障信息，抹掉它反而看不出来。
var tokenPatterns = []*regexp.Regexp{
	// GitHub：PAT（ghp_）、OAuth/Server-to-server（gho_/ghs_）、
	// 用户与刷新令牌（ghu_/ghr_）、fine-grained PAT（github_pat_）。
	regexp.MustCompile(`\b((?:gh[pousr]|github_pat)_)[A-Za-z0-9_]{16,}\b`),
	// Linear API key。
	regexp.MustCompile(`\b(lin_api_)[A-Za-z0-9]{16,}\b`),
	// Anthropic / OpenAI 风格的 sk- 密钥。
	regexp.MustCompile(`\b(sk-)[A-Za-z0-9\-_]{16,}\b`),
	// Slack bot/app/user token。
	regexp.MustCompile(`\b(xox[abprs]-)[A-Za-z0-9\-]{10,}\b`),
	// AWS access key id（前缀不是凭据的一部分，抹掉不影响可读性）。
	regexp.MustCompile(`\b(?:AKIA|ASIA)([A-Z0-9]{16})\b`),
	// URL 里的 basic-auth 口令段：scheme://user:pass@host。多捕获组形态
	// 会被替换逻辑当成 key=value 处理（保留 v[1]+v[2]），所以这里补一个
	// 空捕获组对齐语义：只保住 scheme://user，口令段抹掉。
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@]+)():[^/\s@]+@`),
	// key=value / key: value 形态（含 `DATABASE_PASSWORD=…` 这类带前缀的
	// 变量名），值整段抹掉。值可带引号，门槛 8 字节与 minSecretLen 对齐 ——
	// 更短的值（oauth_token=expired）抹掉只会让输出难读，而真实凭据都
	// 不在这个量级。
	//
	// 不能用 \b 起头：`_` 在 Go 的 \b 里算词字符，`DATABASE_PASSWORD=` 的
	// 下划线之后没有边界，词根永远匹配不上。改成「显式吃一个前置字符」，
	// 由替换模板原样吐回。
	regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)\s*[:=]\s*("[^"]{8,}"|'[^']{8,}'|[^\s,;]{8,})`),
}

// isRedactedValue 报告这段文本是不是「已经脱敏过」的产物。
//
// 两级脱敏会互相看得见：环境值替换留下的 redactedMark 本身就是一个
// 「token=…」形态的值，形态兜底若不跳过它就会写成 [REDACTED][REDACTED]。
func isRedactedValue(s string) bool {
	t := strings.Trim(s, `"'`)
	return t == redactedMark
}

// redactSecrets 把一段自由文本里的凭据替换成 redactedMark。
//
// env 是这次验证**实际**用的额外环境（Step.StackEnv：隔离栈的连接串），
// 与宿主环境一起构成「平台知道的凭据值」全集。
func redactSecrets(s string, env map[string]string) string {
	if s == "" {
		return ""
	}

	// 1) 收集敏感值。
	values := map[string]bool{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name, val := kv[:i], kv[i+1:]
		if len(val) >= minSecretLen && (secretKeyRe.MatchString(name) || urlishKeyRe.MatchString(name)) {
			values[val] = true
		}
	}
	for _, val := range env {
		// StackEnv 一律算敏感：值就是 DSN，自带口令。
		if len(val) >= minSecretLen {
			values[val] = true
		}
	}

	// 2) 长的先替：短值可能是长值的子串，先替短的会把长值切成两段、
	// 让长值再也匹配不上。
	if len(values) > 0 {
		ordered := make([]string, 0, len(values))
		for v := range values {
			ordered = append(ordered, v)
		}
		sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
		replacers := make([]string, 0, 2*len(ordered))
		for _, v := range ordered {
			replacers = append(replacers, v, redactedMark)
		}
		s = strings.NewReplacer(replacers...).Replace(s)
	}

	// 3) 形态兜底。用 ReplaceAllStringFunc 而不是 ReplaceAllString 的
	// `${1}` 模板：要能跳过「上一级已经抹过」的那一段，只替换真正的值。
	for _, re := range tokenPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			v := re.FindStringSubmatch(m)
			if len(v) < 2 || isRedactedValue(v[len(v)-1]) {
				// 没有捕获组可保留（整段都该抹），或上一级已经抹过这一段。
				return m
			}
			// 单捕获组：前缀原样保留（「这里有个 ghp_ 开头的凭据」是有用
			// 的信息）。多捕获组（key=value 形态）：前置字符 + 词根保留，
			// 只抹值。
			if len(v) == 2 {
				return v[1] + redactedMark
			}
			return v[1] + v[2] + redactedMark
		})
	}
	return s
}

// tail 取字符串的尾部，按**字节**上限截断，且不切断多字节 UTF-8 字符。
//
// 两个刻意的选择：
//
//   - **尾部**而非头部：验证输出（go test / 编译器 / panic）把结论写在最后
//     —— FAIL 行、断言消息、栈底。取头部恰好把最有用的那几行挤掉。
//   - **字节**而非 rune：上限服务的是「别把库和提示词撑爆」，那是字节预算；
//     按字节切再回退到 rune 边界，既不用整串解码（长输出的又一次全量分配），
//     又不会切出非法 UTF-8 —— 落库撞 SQLSTATE 22021 就是这么来的
//     （AGENTS.md §9）。
//
// 截断时在开头留标记：静默截断会让人以为输出本来就这么短。
//
// n 是**含标记在内**的总上限：调用方读到的是「这一段的字节预算」，
// 标记额外占几个字节就把预算撑破（它本身就是为提示词/库的预算服务的）。
func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	const mark = "…(已截断)\n"
	if n <= len(mark) {
		// 预算装不下标记：只取尾部那几个字节，不硬塞标记
		cut := len(s) - n
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		return s[cut:]
	}
	// 回退到 rune 边界：按字节硬切会把多字节字符切成两半（SQLSTATE 22021）
	cut := len(s) - (n - len(mark))
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return mark + s[cut:]
}

// failureExcerpt 从一次验证报告里取出「失败在哪、为什么」的文本摘要。
//
// 这是跨任务记忆里最值钱的部分：失败阶段只说明「哪一步」，这段说明
// 「为什么」—— 下一个跑这个单子的 agent 真正需要的是后者。
//
// 内容用报告里已有的东西拼，不引入新的事实来源：首个失败步骤的名字与
// 目录、它的错误（`exit status 1` 这类）、以及它输出的**尾部**。
// 报告里没有失败步骤时返回空串（调用方据此退化为不带摘要的记忆）。
//
// 命令本身不进摘要：它的参数里可能有 agent 直接抄进去的凭据，形态表认不出
// 来；目录已经足够定位「哪一步、在哪跑」。
//
// 返回值一定是脱敏过的 —— 它只往库里和提示词里去。
func failureExcerpt(report Report) string {
	f := report.FirstFailure()
	if f == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("步骤 " + string(f.Step.Name))
	if f.Step.Dir != "" && f.Step.Dir != "." {
		b.WriteString("（" + f.Step.Dir + "）")
	}
	if f.Err != nil {
		b.WriteString("\n错误：" + f.Err.Error())
	}
	if out := strings.TrimRight(f.Output, "\n"); out != "" {
		// 尾部：结论在后面。f.Output 已被 verify 侧截到 maxStepOutput。
		b.WriteString("\n输出尾部：\n" + tail(out, maxStepOutput))
	}

	return tail(redactSecrets(b.String(), f.Step.StackEnv), maxDigestStoredExcerpt)
}

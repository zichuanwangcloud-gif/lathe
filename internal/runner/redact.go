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
// （子进程回显 env）。形态表（tokenShapes）只兜那些不来自本进程环境的值，
// 且**非穷举** —— 见那里的注释。
//
// ---------------------------------------------------------------------------
// 两条互相拉扯的约束
// ---------------------------------------------------------------------------
//
// 这一层同时被两个方向拽着，每次改动都要两头都看：
//
//   - **不能漏**：漏一个就是库里/提示词里留一份明文（§7 红线）。
//   - **不能误伤**：把正常输出抹成 [REDACTED]，摘要就失去排障价值 ——
//     而摘要存在的全部意义就是「哪一步、为什么失败」。
//
// 第三轮复审抓到的三类问题正好各踩一头：邮箱被当连接串抹（误伤 + 损坏
// JSON 结构）、`{"token":null}` 被吞掉右花括号（损坏结构）、转义引号里的
// 口令没被抹（漏）。现在的形态是三条约束换来的，改表前先读下面每条注释。
//
// ---------------------------------------------------------------------------
// 已知的代价与边界（写下来免得被当成「已经保证干净」）
// ---------------------------------------------------------------------------
//
//   - 环境值那一级按**变量名**取值，收窄到「名字下的值就是凭据」才取。
//     再宽的词根（AUTH、笼统的 URL）会把 `auth mode is disabled`、
//     `https://host/tasks/1` 这类正常输出打成 [REDACTED]，摘要的排障价值
//     直接归零 —— 宁可少抹一点非凭据值，也不把摘要变成筛子。代价是：
//     **某个自定义名字的变量装着凭据、名字里又没有词根表那些词时，它的值
//     不会被这一级抹掉**（那还有形态表兜底）。
//   - 形态表认不出 PEM 私钥、自定义前缀的内部 key、以及被当成普通词出现的
//     口令（`the password is hunter2` 这种没有键值结构的）。
//   - 无 scheme 的连接串只在**主机部分是 IP 形状**（`lathe:pw@127.0.0.1`）
//     时被识别：没有 scheme 又用域名主机的 `lathe:pw@db.internal` 认不出。
//     这条是刻意的窄化 —— 放宽就必然把邮箱吃掉（见 urlCredRe 的注释），
//     而带 scheme 的 DSN 一律覆盖（那才是实际会出现在验证输出里的形态）。
//   - 摘要最终会随提示词经 agent.go 的 buildArgs 作为 **argv** 传给 CLI，
//     因此它不只存在于库里：同机进程能通过 `/proc/<pid>/cmdline` 读到。
//     （agent 子进程的环境本身是白名单化的，见 agent.go 的 sanitizedEnv；
//     但 argv 不受它保护。）

import (
	"fmt"
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
// 与 preview 的 passwordClassRe 同一套词根，但**刻意比它窄**，而且各写一份：
// 那一份服务的是「哪些必填变量由平台自动生成口令」，改它的词表不该牵动脱敏。
//
// 这里收词根的标准是「这个名字下的值**就是**凭据」，不是「可能跟认证有关」。
// 收得再宽一点（早先版本收过 AUTH）实测就开始误伤：`LATHE_AUTH_MODE=disabled`
// 会把 `auth mode is disabled, expected enabled` 抹成
// `auth mode is [REDACTED], expected enabled` —— 摘要的排障价值直接没了，
// 而这类输出恰恰是验证失败时最该看的东西。
//
// 反过来也不能窄：**别名要收齐**（第三轮复审实测 `MYSQL_PWD`、
// `LATHE_DB_PASS`、`LATHE_SESSION_KEY`、`LATHE_SIGNING_KEY`、`LATHE_GH_PAT`
// 的值都原样漏了出去）。这类名字下的值都是随机串，形态表认不出来 ——
// 漏一个就是在库里留一份明文。所以：
//
//   - `PWD`/`PASS` 是 `PASSWORD` 的常见简写（MySQL 的 `MYSQL_PWD` 是标准名）
//   - `SIGNING_KEY`/`SESSION_KEY`/`ENCRYPTION_KEY` 是带中缀的 KEY，
//     光有 `API_?KEY` 盖不住
//   - `_PAT`（personal access token）后缀同理
//
// 但**不收单独的 `_KEY`**：太泛（`LATHE_KEYBOARD_LAYOUT`、
// `GPG_KEY_ID` 里的 id 不是口令）。宁可少抹一个非凭据值，也不把常见词
// 打成标记 —— 那会让整套脱敏看起来失效。
var secretKeyRe = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|PWD|PASS|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|CREDENTIAL|SIGNING_?KEY|SESSION_?KEY|ENCRYPTION_?KEY|_PAT$)`)

// urlishKeyRe 命中值是 URL/DSN 的变量名：`postgres://user:pass@host/db` 的
// 口令段不由上面那组词根命名，但一样是凭据。
//
// **不收 BASE_URL / *_URL 这类通用地址**：`LATHE_BASE_URL` 是实例地址，
// 里面没有口令，而它的值会出现在大量正常输出与断言消息里（实测会把
// `got redirect https://lathe.example.com/tasks/1` 抹掉）。收窄的依据是
// 「值里必然带 user:pass@」—— DSN 这一类名字才具备这个性质。
var urlishKeyRe = regexp.MustCompile(`(?i)(^|_)(DSN|DATABASE_URL|DATABASE_URI|REDIS_URL|AMQP_URL|BROKER_URL|CONNECTION_STRING)`)

// minSecretLen 是「值得脱敏的环境值」的最短长度。
//
// 低于它的值不抹：抹 1–7 字节的短串（"1"、"yes"、"true"、"linux"）会把正常
// 输出打成筛子，而真实凭据（PAT、JWT、32 位十六进制口令、带口令的 DSN）
// 都远长于此。
const minSecretLen = 8

// minBareValueLen 是 kv 形态里**裸值**（不带引号）的最短长度。
//
// 引号值没有长度门槛（`"token":"x"` 也抹）—— 引号 + 键名两重信号已经说明
// 那是个值；裸值只有键名一重信号，且和普通词混在一起（`token: expired`、
// `password: <placeholder>`），所以要求它至少像个随机串：
// 长度 8 起、字符集限定在凭据常用的那几类（见 kvBareValue）。
const minBareValueLen = 8

// literalValueRe 命中「明显不是凭据」的字面量：JSON 的 null/true/false、
// 各种空值写法、纯数字、以及占位词。命中就不抹。
//
// 这条是第三轮复审的 B-2：`{"token":null}`、`{"input_token":5000}`、
// `{"credentials":"default"}` 都不该被当成凭据 —— 键名像是敏感名，值却是
// 结构字面量，抹掉只会损坏 JSON 且丢信息。
var literalValueRe = regexp.MustCompile(`(?i)^(null|true|false|none|nil|default|undefined|nan|\[redacted\])$|^[+-]?\d+(\.\d+)?$`)

// urlCredSchemeRe 与 urlCredHostRe 是 URL/连接串的 basic-auth 口令段规则，
// 拆成两条是因为两种形态的**判据不同**（捕获组 2 都是口令）：
//
//	有 scheme（`postgres://lathe:pw@host`、`redis://:pw@host`、`mysql://user:pw@host`）
//	   `://` 本身就是决定性证据，右侧一律认（主机是域名还是 IP 都行）。
//	   用户名允许为空（Redis 的 URI 形态就是 `redis://:pw@host`）。
//	无 scheme（`lathe:pw@127.0.0.1:5432/db`）
//	   验证输出里常常只剩这一段（`dial lathe:pw@…`），必须覆盖；但没有
//	   `://` 就没有证据，只能要求主机部分是 **IP 形状**（`@` 后紧邻数字或
//	   `[`）。邮箱的 `@` 右侧是域名（字母开头），因此不被认。
//
// 不这么分就会踩两条：放宽到「任何 `x:y@z`」→ 邮箱被当凭据抹、JSON 结构
// 被吃坏（第三轮复审 B-1 的回归）；只认带 scheme 的 → 无 scheme 连接串漏。
//
// 两条都用「吃掉主机首字符」代替 lookahead（Go 的 regexp 是 RE2，没有前后
// 视）：该字符不在替换组里，原样保留。
const (
	urlCredSchemeRe = `(?:[a-zA-Z][a-zA-Z0-9+.\-]*://)([^\s/:@]*):([^\s/:@]+)@`
	urlCredHostRe   = `(?:^|[\s(\[{,;='"])([^\s/:@]+):([^\s/:@]+)@(?:\d|\[)`
)

// tokenShape 是凭据形态表的一格：一条正则 + 「要抹掉的那一段」是哪个捕获组。
//
// 显式标出组号，而不是靠「组数猜语义」—— 早先版本就是后者，于是
// `\b(?:AKIA|ASIA)([A-Z0-9]{16})\b`（唯一没把整段圈进组的条目）落进
// 「保留前缀」分支，把 AWS key id 的前 4 位与后 16 位一起原样吐了出去。
// 组号写在表里，每个条目的意图就是它的注释，读一眼就对得上。
type tokenShape struct {
	re *regexp.Regexp
	// group 是「值」所在的捕获组。命中这一段以外的部分（包括同一捕获组
	// 前后的字面量）原样保留：`ghp_` 这类前缀留着是有用的排障信息，
	// `postgres://user:` 与 `@host` 留着能让摘要仍然可读。
	group int
	// skip 非空时，对捕获到的值再判一次：返回 true 就不抹（值太像结构
	// 字面量/普通词，抹了只会损坏信息）。nil 表示一律抹。
	skip func(value string) bool
}

// 键名词根（kv 形态用）。与 secretKeyRe 是两码事：那个筛**环境变量名**，
// 这个筛**文本里的键名**，但词根表保持一致 —— 两处漏法不同，收的词应该一样。
const kvKeyRoots = `password|passwd|pwd|pass|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|signing[_-]?key|session[_-]?key|credentials?`

// kvValue 是带引号的值：允许 `\"`/`\'` 转义。
//
// 老写法 `"[^"]*"` 会在第一个转义引号处截断，于是
// `{"token":"abc\"defghijklmno"}` 只抹掉 `abc\"` 留下一截明文 ——
// 看起来已经脱敏，实际上口令还在（第三轮复审的 B-3，§7 红线内的真泄漏）。
const kvValue = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`

// kvBareValue 是裸值：没有引号包裹，只有键名一个信号。
//
// 字符集限定在凭据常用的那几类（字母数字 + `-_./=+`），**不接受 `,;}` 等
// 结构字符** —— 老写法 `[^\s,;]{4,}` 是贪婪的，`{"token":null}` 会一路
// 吃到串尾把右花括号也吞掉（第三轮复审的 B-2）。
//
// 长度下限交给 minBareValueLen，理由是裸值必须「像个随机串」才敢动。
const kvBareValue = `[A-Za-z0-9_\-./+=]{%d,}`

// notASecretLiteral 是 tokenShape.skip 的实现：明显不是凭据的值不抹。
func notASecretLiteral(v string) bool {
	return literalValueRe.MatchString(strings.Trim(v, `"'`))
}

// tokenShapes 是凭据形态的兜底表。
//
// **非穷举**：它盖的是「无需上下文就能认出来的凭据形态」。认不出的还有
// 不少 —— PEM 私钥块（`-----BEGIN ... PRIVATE KEY-----`）、自签名的
// base64 blob、自定义前缀的内部 key、以及任意被当成普通词出现的口令
// （`the password is hunter2`）。这一层的价值在于「多盖一层」而不是
// 「盖全」：真的保证来自上面那一级（环境值与文本比对），那里不依赖
// 凭据长什么样。
var tokenShapes = []tokenShape{
	// GitHub：PAT（ghp_）、OAuth/Server-to-server（gho_/ghs_）、
	// 用户与刷新令牌（ghu_/ghr_）、fine-grained PAT（github_pat_）。
	// 前缀保留：「这里有个 ghp_ 开头的凭据」本身就是有用的排障信息。
	{re: regexp.MustCompile(`\b(?:gh[pousr]|github_pat)_([A-Za-z0-9_]{16,})\b`), group: 1},
	// Linear API key（同上，前缀保留）。
	{re: regexp.MustCompile(`\blin_api_([A-Za-z0-9]{16,})\b`), group: 1},
	// Anthropic / OpenAI 风格的 sk- 密钥（前缀保留）。
	{re: regexp.MustCompile(`\b(sk-[A-Za-z0-9\-_]{16,})\b`), group: 1},
	// Slack bot/app/user token（前缀保留）。
	{re: regexp.MustCompile(`\b(xox[abprs]-)([A-Za-z0-9\-]{10,})\b`), group: 2},
	// JWT / JWS：`eyJ` 开头的三段 base64url（`eyJ` 是 `{"a`/`{"t` 的
	// base64，任何 JSON 头的 JWT 都以它开头）。整段抹掉 —— 三段一起才是
	// 凭据，留着 header 段等于告诉人「这里有 token」之外还漏了算法与
	// kid；末段允许为空（未签名的 JWT）。
	{re: regexp.MustCompile(`\b(eyJ[A-Za-z0-9\-_]*\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]*)\b`), group: 1},
	// AWS access key id：整段抹掉（AKIA/ASIA 这 4 位与后面 16 位一起才
	// 构成 key id）。
	{re: regexp.MustCompile(`\b((?:AKIA|ASIA)[A-Z0-9]{16})\b`), group: 1},
	// 带 scheme 的连接串口令段（含 `redis://:pw@host` 这种空用户名形态）。
	{re: regexp.MustCompile(urlCredSchemeRe), group: 2},
	// 无 scheme 但主机是 IP 形状的连接串口令段。
	{re: regexp.MustCompile(urlCredHostRe), group: 2},
	// kv 形态之一：**带引号的值**。三个变体各对应一类真实文本：
	//
	//   `token:"x"`            → 值带引号、键不带
	//   `"apiKey":"x"`         → JSON（键带引号，且键名后直接跟 `:`）
	//   `'password': 'x'`      → 单引号风格
	//
	// 键允许带引号是必需的：不带时 `\s*[:=]` 接不上 JSON 里「引号 + 冒号」
	// 那段，`{"apiKey":"…"}` 会整体漏出去。值允许转义引号见 kvValue。
	// 引号值不看长度（两重信号够了），但仍跳过 null/true/default 这类
	// 结构字面量。
	{
		re:    regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])["']?(` + kvKeyRoots + `)["']?\s*[:=]\s*(` + kvValue + `)`),
		group: 2,
		skip:  notASecretLiteral,
	},
	// kv 形态之二：**裸值**（`token=hunter2hunter2`、`password: hunter2`）。
	//
	// 值必须是「像随机串」的连续字符（见 kvBareValue），长度不足
	// minBareValueLen 或含结构字符都不动 —— 这一支是误伤的主要来源，
	// 保守优先。
	{
		re: regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|[^A-Za-z0-9])["']?(`+kvKeyRoots+
			`)["']?\s*[:=]\s*(`+kvBareValue+`)(?:[\s,;}\])"']|$|(?://))`, minBareValueLen)),
		group: 2,
		skip:  notASecretLiteral,
	},
}

// isRedactedValue 报告这段文本是不是「已经脱敏过」的产物。
//
// 两级脱敏会互相看得见：环境值替换留下的 redactedMark 本身就是一个
// 「token=…」形态的值，形态兜底若不跳过它就会写成 [REDACTED][REDACTED]。
//
// 这是**防御性**的：当前几条形态断言即使不跳过也恰好得到同样结果（标记
// 里的方括号不在裸值字符集内），所以它挡的是「将来把某个形态的字符集放宽」
// 这类改动 —— 有单测直接钉住这个函数本身。
func isRedactedValue(s string) bool {
	return strings.Trim(s, `"'`) == redactedMark
}

// redact 把这一段文本里该条目命中的「值」换成 redactedMark。
//
// 按字节区间做替换（而不是给 ReplaceAllString 写 `${1}` 模板）：模板没法
// 表达「保留匹配到的其它部分」，而这里每条的形状都不一样。
func (sh tokenShape) redact(s string) string {
	matches := sh.re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, m := range matches {
		i := 2 * sh.group
		if i+1 >= len(m) || m[i] < 0 {
			continue // 该组没参与匹配（可选组），整段不动
		}
		b.WriteString(s[prev:m[i]])
		val := s[m[i]:m[i+1]]
		// 上一级已经抹过这一段（环境值替换留下的标记本身长得就像
		// `token="[REDACTED]"`），或这段只是结构字面量 —— 原样保留。
		if isRedactedValue(val) || (sh.skip != nil && sh.skip(val)) {
			b.WriteString(val)
		} else {
			b.WriteString(redactedMark)
		}
		prev = m[i+1]
	}
	b.WriteString(s[prev:])
	return b.String()
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

	// 3) 形态兜底。每个条目只抹它标出的那一段，其余（前缀、`user:`、
	// `@host`、引号、JSON 结构字符）原样保留 —— 见 tokenShape 的注释。
	for _, shape := range tokenShapes {
		s = shape.redact(s)
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

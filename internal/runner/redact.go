package runner

// redact.go 「验证输出摘要」落库前的脱敏（§3.6 第 16 条的一部分）。
//
// ---------------------------------------------------------------------------
// 两层：权威层在前，形态表兜底
// ---------------------------------------------------------------------------
//
// 四轮审查的教训是同一件事：**只靠形态猜「哪些串像凭据」收敛不了**——
// 每修好一类就会出现另一类（邮箱被当连接串、JSON 收尾被吞、转义引号漏、
// 连续键值对只抹第一个）。所以这里按可靠性分层，顺序就是优先级：
//
//	层 1（权威）knownCredentialValues + redactKnown
//	    平台在**运行期已知**的凭据值，按字节精确子串替换。
//	    判据不依赖形态：值本身是从环境/隔离栈连接串里拿到的。
//	    覆盖主线的绝大部分 —— 子进程回显 env 正是 leak 的头号形态。
//
//	层 2（兜底）tokenShapes
//	    形态识别，盖那些「不在本进程环境里、但看得出是凭据」的值
//	    （agent 抄进代码的 token、上游日志里的 key）。**尽力而为，非穷举**。
//
// 分层不只是组织方式：层 1 不受层 2 那些判据的约束 —— 值被 8KB 尾窗砍成
// 残段、出现在域名主机的 DSN 里、挂在自定义变量名下，只要它是平台已知的
// 凭据值，层 1 都能精确命中。层 2 的边界因此不该被当作「通过条件」。
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
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// 已知接受的边界（**这些不是通过条件**，列出来免得被当成「已经保证干净」）
// ---------------------------------------------------------------------------
//
// 权威层（层 1）：
//   - **登记口径按来源不同**：
//       · 装配侧 registered（`config.KnownSecrets`）与进程环境里名字命中词根的
//         变量：登记**整串** + 它的口令段。名字/来源已经说明它是凭据。
//       · 验证步骤的 `StackEnv`：**只登记含 `user:pass@` 的 DSN 值与它解析出的
//         口令段**，绝不逐值全登记。那张表里还有 `DATABASE_HOST=127.0.0.1`、
//         `DATABASE_PORT`、`DATABASE_USER=lathe` 这类非凭据的运行参数，
//         登记它们有两个后果，都已实测：误伤正常排障信息
//         （`dial tcp 127.0.0.1:56001: refused` 里的主机没了），以及**自伤**
//         —— 层 1 先把主机抹成标记，层 2 要求 `@<host>:<port>` 的规则随即
//         匹配不上，口令反而明文留下。
//   - **权威层固有的误伤面**：全量精确替换意味着**任意与凭据值同值的正常文本
//     都会被抹**，不只是名字级 —— 某个凭据值恰好等于一段普通字符串时，
//     那段文本会被替换掉。这是「不猜形态」的另一面代价，接受它是为了换
//     主线上的确定性。
//   - 环境值/登记值**短于 8 字节**不参与替换（短串会让普通输出满屏标记）。
//   - 无下划线连写的别名（`MYSQLPWD`、`DBPASS`、`PGPASS`）**不被覆盖** ——
//     太容易命中无关词（`COMPASS`、`BYPASS`）。登记在案，不修。
//
// 形态表（层 2，非穷举）：
//   - 无键值结构的口令（`the password is hunter2`）。
//   - PEM 私钥块（`-----BEGIN ... PRIVATE KEY-----`）、`file:///…` 里的路径。
//   - 裸值 < 8 字节，或值里含 `\` / 多字节字符（裸值字符集只收
//     `[A-Za-z0-9_\-./+=]`）。
//   - 结构字面量白名单（`null`/`true`/`false`/`default`/纯数字）**刻意不抹**。
//   - 无 scheme 的连接串要求**主机带端口/路径/`tcp(`/IPv6 括号**；因此
//     `dial lathe:pw@127.0.0.1 timeout`（无端口无路径）**不抹口令** ——
//     收的话就得接受 `a:b@c.com` 这类也进来。带 scheme 的 DSN 不受此限。
//   - `a:b@c.com` 判为邮箱形态**不抹**；反例 `noreply@10.0.0.1`（无冒号）同样不抹。
//   - **尾窗恰好砍在口令内部**（只剩口令的后半截）：残段里没有任何结构线索，
//     形态表接不上；权威层也接不上（登记的是完整口令）。属已知边界。
//
// 摘要本身：
//   - 它会随提示词经 agent.go 的 buildArgs 作为 **argv** 传给 CLI，因此不只
//     存在于库里：同机进程能通过 `/proc/<pid>/cmdline` 读到。（agent 子进程
//     的环境是白名单化的，见 agent.go 的 sanitizedEnv；argv 不受它保护。）
//   - 摘要是 verify 已**头部**截到 16KB 那份的尾部，取舍在 verify 侧而不是
//     下面的 tail。

import (
	"net/url"
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

// ---------------------------------------------------------------- 层 1：权威层

// secretKeyRe 命中「变量名本身说明它是敏感值」的环境变量。
//
// 与 preview 的 passwordClassRe 同一套词根，但**刻意比它窄**，而且各写一份：
// 那一份服务的是「哪些必填变量由平台自动生成口令」，改它的词表不该牵动脱敏。
//
// 收词根的标准是「这个名字下的值**就是**凭据」，不是「可能跟认证有关」。
// 两个方向都有实测教训：
//
//   - **太宽**：早先收过 AUTH 与笼统的 URL，于是 `LATHE_AUTH_MODE=disabled`
//     把 `auth mode is disabled` 抹掉、`LATHE_BASE_URL` 把
//     `https://host/tasks/1` 抹掉。
//   - **太窄**：`MYSQL_PWD`、`LATHE_DB_PASS`、`LATHE_SESSION_KEY`、
//     `LATHE_GH_PAT` 的值原样落库 —— 这类名字下的值都是随机串，形态表认不出。
//
// `PWD`/`PASS` 必须**带前置下划线**（`_PWD$`/`_PASS$`）：带前缀的形式才是
// 凭据别名（`MYSQL_PWD`、`LATHE_DB_PASS`）；裸的 `PWD` 是 POSIX shell 的
// 工作目录变量，而它的值会作为**步骤目录**出现在摘要里 —— 「哪一步、在哪跑」
// 正是摘要存在的意义，抹了它等于把排障信息换成一行 [REDACTED]。
// 下划线锚定同时排掉 `OLDPWD`、`SSH_ASKPASS`、`BYPASS_CACHE`、
// `COMPASS_HOME`、`LATHE_PASSIVE_MODE`（PWD/PASS 前面都不是 `_`）。
var secretKeyRe = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|CREDENTIAL|SIGNING_?KEY|SESSION_?KEY|ENCRYPTION_?KEY|_PAT$|_PWD$|_PASS$)`)

// urlishKeyRe 命中值是 URL/DSN 的变量名：`postgres://user:pass@host/db` 的
// 口令段不由上面那组词根命名，但一样是凭据。
//
// **不收 BASE_URL / *_URL 这类通用地址**：`LATHE_BASE_URL` 是实例地址，
// 里面没有口令，而它的值会出现在大量正常输出与断言消息里。收窄的依据是
// 「值里必然带 user:pass@」—— DSN 这一类名字才具备这个性质。
var urlishKeyRe = regexp.MustCompile(`(?i)(^|_)(DSN|DATABASE_URL|DATABASE_URI|REDIS_URL|AMQP_URL|BROKER_URL|CONNECTION_STRING)`)

// dsnUserinfoRe 从一段连接串里抠出 `user:pass@` 的口令段。
//
// url.Parse 能处理标准 DSN，但兼容不了 `user:pw@tcp(host)/db` 这类非 URL
// 形态，也接不住被截断的残段 —— 两个都试，命中即登记。
var dsnUserinfoRe = regexp.MustCompile(`[^\s/:@]+:([^\s/:@]+)@`)

// minSecretLen 是「值得脱敏的环境值」的最短长度。
//
// 低于它的值不抹：抹 1–7 字节的短串（"1"、"yes"、"true"、"linux"）会把正常
// 输出打成筛子，而真实凭据（PAT、JWT、32 位十六进制口令、带口令的 DSN）
// 都远长于此。
const minSecretLen = 8

// knownCredentialValues 汇总这一轮脱敏可用的「已知凭据值」（层 1 的输入）。
//
// 三个来源，**登记口径各不相同**：
//
//	registered  装配侧显式登记的整串（配置里的 DB DSN / 集成 token）。
//	            整串登记 + 口令段：整串替换把 DSN 一笔抹掉，口令段兜住
//	            被 8KB 尾窗砍头的残段。
//	extraEnv    验证步骤的额外环境（Step.StackEnv）。**只登记含 `user:pass@`
//	            的 DSN 值及其口令段**，绝不逐值全登记 —— 那张表里还有
//	            `DATABASE_HOST=127.0.0.1`、`DATABASE_PORT`、`DATABASE_USER=lathe`
//	            这类**非凭据**的运行参数，它们会成片出现在正常排障输出里
//	            （`dial tcp 127.0.0.1:56001: refused` 就是要看的那条信息）。
//	            两个理由使它必须收窄：
//	              · 误伤：把回环地址/端口抹掉，摘要里「连的是哪个库」就没了；
//	              · 更坏的是**自伤**：层 1 先把主机抹成 [REDACTED]，层 2 的
//	                无 scheme 规则（要求 `@` 后是带端口/路径/域名的主机）
//	                就再也匹配不上 —— 于是口令明文留下。收窄主机登记即消除。
//	os.Environ  本进程环境里**名字命中词根**的变量（名字本身就是判据）。
//
// 名字命中词根的变量按名登记（`DATABASE_URL=…` 在内），那是刻意的：
// 名字已经说明它是连接串/凭据。
func knownCredentialValues(registered []string, extraEnv map[string]string) []string {
	seen := map[string]bool{}

	add := func(v string) {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen || seen[v] {
			return
		}
		seen[v] = true
	}

	// addDigest 登记一个「已知凭据值」：整串 + 从它里面解析出的口令段。
	//
	// 口令段单独登记的必要性：8KB 尾窗砍头会产生只剩
	// `lathe:REALVERYSECRETPW@db.internal:5432/app` 的残段，整串精确替换
	// 接不上，但口令段本身仍然接得上。
	addDigest := func(v string) {
		v = strings.TrimSpace(v)
		add(v)
		add(passwordOf(v))
	}

	for _, v := range registered {
		addDigest(v)
	}
	for name, val := range extraEnv {
		// 名字命中词根 → 整串算凭据（`DATABASE_URL` 这类）
		if secretKeyRe.MatchString(name) || urlishKeyRe.MatchString(name) {
			addDigest(val)
			continue
		}
		// 其余一律**不登记整串**，只取「值里带口令」的那一种
		// （`user:pass@` 形态）。见上面 extraEnv 那段的两个理由。
		add(passwordOf(val))
	}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name, val := kv[:i], kv[i+1:]
		if secretKeyRe.MatchString(name) || urlishKeyRe.MatchString(name) {
			addDigest(val)
		}
	}

	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	// 长的先替：短值可能是长值的子串，先替短的会把长值切成两段、
	// 让长值再也匹配不上。
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// passwordOf 从一段值里抠出任一形态携带的口令段（没有则返回空串）。
//
// 两条分支各覆盖一半，**缺一不可**：
//
//	url.Parse  标准 URL 形态（`postgres://user:pw@host/db`）。它做的是
//	           百分号解码与前缀校验，`mysql://u:p@/db` 这类也走它。
//	dsnUserinfoRe  非 URL 形态（`user:pw@tcp(host)/db`、被砍头后不含
//	           scheme 的残段）—— url.Parse 会因为没有 scheme 而失败。
//
// 单测分别钉住两条（TestPasswordOfURLForm / TestPasswordOfNonURLForm）。
func passwordOf(v string) string {
	v = strings.TrimSpace(v)
	if u, err := url.Parse(v); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			return pw
		}
	}
	if m := dsnUserinfoRe.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return ""
}

// redactKnown 是层 1：把已知凭据值按字节精确替换。
func redactKnown(s string, values []string) string {
	if len(values) == 0 {
		return s
	}
	replacers := make([]string, 0, 2*len(values))
	for _, v := range values {
		replacers = append(replacers, v, redactedMark)
	}
	return strings.NewReplacer(replacers...).Replace(s)
}

// ---------------------------------------------------------------- 层 2：形态表

// literalValueRe 命中「明显不是凭据」的字面量：JSON 的 null/true/false、
// 各种空值写法、纯数字、以及占位词。命中就不抹。
//
// `{"token":null}`、`{"input_token":5000}`、`{"credentials":"default"}` 都不该
// 被当成凭据 —— 键名像是敏感名，值却是结构字面量，抹掉只会损坏 JSON 且丢信息。
var literalValueRe = regexp.MustCompile(`(?i)^(null|true|false|none|nil|default|undefined|nan|\[redacted\])$|^[+-]?\d+(\.\d+)?$`)

// urlCredSchemeRe 是**带 scheme** 的连接串口令段（捕获组 2）。
//
// `://` 本身就是决定性证据，右侧一律认（主机是域名还是 IP 都行），用户名
// 允许为空 —— 覆盖 `postgres://lathe:pw@host`、`redis://:pw@host`、
// `mysql://user:pw@db.internal:3306/x`。
const urlCredSchemeRe = `[a-zA-Z][a-zA-Z0-9+.\-]*://([^\s/:@]*):([^\s/:@]+)@`

// urlCredHostRe 是**无 scheme** 的连接串口令段（捕获组 2）。
//
// 验证输出里常常只剩这一段（`dial lathe:pw@127.0.0.1`、被尾窗砍头的
// `e:PW@db.internal:5432/app`），必须覆盖；但没有 `://` 就没有证据，只能靠
// 两侧形状把它与邮箱分开：
//
//	左侧（userinfo）  只收 `[A-Za-z0-9._%+\-]` —— 引号、花括号、逗号都不在
//	                  里面，于是 `{"user":"a@b.com"}` 里那个 `@` 左边是
//	                  `"user":"a`（含引号）构不成匹配。JSON 里的邮箱因此
//	                  天然被排除（这是上一版靠「主机必须是 IP」来避让的，
//	                  代价是把 `@db.internal` 这类域名主机整类漏掉）。
//	右侧（host）      要求带端口、路径、`tcp(` 参数或 IPv6 括号 ——
//	                  `a:b@c.com` 这种裸域名不是连接串（判为邮箱形态，
//	                  见文件头的边界清单）。
//
// 前导字符类不是 lookbehind（RE2 没有），用「吃掉一个前置字符」代替：
// 该字符不在替换组里，原样保留。
const urlCredHostRe = `(?:^|[\s(\[{,;='"|])([A-Za-z0-9._%+\-]+):([^\s/:@'"]+)@` +
	`(?:` +
	`(?:(?:[A-Za-z0-9\-]+\.)+[A-Za-z0-9\-]+|localhost|(?:\d{1,3}\.){3}\d{1,3}):\d+` + // host.tld:5432
	`|(?:[A-Za-z0-9\-]+\.)+[A-Za-z0-9\-]+/` + // host.tld/path
	`|tcp\(` + // mysql 参数形态 user:pw@tcp(host:3306)/db
	`|\[[0-9a-fA-F:]+\]` + // [::1]:6379
	`)`

// kvKeyRoots 是 kv 形态的**键名**词根（文本级）。与 secretKeyRe 是两码事
// （那个筛环境变量名），但两处收的词必须一致 —— 否则「文本里出现、值不在
// env 中」的那一半就是空白（`LATHE_ENCRYPTION_KEY=…` 实测漏过）。
//
// `pwd`/`pass`/`pat` 写成「必须有前缀下划线」（`[a-z0-9_]*_(?:pwd|pass|pat)`）：
// 带前缀的才是凭据别名（`MYSQL_PWD=`、`LATHE_DB_PASS=`），裸的 `PWD=` 是
// 环境转储里的工作目录 —— `docker inspect Env` 那种输出里抹掉它，等于把
// 「在哪跑」换成了 [REDACTED]。键名后面必须紧跟 `[:=]` 这一点另有作用：
// `PASSIVE_MODE=`、`BYPASS=` 都接不上分隔符，天然不命中。
const kvKeyRoots = `password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|` +
	`signing[_-]?key|session[_-]?key|encryption[_-]?key|credentials?|` +
	`[a-z0-9_]*_(?:pwd|pass|pat)`

// kvValue 是带引号的值：允许 `\"`/`\'` 转义。
//
// 老写法 `"[^"]*"` 会在第一个转义引号处截断，于是
// `{"token":"abc\"defghijklmno"}` 只抹掉 `abc\"` 留下一截明文 ——
// 看起来已经脱敏，实际上口令还在（§7 红线内的真泄漏）。
const kvValue = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`

// kvBareValue 是裸值：没有引号包裹，只有键名一个信号。
//
// 字符集限定在凭据常用的那几类（字母数字 + `-_./=+`）且**至少 8 字节** ——
// 引号值没有长度门槛（`"token":"x"` 也抹，引号 + 键名两重信号够了），
// 裸值只有键名一重信号、又和普通词混在一起（`token: expired`、
// `password: <placeholder>`），所以要求它至少像个随机串。
//
// 这个字符集同时是**终止条件**：不接受 `,;}` 等结构字符、也不接受空白，
// 于是**不再跟一个收尾守卫**。早先跟了 `(?:[\s,;}\"']|$)`，那是个消费类，
// 匹配后从它之后继续扫，同一段里的第二个键值对永远匹配不上。
//
// 早先的写法是 `[^\s,;]{4,}` 后面跟 `(?:[\s,;}\])"']|$)`：那个类是**消费类**，
// 匹配后从它之后继续扫，于是同一段里的第二个键值对永远匹配不上
// （`TOKEN=a\nSECRET=b` 只抹第一个，`?api_key=a&x=2` 整条不匹配）。这正是
// 子进程回显 env 的头号形态，等于「同一段输出的第二行起全漏」。
const kvBareValue = `[A-Za-z0-9_\-./+=]{8,}`

// notASecretLiteral 是 tokenShape.skip 的实现：明显不是凭据的值不抹。
func notASecretLiteral(v string) bool {
	return literalValueRe.MatchString(strings.Trim(v, `"'`))
}

// tokenShape 是凭据形态表的一格：一条正则 + 「要抹掉的那一段」是哪个捕获组。
//
// 显式标出组号，而不是靠「组数猜语义」—— 早先版本就是后者，于是
// `\b(?:AKIA|ASIA)([A-Z0-9]{16})\b`（唯一没把整段圈进组的条目）落进
// 「保留前缀」分支，把 AWS key id 的前 4 位与后 16 位一起原样吐了出去。
type tokenShape struct {
	re *regexp.Regexp
	// group 是「值」所在的捕获组。命中这一段以外的部分（包括同一捕获组
	// 前后的字面量）原样保留：`ghp_` 这类前缀留着是有用的排障信息，
	// `postgres://user:` 与 `@host` 留着能让摘要仍然可读。
	group int
	// skip 非空时，对捕获到的值再判一次：返回 true 就不抹（值太像结构
	// 字面量/普通词）。nil 表示一律抹。
	skip func(value string) bool
}

// tokenShapes 是凭据形态的兜底表（层 2）。
//
// **非穷举** —— 见文件头「已知接受的边界」。这一层的价值在于「多盖一层」
// 那些运行期不可知的值，而不是「盖全」；真正的保证来自层 1。
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
	// 凭据；末段允许为空（未签名的 JWT）。
	{re: regexp.MustCompile(`\b(eyJ[A-Za-z0-9\-_]*\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]*)\b`), group: 1},
	// AWS access key id：整段抹掉（AKIA/ASIA 这 4 位与后面 16 位一起才
	// 构成 key id）。
	{re: regexp.MustCompile(`\b((?:AKIA|ASIA)[A-Z0-9]{16})\b`), group: 1},
	// 带 scheme 的连接串口令段（含 `redis://:pw@host` 空用户名形态）。
	{re: regexp.MustCompile(urlCredSchemeRe), group: 2},
	// 无 scheme 的连接串口令段（判据见 urlCredHostRe）。
	{re: regexp.MustCompile(urlCredHostRe), group: 2},
	// kv 形态之一：**带引号的值**（`token:"x"`、`"apiKey":"x"`、`'password': 'x'`）。
	// 引号值不看长度（两重信号够了），但仍跳过结构字面量。
	{
		re:    regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])["']?(` + kvKeyRoots + `)["']?\s*[:=]\s*(` + kvValue + `)`),
		group: 2,
		skip:  notASecretLiteral,
	},
	// kv 形态之二：**裸值**（`token=hunter2hunter2`、`password: hunter2`）。
	// 值必须是「像随机串」的连续字符（见 kvBareValue），长度不足
	// minBareValueLen 或含结构字符都不动 —— 这一支是误伤的主要来源。
	{
		re:    regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])["']?(` + kvKeyRoots + `)["']?\s*[:=]\s*(` + kvBareValue + `)`),
		group: 2,
		skip:  notASecretLiteral,
	},
}

// isRedactedValue 报告这段文本是不是「已经脱敏过」的产物。
//
// 两级脱敏会互相看得见：层 1 替换留下的 redactedMark 本身就是一个
// 「token=…」形态的值，形态兜底若不跳过它就会写成 [REDACTED][REDACTED]。
//
// 这是**防御性**的：当前几条形态断言即使不跳过也恰好得到同样结果（标记
// 里的方括号不在裸值字符集内），所以它挡的是「将来放宽某个形态的字符集」
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
		// 上一级已经抹过这一段，或这段只是结构字面量 —— 原样保留。
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

// redactText 是两层的总入口：先权威层，后形态兜底。
//
// registered 是装配侧登记的已知凭据（Pipeline.KnownSecrets）；
// extraEnv 是这次验证步骤的额外环境（Step.StackEnv）。
func redactText(s string, registered []string, extraEnv map[string]string) string {
	if s == "" {
		return ""
	}
	s = redactKnown(s, knownCredentialValues(registered, extraEnv))
	for _, shape := range tokenShapes {
		s = shape.redact(s)
	}
	return s
}

// redactSecrets 是 redactText 的「无登记值」入口（测试与兼容调用用）。
func redactSecrets(s string, env map[string]string) string {
	return redactText(s, nil, env)
}

// ---------------------------------------------------------------- 截断

// tail 取字符串的尾部，按**字节**上限截断，且不切断多字节 UTF-8 字符。
//
// 两个刻意的选择：
//
//   - **尾部**而非头部：验证输出（go test / 编译器 / panic）把结论写在最后
//     —— FAIL 行、断言消息、栈底。取头部恰好把最有用的那几行挤掉。
//     （代价是左边界会切出 DSN 残段 —— 那正是权威层要精确命中的形态之一。）
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
// 返回值一定是脱敏过的（权威层用本流水线登记的已知凭据 + 这一步的隔离栈
// 连接串）—— 它只往库里和提示词里去。
func (p *Pipeline) failureExcerpt(report Report) string {
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

	return tail(redactText(b.String(), p.KnownSecrets, f.Step.StackEnv), maxDigestStoredExcerpt)
}

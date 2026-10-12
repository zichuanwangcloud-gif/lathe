package prd

// stall.go 搁置扫描器：把「挂着问题没人回答」的 PRD 转 abandoned，
// 并发一封提醒邮件（docs/10 §9 未决第 1 条）。
//
// ---------------------------------------------------------------------------
// 为什么需要它
// ---------------------------------------------------------------------------
//
// 规划对话的形态是「智能体提问 → 挂起等人答」（§1.3 的 A–E 主循环）。
// awaiting_answers 是这条流水线上唯一一个「等人」的状态，而在此之前
// 没有任何东西管它的寿命：人不答，这份 PRD 就永远躺在 awaiting_answers，
// 既不占资源也不产生输出，只是安静地陈旧下去。列表页按状态筛能看见一堆
// 待回答，但分不出哪份是「人正在想」、哪份是「人已经把这事忘了」。
//
// 转 abandoned 不是惩罚，是把「停了」这件事记下来：它让列表有意义、
// 让属主被提醒一次、也让「那份为什么没做」有据可查（而不是一份永远挂在
// 待回答里的 PRD）。
//
// ---------------------------------------------------------------------------
// 为什么不复用 runner 的收割机那套
// ---------------------------------------------------------------------------
//
// 与收割机（runner/reaper.go）形似而神不同，刻意不共用一个「轮询器框架」：
//
//   - 收割机删的是磁盘上的目录与 git 分支，它的绝大部分复杂度都在
//     「怎么不误删」。这里只改一行状态字段，没有不可逆的破坏面。
//   - 收割机必须原子认领（ClaimForReap）才敢碰磁盘；这里的 CAS 就是
//     状态转移本身（TransitionPRD 带 From 守卫），不需要第二套机制。
//   - 两者扫描的判据、周期、失败代价都不一样。抽一个共同父类换来的
//     只是「都是 ticker + ctx.Done」这点形状上的相似，不值。
//
// 相似的是装配方式（照 MergePoller / WorktreeReaper：ticker + ctx.Done，
// 单轮失败只 warn，由 cmd/lathe 启动），这一层在 main.go 里对齐。
//
// ---------------------------------------------------------------------------
// 通知为什么不能影响状态流转
// ---------------------------------------------------------------------------
//
// 同 notify.go 第 2 条约束：发信是副作用，不是流程的一部分。SMTP 没配、
// 投递失败、收件人查不到 —— 一律只记日志，该转的状态照转。把通知做成
// 能让状态卡住的东西，等于用一个「锦上添花」的功能给主流程加了新失败点。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// defaultStallSweepInterval 是扫描间隔。
//
// 一小时一次：判定阈值是天级的，扫得再勤也只是白占连接（同收割机）。
const defaultStallSweepInterval = time.Hour

// defaultStallTimeout 是搁置阈值的兜底值（七天）。
//
// 与 config 的默认值一致，但这里再声明一份：Sweeper 可能被测试或别的
// 调用方直接构造（绕过 config），那时 Timeout 留零值必须有意义 ——
// 零值在下面被解释成「用默认」，而不是「立刻超时」。
const defaultStallTimeout = 168 * time.Hour

// StallStore 是扫描器需要的持久化能力（窄接口，便于注入假件）。
type StallStore interface {
	// ListStalledPRDs 返回停在 awaiting_answers 且最后一轮早于 cutoff 的 PRD。
	ListStalledPRDs(ctx context.Context, now, cutoff time.Time, limit int) ([]store.PRDRow, error)
	// TransitionPRD 落一次状态转移。From 是 CAS 比较项：不匹配则失败，
	// 这正是「同一份 PRD 不会被两轮扫描重复搁置」的保证。
	TransitionPRD(ctx context.Context, id, userID int64, p store.TransitionPRDParams) (*store.PRDRow, error)
}

// StallNotifier 给 PRD 属主投一封搁置提醒。
//
// 与 runner.TaskMail 同一套设计：实现方负责解析收件人与 SMTP 可用性，
// **未配置 SMTP 时返回 nil（静默跳过）而非错误**。
type StallNotifier interface {
	NotifyPRDStalled(ctx context.Context, prdID int64, subject, body string) error
}

// StalledPRDMail 渲染搁置提醒的主题与正文。
//
// 抽成纯函数，照 terminalMail 的惯例（notify.go 写明同样的理由：
// 能脱离 SMTP 测正文里确实带上了该带的东西）。
//
// timeout 写进正文而不是只写「已搁置」：人收到信要知道「多久没理它」
// 才知道这事有多严重、也从哪儿算起。baseURL 为空则省略详情行。
func StalledPRDMail(baseURL string, prdID int64, timeout time.Duration) (subject, body string) {
	subject = fmt.Sprintf("[Lathe] 模糊任务 #%d 已搁置", prdID)

	var b strings.Builder
	fmt.Fprintf(&b, "模糊任务 #%d 已连续 %s没有人回答，已转为「已搁置」。\n",
		prdID, humanDuration(timeout))
	b.WriteString("\n它没有丢：回到详情页答一句（或直接点「继续这份 PRD」）就能接着推进，" +
		"之前的对话与已确认的节都还在。\n")

	if baseURL != "" {
		fmt.Fprintf(&b, "\n详情：%s/prds/%d\n", strings.TrimRight(baseURL, "/"), prdID)
	}
	return subject, b.String()
}

// humanDuration 把时长说成人话（7 天而不是 168h0m0s）。
//
// 只处理天与小时两档：搁置阈值最短 1 小时、默认 7 天，落在这一档里。
// 不做通用的时长美化 —— 那是个没人维护得住的函数，而这里只需要
// 「读起来像人说的话」。
func humanDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		days := int(d.Hours() / 24)
		// 28 天不是一个月：搁置阈值本来就是「多久没动静」，
		// 用「4 周」比「1 个月」更贴事实，也避免踩月份天数的坑。
		if days%7 == 0 && days >= 7 {
			return fmt.Sprintf("%d 周", days/7)
		}
		return fmt.Sprintf("%d 天", days)
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}

// StallSweeper 按阈值把等人回答的 PRD 转成搁置。
type StallSweeper struct {
	Store StallStore
	// Notifier 为 nil 或 notify 为 false 时只转状态、不发信
	// （LATHE_PRD_STALL_NOTIFY=false）。
	Notifier StallNotifier
	// Notify 为假时跳过发信。单独一个字段而不是靠 Notifier==nil：
	// 装配层始终会传一个真的 notifier，关不关是配置的事。
	Notify bool

	// Timeout 是搁置阈值；<=0 时取 defaultStallTimeout。
	Timeout time.Duration
	// Interval 是扫描间隔；<=0 时取 defaultStallSweepInterval。
	Interval time.Duration

	// BaseURL 用于在邮件里拼详情链接（config.PublicURL）。
	BaseURL string

	// Now 可注入，便于测试（假时钟，不许 sleep）。
	Now func() time.Time
}

func (s *StallSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// effectiveTimeout 返回生效的阈值：零值解释成「用默认」而不是「立刻超时」。
//
// 这里刻意【不】兜下限，与收割机的 effectiveTTL 不同。收割机必须自己兜，
// 因为 TTL 在那里是孤儿清扫的安全机制（配错会误删在途现场）；而搁置阈值
// 的下限纯粹是防手滑，已经在 config.Validate 里拦住了（配得更小启动就
// 报错）。在这里再夹一道唯一的后果是把「配置没生效」掩盖掉 ——
// Sweeper 被绕过 config 直接构造时，调用方本来就该自己给出合理的值。
func (s *StallSweeper) effectiveTimeout() time.Duration {
	if s.Timeout <= 0 {
		return defaultStallTimeout
	}
	return s.Timeout
}

// Run 启动轮询循环，直到 ctx 取消。
//
// 结构照 WorktreeReaper.Run / MergePoller.Run：ticker + ctx.Done 优雅退出，
// 单轮失败只 warn。
//
// 刻意不在启动时立刻扫一轮：那是服务起来最忙的时刻（Reconcile 刚把所有
// 在途任务重新入队），而搁置判定是天级的，等一个 tick 不会有任何差别。
func (s *StallSweeper) Run(ctx context.Context) {
	timeout := s.effectiveTimeout()
	interval := s.Interval
	if interval <= 0 {
		interval = defaultStallSweepInterval
	}
	slog.Info("PRD 搁置扫描器已启动",
		"timeout", timeout, "interval", interval, "notify", s.Notify && s.Notifier != nil)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("PRD 搁置扫描器停止")
			return
		case <-ticker.C:
			if n, err := s.SweepOnce(ctx); err != nil {
				slog.Warn("PRD 搁置扫描轮次失败", "err", err)
			} else if n > 0 {
				// 只在真搁置了东西时打 info（照 gcSessions / 收割机的做法）：
				// 一小时一条「搁置了 0 份」是纯噪声。
				slog.Info("PRD 搁置扫描完成", "stalled", n)
			}
		}
	}
}

// SweepOnce 扫一轮并搁置超期 PRD，返回本轮搁置的数量。
//
// 导出是为了让测试直接驱动一轮，不必等 ticker（同 ReapOnce）。
func (s *StallSweeper) SweepOnce(ctx context.Context) (int, error) {
	if s.Store == nil {
		return 0, nil
	}
	timeout := s.effectiveTimeout()
	now := s.now()
	cutoff := now.Add(-timeout)

	rows, err := s.Store.ListStalledPRDs(ctx, now, cutoff, stallScanLimit)
	if err != nil {
		return 0, fmt.Errorf("prd: 查询超期 PRD 失败: %w", err)
	}

	stalled := 0
	for _, row := range rows {
		// 单个 PRD 失败只 warn 并继续 —— 不让一份坏数据毁掉整轮
		// （照 MergePoller.pollOnce 的容错立场）。
		if s.stall(ctx, row, timeout, now) {
			stalled++
		}
	}
	return stalled, nil
}

// stall 搁置一份 PRD，报告本轮是否真的转了。
//
// 顺序是刻意的：**先 CAS 转移，转成功了才发信**。
//
// 反过来（先发信再转移）会带来两个问题：一是 CAS 失败时信已经发出去了
// （人收到「你的 PRD 被搁置了」但状态没变，纯误导）；二是幂等没了 ——
// 两轮扫描都发信。现在这个顺序让 CAS 天然成为幂等闸门：
// awaiting_answers → abandoned 这一条边一辈子只成功一次，失败的那次
// 什么都不做。不需要额外记「已通知过」的列。
//
// now 由 SweepOnce 传进来而不是就地再取一次：同一轮里所有判定必须基于
// 同一个瞬间，否则「cutoff 说它超期、日志说的时间对不上」这种偏移会在
// 排障时把人绕进去。
func (s *StallSweeper) stall(ctx context.Context, row store.PRDRow, timeout time.Duration, now time.Time) bool {
	_, err := s.Store.TransitionPRD(ctx, row.ID, row.UserID, store.TransitionPRDParams{
		From: string(StateAwaitingAnswers),
		To:   string(StateAbandoned),
	})
	if err != nil {
		if errors.Is(err, store.ErrPRDStateChanged) {
			// 人在这两轮之间答了（或自己放弃了）—— 这正是 CAS 该拦住的事，
			// 不是错误。不打扰日志。
			return false
		}
		if errors.Is(err, store.ErrPRDNotFound) {
			// PRD 被删了。属主删掉的东西不该留下痕迹。
			return false
		}
		slog.Warn("搁置 PRD 失败（跳过，下一轮再看）", "prd", row.ID, "err", err)
		return false
	}

	// 日志里带上按什么规则、在什么时刻判的 —— 与收割机写进事件流的
	// reason 同一个立场：事后要能回答「它为什么被搁置了」。
	slog.Info("已搁置无回答的 PRD",
		"prd", row.ID, "user", row.UserID, "round", row.Round,
		"timeout", timeout, "cutoff", now.Add(-timeout).UTC().Format(time.RFC3339))

	s.notify(ctx, row, timeout)
	return true
}

// notify 发搁置提醒。失败只记日志，绝不影响已经落好的状态转移。
//
// 同步发信（不像 runner.mailTerminal 那样丢 goroutine）是刻意的：
// 这里是每小时一轮的后台扫描，一轮最多几十份、每份一封信，发信的
// 二十秒预算摊在一小时里无所谓。而 mailTerminal 之所以必须异步，
// 是因为它排在 MergePoller 的串行主路径上（那条路径上同步发信会让
// 一个假死的 SMTP 停掉所有用户的 PR 合并检测）。这里没有那个约束，
// 同步反而让「扫完这一轮」有个明确的下界，测试也好写。
func (s *StallSweeper) notify(ctx context.Context, row store.PRDRow, timeout time.Duration) {
	if !s.Notify || s.Notifier == nil {
		return
	}
	subject, body := StalledPRDMail(s.BaseURL, row.ID, timeout)
	if err := s.Notifier.NotifyPRDStalled(ctx, row.ID, subject, body); err != nil {
		slog.Warn("搁置提醒发信失败（不影响已落的状态）", "prd", row.ID, "err", err)
	}
}

// stallScanLimit 是单轮扫描量上限。存量积压时靠多轮排空（见 ListStalledPRDs）。
const stallScanLimit = 100

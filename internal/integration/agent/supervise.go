package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/capbuf"
)

// supervise.go —— 与 CLI 输出格式无关的进程监管（claude 与 codex 共用这一份）。
//
// 为什么只能有一份：独立进程组、超时/取消杀整棵树、静默看门狗、退出后的
// 兜底扫组、管道宽限期、错误分类 —— 这些都是事故换来的（见各处注释）。
// 每个驱动各抄一套，迟早有一套漏掉其中一条（与 AGENTS.md §7「不写第二套
// 清理逻辑」同理）。驱动只决定「怎么起」（launch）与「怎么读」（parse）。

// launch 是驱动「怎么起进程」的完整描述。supervise 只负责起、看、杀、收，
// 不决定 argv、env、stdin 里写什么。
type launch struct {
	Bin  string
	Args []string
	Dir  string   // 必填；supervise 原样设 cmd.Dir（B2-3）
	Env  []string // 驱动拼好的完整环境（sanitizedEnv() + ExtraEnv + 驱动强制项）
	// Stdin 由驱动提供；nil = 不接 stdin（claude 今天的行为，逐字节不变）。
	//   codex：strings.NewReader(prompt)，读完即 EOF。
	//   f01 的 stream-json：必须传 *os.File（os.Pipe 的读端）。原因：传普通
	//   io.Reader 时 os/exec 会起一条拷贝协程，cmd.Wait() 要等它读到 EOF；
	//   写端不关，Wait 永远不返回。传 *os.File 时子进程直接继承 fd，没有拷贝协程。
	//   supervise 在 Start 之后关闭父进程手里这份读端（与 stdout/stderr 写端同一处理）。（f01 S5）
	Stdin io.Reader
	// Started 在 cmd.Start 成功后、读协程开跑前同步调用一次（可为 nil）。
	//   f01 的 OnReady（交出 Session 句柄）挂在这里。codex 不用。（f01 S4）
	Started func()
	// BeforeKill（f01 S2）：ctx 结束（总超时 / 取消）或静默看门狗触发时，
	//   supervise 在 killProcessGroup 之前同步调用一次（sync.Once 保证两路同时触发也只调一次；
	//   可为 nil）。进程正常退出后的那次「兜底扫进程组」不算，不调用。
	//   f01 在这里关 stdin 写端、置 Session.closed。claude（f06 版）与 codex 都传 nil。
	BeforeKill func()
	// Exited 在 cmd.Wait 返回后、管道宽限前同步调用一次（可为 nil）。
	//   f01 的兜底关闭点（关 stdin 写端、置 Session.closed）。codex 不用。（f01 S4）
	Exited func()
}

// parseHooks（f01 S1）：parse 拿到的两个回调。
//
//	Touch = supervise 的 idle.touch。parse 对**每一条可解析的 JSON 行**先调 Touch，
//	        再决定要不要 Emit——过滤发生在 parse 里，被过滤掉的行也必须喂看门狗。
//	Emit  = supervise 包好的事件出口（内部再调 RunParams.OnEvent）。
//
// supervise 只通过 Touch 喂看门狗，Emit 外层不再 touch：看门狗看到哪些行，
// 完全由 parse 决定，与「哪些行被转发」解耦。
type parseHooks struct {
	Touch func()
	Emit  func(Event)
}

// superviseLimits 是一次运行的两道上限。
type superviseLimits struct {
	// Timeout 是总超时（「跑得久」）。
	Timeout time.Duration
	// Idle 是静默判死阈值（「卡住了」），见 idleTimeout。
	Idle time.Duration
}

// timeoutError 是总超时的错误值：文案与抽出 supervise 之前逐字相同，
// 同时 errors.Is(err, context.DeadlineExceeded) 成立。
//
// 不用 fmt.Errorf("…%w", context.DeadlineExceeded)：那会在文案末尾拼上
// 「: context deadline exceeded」，改变回帖与失败原因里的既有文案。
// f03 的状态判定（killed vs error）依赖 errors.Is，见 f06 PRD §4.1。
type timeoutError struct{ limit time.Duration }

func (e *timeoutError) Error() string {
	return fmt.Sprintf("agent: 执行超时（上限 %v），进程树已回收", e.limit.Round(time.Second))
}

func (e *timeoutError) Unwrap() error { return context.DeadlineExceeded }

// supervise 起进程、流式解析、按两道上限判死，并保证返回前子进程树已回收。
//
// onEvent 是调用方的事件回调（RunParams.OnEvent，可为 nil），在读协程里同步执行。
func supervise(ctx context.Context, l launch, limits superviseLimits,
	parse func(r io.Reader, h parseHooks) (*Result, error),
	onEvent func(Event)) (*Result, error) {
	if limits.Idle <= 0 {
		limits.Idle = idleTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	// 报错用的实际上限：调用方可能带了更紧的 deadline（如预览推荐的
	// 10 分钟），超时时报 45 分钟会误导排障
	limit := limits.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if remain := time.Until(dl); remain < limit {
			limit = remain
		}
	}

	// 刻意不用 exec.CommandContext：它只杀直接子进程，而 agent CLI 会派生
	// 自己的子进程。这里用 Setpgid 建独立进程组，超时后杀整组。
	cmd := exec.Command(l.Bin, l.Args...)
	cmd.Dir = l.Dir
	cmd.Env = l.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if l.Stdin != nil {
		cmd.Stdin = l.Stdin
	}

	// stdout/stderr 都用自建管道、在后台读，而不是 StdoutPipe +「读到 EOF
	// 再 Wait」。EOF 要等**所有**持有写端的进程都退出 —— agent 用 Bash
	// 留下的后台进程（dev server 之类）会继承这两个 fd，它不退出管道就不关，
	// Run 一直挂到执行超时：一次成功的执行被报成超时，期间进程树、事件 sink
	// 的协程、worker 槽位全被占着。现在先等进程退出，再给读端一段宽限期。
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("agent: 建 stdout 管道失败: %w", err)
	}
	defer func() { _ = outR.Close() }()
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outW.Close()
		return nil, fmt.Errorf("agent: 建 stderr 管道失败: %w", err)
	}
	defer func() { _ = errR.Close() }()
	cmd.Stdout, cmd.Stderr = outW, errW

	startErr := cmd.Start()
	// 父进程必须关掉自己的写端，否则读端永远等不到 EOF
	_ = outW.Close()
	_ = errW.Close()
	// stdin 是 *os.File 时子进程已继承 fd，父进程这份读端同样要关（f01 S5）。
	if f, ok := l.Stdin.(*os.File); ok {
		_ = f.Close()
	}
	if startErr != nil {
		return nil, fmt.Errorf("agent: 启动 %s 失败: %w", l.Bin, startErr)
	}
	pgid := cmd.Process.Pid
	if l.Started != nil {
		l.Started()
	}

	// kill 前钩子：ctx 与看门狗两路都可能触发，只调一次。
	var beforeKill sync.Once
	killNow := func() {
		if l.BeforeKill != nil {
			beforeKill.Do(l.BeforeKill)
		}
		killProcessGroup(pgid)
	}

	// 监听取消：一旦 ctx 结束就杀整个进程组
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killNow()
		case <-killed:
		}
	}()
	defer close(killed)

	// 静默看门狗：连续 Idle 没有产出任何可解析的行就判死并杀掉进程组。
	// 它独立于总超时 —— 「跑得久」和「卡住了」是两回事，报错也要能分开。
	idle := newIdleWatch(limits.Idle)
	defer idle.stop()
	go func() {
		select {
		case <-idle.fired:
			killNow()
		case <-killed:
		}
	}()

	// 事件在后台读，OnEvent 因此在这个协程里回调（RunParams.OnEvent 的
	// 契约本来就是「在读取协程里同步执行」，调用方的实现须并发安全）。
	type parsed struct {
		result *Result
		err    error
	}
	var out parsed
	parseDone := make(chan struct{})
	hooks := parseHooks{
		Touch: idle.touch,
		Emit: func(e Event) {
			if onEvent != nil {
				onEvent(e)
			}
		},
	}
	go func() {
		out.result, out.err = parse(outR, hooks)
		close(parseDone)
	}()
	// stderr 只留头尾：只用于报错，CLI 狂刷时内存不该跟着涨。
	stderr := capbuf.New(stderrHead, stderrTail)
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stderr, errR)
		close(stderrDone)
	}()

	waitErr := cmd.Wait()
	if l.Exited != nil {
		l.Exited()
	}
	// 兜底：即使正常退出也再扫一遍进程组，杀掉可能残留的孙子进程。
	// 同组的遗留后台进程死了，它们持有的写端随之关闭，读端拿到 EOF。
	// 这一次不算「kill 前」，不调 BeforeKill。
	killProcessGroup(pgid)

	// 进程已退出，管道里剩下的内容一般毫秒级读完。宽限期过了还没 EOF，
	// 说明写端被逃出进程组的进程（setsid 之类）攥着 —— 强关读端，不再等。
	deadline := time.Now().Add(pipeDrainGrace)
	forced := drainOrClose(parseDone, outR, deadline)
	drainOrClose(stderrDone, errR, deadline)

	result, parseErr := out.result, out.err
	if forced {
		if result != nil {
			// result 已到手，读端是我们自己关的，读错误不算执行失败
			parseErr = nil
		} else if parseErr != nil {
			parseErr = fmt.Errorf("agent: CLI 已退出但 stdout 被逃出进程组的后台进程占用，"+
				"%v 后强制停止读取: %w", pipeDrainGrace, parseErr)
		}
	}

	// 静默判死要先于总超时判：两者都是「进程被杀、没有 result」，但原因
	// 完全不同，报错必须能分开 —— 一个是设的上限太小，一个是进程卡住了。
	if idle.firedNow() {
		return result, fmt.Errorf("%w（%v 内没有任何输出，进程树已回收，stderr: %s）",
			ErrIdle, limits.Idle, stderr.String())
	}
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return result, &timeoutError{limit: limit}
	} else if ctxErr != nil {
		return result, fmt.Errorf("agent: 执行被取消: %w", ctxErr)
	}

	if parseErr != nil {
		return result, parseErr
	}
	if result == nil {
		return nil, fmt.Errorf("%w（退出错误: %v，stderr: %s）",
			ErrNoResultEvent, waitErr, stderr.String())
	}

	// CLI 以非零码退出但给了 result 事件时，以 result 为准：
	// is_error/subtype 描述得比退出码精确。
	if waitErr != nil && !result.IsError {
		return result, fmt.Errorf("agent: CLI 非零退出（%v），stderr: %s",
			waitErr, stderr.String())
	}
	return result, nil
}

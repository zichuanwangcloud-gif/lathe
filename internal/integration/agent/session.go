package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// session.go —— 运行中插话（docs/features/interject.md）的驱动侧句柄。
//
// StreamInput 模式下 claude 以 `--input-format stream-json --replay-user-messages`
// 启动，prompt 作为 stdin 的第一帧；Session 持有 stdin 写端，让调用方在进程
// 还在跑的时候再追加一帧 user 消息（实测：进行中写入的帧被当前 turn 吸收，
// 不开新 turn）。
//
// stdin 的生命周期（三者任一先到即关，sync.Once 幂等）：
//  1. 读协程解析到本次运行的第一条 result；
//  2. ctx 结束（总超时/取消）或静默看门狗触发 —— 在杀进程组之前；
//  3. cmd.Wait 返回之后（兜底）。
//
// 不存在「常开到进程退出」的路径：CLI 对未关 stdin 的行为实测不稳定
// （有时 result 后约 4s 自退，有时一直等 EOF），依赖它要么让看门狗把
// 成功报成 ErrIdle，要么让迟到的一句话凭空多开一个 turn。唯一稳定成立的是
// 「close 之后已写进管道的帧仍会被吸收、进程正常退出」。
//
// 写成功不代表进程活着、写失败也不代表进程死了（子进程被杀后对写端写
// 仍可能返回 nil）—— 存活判据只看 closed 标志。

// deliverTimeout 是一次 Deliver 的上限：排队 + 写进管道都算在内。
const deliverTimeout = 2 * time.Second

// outboundCap 是待写帧的排队上限。
const outboundCap = 16

var (
	// ErrSessionClosed 表示会话已收尾（stdin 已关），这一帧没有写入。
	ErrSessionClosed = errors.New("agent: 会话已收尾，stdin 已关闭")
	// ErrDeliverBackpressure 表示 2 秒内没排上队或没写进管道（CLI 不读 stdin、管道满）。
	ErrDeliverBackpressure = errors.New("agent: 投递通道拥塞，2 秒内没有写进 stdin")
	// ErrInvalidDeliveryID 表示调用方给的帧 uuid 不是合法 UUID。
	ErrInvalidDeliveryID = errors.New("agent: deliveryID 不是合法 UUID")
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type outbound struct {
	line     []byte
	deadline time.Time // 零值 = 不设写期限（首帧 prompt）
	reply    chan error
	// claim 防「Deliver 已按超时返回、写协程随后又把它写出去」：
	// 0 = 排队中，1 = 写协程已认领，2 = 调用方已放弃。谁先 CAS 成功谁说了算。
	claim *atomic.Int32
}

// Session 是一次 StreamInput 运行的 stdin 句柄。并发安全。
type Session struct {
	pw *os.File

	ch     chan outbound
	done   chan struct{} // closeStdin 时关闭
	exited chan struct{} // 写协程退出时关闭

	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func newSession(pw *os.File) *Session {
	s := &Session{
		pw:     pw,
		ch:     make(chan outbound, outboundCap),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

// Closed 报告 stdin 是否已关闭（首条 result / ctx 结束 / 进程退出，三者任一）。
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// closeStdin 关闭写端并置 closed；排队中尚未写出的帧由写协程以
// ErrSessionClosed 回给各自的 Deliver。幂等。
func (s *Session) closeStdin() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		_ = s.pw.Close() // 写协程若正卡在 Write 上会因此返回错误
	})
}

// Deliver 把一句话作为一帧 stream-json user 消息写进 stdin，帧 uuid = deliveryID，
// **写完（或失败）才返回**，上限 2 秒，绝不无限阻塞：
//   - 会话已收尾（stdin 已关）或在排队期间收尾 → ErrSessionClosed
//   - 2 秒内没排上队 / 没写进管道 → ErrDeliverBackpressure
//
// deliveryID 由调用方生成并传入：调用方必须先登记「等这个 id 的回显」再调
// Deliver，于是回显无论多早到达都有人接住。
func (s *Session) Deliver(ctx context.Context, deliveryID, text string) error {
	if !uuidRe.MatchString(deliveryID) {
		return ErrInvalidDeliveryID
	}
	if s.Closed() {
		return ErrSessionClosed
	}
	line, err := userFrame(deliveryID, text)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(deliverTimeout)
	f := outbound{line: line, deadline: deadline, reply: make(chan error, 1), claim: new(atomic.Int32)}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case s.ch <- f:
	case <-s.done:
		return ErrSessionClosed
	case <-timer.C:
		return ErrDeliverBackpressure
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-f.reply:
		return err
	case <-timer.C:
		// 排在前面的帧（例如还没写完的大 prompt）占着写协程：放弃这一帧。
		// 写协程若已认领它，它的写期限就是同一个 deadline，很快会回话。
		if f.claim.CompareAndSwap(0, 2) {
			return ErrDeliverBackpressure
		}
		return <-f.reply
	case <-s.exited:
		// 写协程已退出：它要么已经回了话（先看一眼），要么这帧是在它
		// 排空之后才进的队 —— 那就是没写，按收尾处理。
		select {
		case err := <-f.reply:
			return err
		default:
			return ErrSessionClosed
		}
	}
}

// writeFirst 把首帧（prompt）交给写协程。不设写期限：prompt 可能远大于
// 管道缓冲，CLI 读得慢不是错误；收尾时关写端会让卡住的写返回。
func (s *Session) writeFirst(line []byte) {
	s.ch <- outbound{line: line, reply: make(chan error, 1)}
}

func (s *Session) writeLoop() {
	defer close(s.exited)
	for {
		select {
		case f := <-s.ch:
			if f.claim != nil && !f.claim.CompareAndSwap(0, 1) {
				continue // 调用方已按超时放弃，不写
			}
			if s.Closed() {
				f.reply <- ErrSessionClosed
				continue
			}
			f.reply <- s.write(f)
		case <-s.done:
			for {
				select {
				case f := <-s.ch:
					f.reply <- ErrSessionClosed
				default:
					return
				}
			}
		}
	}
}

func (s *Session) write(f outbound) error {
	if !f.deadline.IsZero() {
		_ = s.pw.SetWriteDeadline(f.deadline)
	} else {
		_ = s.pw.SetWriteDeadline(time.Time{})
	}
	n, err := s.pw.Write(f.line)
	if err == nil {
		return nil
	}
	if s.Closed() {
		return ErrSessionClosed
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		if n > 0 {
			// 半帧已进管道：再写别的帧会和这半行粘在一起，整条输入流就坏了。
			// 宁可收尾（后续插话走「下一轮补投 / 结算」），也不发出坏帧。
			s.closeStdin()
		}
		return ErrDeliverBackpressure
	}
	// 其它写错误（EPIPE：进程已退出）：进程不再读 stdin，按收尾处理。
	s.closeStdin()
	return ErrSessionClosed
}

// userFrame 构造一帧 stream-json user 消息（以换行结尾）。
func userFrame(id, text string) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
		"uuid": id,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: 编码 user 帧失败: %w", err)
	}
	return append(b, '\n'), nil
}

// newFrameID 生成首帧的 uuid（v4）。
func newFrameID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("agent: 生成 UUID 失败: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

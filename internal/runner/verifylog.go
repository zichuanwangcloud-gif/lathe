package runner

// verifylog.go 验证日志落盘（docs/08-debt-cleanup.md T4）。
//
// 问题：verifications.log_ref 这一列从 0001 迁移就在，读侧
// （store.VerificationRow）也早就把它吐给前端了 —— 但**从来没人写过它**，
// 所以前端拿到的永远是 null。roadmap §0 那张「配置了但没接线」的清单里
// 就记着它，事故编号 #466：排障时无日志可查。
//
// 光在 INSERT 里补一列不够。真正的坑在更上游：
//
//   - 验证输出在 runStep 里就已经被 truncate 到 16KB（maxStepOutput），
//     那是为了「别把整个构建日志灌进数据库」—— 这个理由是对的，
//     但结果是完整日志根本没在任何地方存在过
//   - **通过的步骤的输出直接丢弃**：只有失败步骤的前 4KB 会进
//     agent_events 时间线。而排障时最想看的往往正是「上一次通过时是什么样」
//
// 所以落盘要发生在截断之前，而且通过的步骤也要留。
//
// 为什么做成注入的接口而不是给 Verifier 加个 logDir 字段：
// Verifier 是被并发任务共用的一个实例（cmd/lathe 里只 NewVerifier 一次），
// 给它加可变字段、再在每次验证前改一下，就是一个货真价实的数据竞争。
// 每轮验证由 Pipeline 现造一个已经把 taskID 与轮次烘进去的 logger 传进来，
// Verifier 自己保持无状态。

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// verifyLogRoot 是日志根目录名，位于 DataDir 之下。
//
// **刻意不放 worktree 里**：worktree 会被回收（合并后回收、同名尸体回收、
// T6 的 TTL 收割机），而日志的全部价值就在于「现场没了之后还能查」。
// 放在 worktree 里等于排障时正好没有。
const verifyLogRoot = "verify-logs"

// StepLogger 把一条验证步骤的完整输出落盘，并返回可回查的引用。
//
// 实现必须满足：**落盘失败绝不能让验证失败**。返回错误即可，
// 调用方会降级为「log_ref 留空 + 记 warn」继续跑。磁盘满、权限不对
// 都不该把一次本来能通过的验证判死。
type StepLogger interface {
	WriteStepLog(step string, output []byte) (ref string, err error)
}

// fileStepLogger 把日志写进 <DataDir>/verify-logs/task-<id>/round-<n>/。
//
// ref 存的是**相对 DataDir 的路径**，不是绝对路径：DataDir 是可配的
// （LATHE_DATA_DIR），把绝对路径写进数据库会让部署目录一变、
// 历史记录里的路径全部失效。相对路径 + 一个已知的根，语义稳定。
type fileStepLogger struct {
	// dataDir 是 DataDir 绝对路径，用于拼实际写入位置。
	dataDir string
	// rel 是相对 dataDir 的本轮目录，如 verify-logs/task-42/round-0。
	rel string
	// seq 保证同一轮里重名步骤不互相覆盖。heavy 档的回归与 light 档的
	// 构建步骤名不会撞，但修复回路里同名步骤会反复出现，
	// 而同一轮内也可能有多条同名的复现测试。
	seq atomic.Int64
}

// WriteStepLog 落盘并返回相对路径。
func (l *fileStepLogger) WriteStepLog(step string, output []byte) (string, error) {
	if l == nil || l.dataDir == "" {
		return "", nil
	}
	n := l.seq.Add(1)
	name := fmt.Sprintf("%02d-%s.log", n, sanitizeLogName(step))
	rel := filepath.Join(l.rel, name)
	abs := filepath.Join(l.dataDir, rel)

	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", fmt.Errorf("建验证日志目录失败: %w", err)
	}
	// 0600：验证输出可能包含仓库内容与环境细节，按凭据同级对待
	// （与 internal/secret 的落盘权限一致）。
	if err := os.WriteFile(abs, output, 0o600); err != nil {
		return "", fmt.Errorf("写验证日志失败: %w", err)
	}
	return rel, nil
}

// sanitizeLogName 把步骤名规整成安全的文件名。
//
// 步骤名里可能有斜杠与空格（复现测试的名字来自测试文件路径），
// 直接拼进路径会写到意料之外的位置。
func sanitizeLogName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "step"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "step"
	}
	// 防一手过长的名字撑爆文件名上限
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// stepLogger 造一个把 taskID 与轮次烘进去的 logger。
//
// LogDir 未配置时返回 nil —— 调用链上所有 WriteStepLog 都会被 nil 检查
// 短路，log_ref 留空，行为与本项之前完全一致（测试与不关心日志的
// 调用方不必被迫准备一个目录）。
//
// round 0 是首轮验证，1..N 对应修复回路的第 N 轮。分目录存放是 AC3 的
// 要求：同任务多轮不能互相覆盖，否则「第一轮为什么挂」这个问题
// 在第二轮跑完之后就永远回答不了了。
func (p *Pipeline) stepLogger(taskID int64, round int) StepLogger {
	if p.LogDir == "" {
		return nil
	}
	return &fileStepLogger{
		dataDir: p.LogDir,
		rel:     filepath.Join(verifyLogRoot, fmt.Sprintf("task-%d", taskID), fmt.Sprintf("round-%d", round)),
	}
}

// writeStepLog 是所有落盘点的统一入口：吞掉错误、只记日志。
//
// 单独抽出来是为了让「落盘失败不能让验证失败」这条约束（T4-AC6）
// 只在一处实现 —— 散在各个调用点迟早有一处忘记吞错误，
// 于是磁盘满就能把一次本来能通过的验证判死。
func writeStepLog(logs StepLogger, step string, output []byte) string {
	if logs == nil {
		return ""
	}
	ref, err := logs.WriteStepLog(step, output)
	if err != nil {
		slog.Warn("验证日志落盘失败（不影响验证结果）", "step", step, "err", err)
		return ""
	}
	return ref
}

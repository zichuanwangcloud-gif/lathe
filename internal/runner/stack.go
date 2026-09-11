package runner

// stack.go 验证阶段的依赖隔离栈接线（docs/08-debt-cleanup.md T8）。
//
// runner 侧只声明「起栈 / 拆栈」这两件事的窄接口，实现在
// internal/preview（那里有 docker 能力与可注入的 exec）。
// 这与 VerificationRecorder / TaskMail / StepLogger 是同一套做法。

import (
	"context"
	"errors"
)

// VerifyStackUp 起一个任务专属的依赖栈。
//
// infra 为空时应返回 (nil, nil) —— 「这个仓库没声明依赖」不是错误，
// 是最常见的情形。
type VerifyStackUp interface {
	UpVerifyStack(ctx context.Context, taskID int64, infra []string) (VerifyStackHandle, error)
}

// VerifyStackHandle 是已起的栈：拿连接串、拆栈。
type VerifyStackHandle interface {
	// StackEnv 返回注入验证命令的宿主口径连接串。
	StackEnv() map[string]string
	// Down 拆栈。尽力清理，失败只告警。
	Down(ctx context.Context) error
}

// ErrStackOverThreshold 表示资源水位不允许起隔离栈。
//
// 实现方应包装成可用 errors.Is 判定的形态。runner 据此**降级为无隔离
// 执行并留痕**，而不是让验证失败 —— 见 T8-AC5 的决策记录。
var ErrStackOverThreshold = errors.New("runner: 资源水位超阈值，不起验证隔离栈")

// ErrStackDockerDown 表示起栈失败的根因是 docker 守护进程不可用。
//
// 与 ErrStackOverThreshold 分开、且**不降级**：docker 挂了是比拉不到
// 镜像更根本的故障，降级跑出来的结论（无隔离、环境可疑）与其说是
// 验证，不如说是碰运气。实现方应把「docker 不可用」包装成本错误。
var ErrStackDockerDown = errors.New("runner: docker 守护进程不可用，不起验证隔离栈")

// stackUnavailable 报告这个起栈错误是否属于「该降级而不是判死」。
//
// 水位超阈值是唯一该降级的：机器忙不是任务的错，回落到无隔离执行
// 仍能给出验证结论（只是并发污染的风险回到本项之前的水平）。
// 其它错误（未知依赖名、镜像拉不下来、就绪超时、**docker 守护进程
// 不可用**）都是配置或环境问题，该让人看见，不该悄悄降级。
//
// docker 不可用尤其不能降级：CheckResources 的第一分支就是
// !DockerOK → Allowed=false，若与水位共用同一个错误身份，日志会写
// 「资源水位不允许」，把人引去调阈值 —— 而真正该做的是把 docker 起来。
func stackUnavailable(err error) bool {
	return errors.Is(err, ErrStackOverThreshold)
}

// ErrVerifyStackUp 是起隔离栈失败的独立错误身份（T8-AC7）。
//
// 为什么必须独立：起栈失败若冒充成复现阶段的 StatusError，会被
// redEnvError 归类为「环境问题、任务失败留现场」—— 语义上恰好也对，
// 但错误信息会让人以为是复现测试跑不起来，于是去查一个根本没问题的
// 测试命令。红阶段的三分路由（blocked_spec / 失败留现场 / 进修复回路）
// 依赖错误身份，混进第四种来源就会误判。
var ErrVerifyStackUp = errors.New("runner: 起验证隔离栈失败")

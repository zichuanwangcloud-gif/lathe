package preview

// verifystack.go 验证阶段的 per-task 依赖隔离栈（docs/08-debt-cleanup.md T8）。
//
// ---------------------------------------------------------------------------
// 「隔离」在这里到底指什么
// ---------------------------------------------------------------------------
//
// 02-design §5.3 第 1 步的原文是「起隔离栈（per-task compose project：
// **动态端口 + 独立 DB schema**）」，§8 P1 又说「待目标仓库**声明服务栈**后补」。
//
// 这说的**不是**「把测试进程塞进容器里跑」，而是「给每个任务一套自己的
// **依赖**（自己的库、自己的随机端口），把连接串注入验证命令」。
// 测试仍在宿主的 worktree 里跑。
//
// 这个读法要紧，因为两种做法的难度与风险差一个量级：
//
//   - 容器化测试运行器：需要目标仓库的镜像自带工具链与源码，
//     还要解决 docker exec 收集退出码与输出、缓存挂载、权限等一堆问题
//   - per-task 依赖栈：复用现成的 InfraCatalog + 随机端口 + 标签清理，
//     解决的正是真实痛点 —— 并发任务共用一个 postgres，
//     互相写脏对方的表，红绿结论因此不可信
//
// 后者才是「验证结论可信」这个产品主张的必要条件。真正的**执行**隔离
// （bubblewrap / 容器里跑 agent 与测试）是另一件事，roadmap §3.5 第 14 条。
//
// ---------------------------------------------------------------------------
// 为什么落在 preview 包内而不是 runner
// ---------------------------------------------------------------------------
//
// Manager 的 exec / execStream 是**私有字段**（测试注入假件用）。
// 代码放在包内才能用上那个注入点 —— 否则 runner 侧的单测只能依赖真 docker，
// 那种测试在 CI 里第一个挂。
//
// 同时刻意**不复用 Manager.Start**：它是异步 fire-and-forget
// （内部 go m.run(...)），失败只写进 ops[taskID].Error 靠 Status 轮询发现，
// 而且 ops 以 taskID 为键、ErrBuildInProgress 会拒绝同 taskID 的第二次启动。
// 验证要的是同步阻塞、起不来立即报错，且不能与人点的预览互相拒绝。
//
// ---------------------------------------------------------------------------
// 与预览栈的隔离（一个真实的撞车风险）
// ---------------------------------------------------------------------------
//
// Manager.Stop(taskID) 的清理方式**不是** docker compose down，而是按
// lathe.task=<id> 标签查出容器/网络/镜像后逐个 rm -f。所以如果验证栈沿用
// 同一套标签，人在看板点一次「停止预览」就会把同一任务正在跑的验证容器
// 一起删掉，反之亦然。
//
// 解法：验证栈用独立的标签键 labelVerify 与独立的资源命名前缀，
// 且**绝不打 labelTask** —— Stop 的两路查询因此都匹配不到它。
// TestVerifyStackIsolatedFromPreviewStop 钉住这一条。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// labelVerify 是验证隔离栈的标签键。
//
// 与 labelPreview 分开是为了让两套栈的清理互不干扰（见文件头注释）。
// 值是 taskID，这样 Down 能按任务精确清理。
const labelVerify = "lathe.verify"

// ErrVerifyStackOverThreshold 表示资源水位不允许起隔离栈。
//
// 调用方应据此**降级为无隔离执行并留痕**，而不是让验证失败 ——
// 见 runner 侧对这个错误的处理与 T8-AC5 的决策记录。
var ErrVerifyStackOverThreshold = errors.New("preview: 资源水位超阈值，不起验证隔离栈")

// VerifyStack 是一个任务专属的依赖栈。
type VerifyStack struct {
	m       *Manager
	taskID  int64
	network string
	// containers 是本栈起的容器名，Down 时逐个清理。
	containers []string

	// Env 是注入给验证命令的**宿主口径**连接串
	// （127.0.0.1:<随机端口>，不是任务网络内的别名）。
	Env map[string]string
}

// UpVerifyStack 同步起一个任务专属的依赖栈。
//
// infra 是要起的服务名（InfraCatalog 的键）。为空时返回 (nil, nil) ——
// 「这个仓库没声明依赖」不是错误，是最常见的情形，调用方按无隔离执行。
//
// 起栈失败会把已起的部分清理干净再返回错误：半个栈比没有栈更糟，
// 它会让下一次启动撞上同名容器。
func (m *Manager) UpVerifyStack(ctx context.Context, taskID int64, infra []string) (*VerifyStack, error) {
	if len(infra) == 0 {
		return nil, nil
	}

	// 资源闸门（AC5）。验证栈与预览栈复用同一组阈值 —— 它们抢的是同一台
	// 机器的内存与磁盘，为验证另设一组阈值只会让两边加起来超过机器上限。
	// 键名仍叫 preview_* 是历史包袱，注释在这里说明它其实是全局闸门。
	if rs, err := m.CheckResources(ctx); err == nil && !rs.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrVerifyStackOverThreshold, rs.Reason)
	} else if err != nil {
		// 测不出水位不该挡住验证：闸门是保护措施，不是前置条件。
		slog.Warn("验证隔离栈的资源水位测量失败，继续起栈", "task", taskID, "err", err)
	}

	st := &VerifyStack{
		m:       m,
		taskID:  taskID,
		network: fmt.Sprintf("lathe-verify-t%d-net", taskID),
		Env:     map[string]string{},
	}

	// 任务网络：同栈容器互访用。刻意不打 labelTask —— 见文件头注释。
	if _, stderr, err := m.exec(ctx, m.DockerBin, "network", "create",
		"--label", fmt.Sprintf("%s=%d", labelVerify, taskID),
		st.network); err != nil {
		return nil, fmt.Errorf("创建验证网络失败: %s", tail(stderr, 500))
	}

	for _, name := range infra {
		spec, ok := InfraCatalog[name]
		if !ok {
			_ = st.Down(ctx)
			return nil, fmt.Errorf("未知的基础设施 %q（可选：%s）", name, strings.Join(infraNames(), ", "))
		}
		if spec.Port == 0 || spec.HostEnv == nil {
			_ = st.Down(ctx)
			return nil, fmt.Errorf("基础设施 %q 未声明宿主端口，无法用于验证隔离", name)
		}

		// 口令每次启动生成，永不让人填（沿用预览流的立场）。
		pw := generatePassword()
		cname := fmt.Sprintf("lathe-verify-t%d-%s", taskID, name)

		args := []string{"run", "-d", "--name", cname,
			"--network", st.network, "--network-alias", spec.Alias,
			"--label", fmt.Sprintf("%s=%d", labelVerify, taskID),
			// -p 0:<port>：让 docker 挑一个空闲宿主端口。钉死端口会让
			// 并发任务互相撞车 —— 而「并发任务互不干扰」正是本项的目的。
			"-p", fmt.Sprintf("0:%d", spec.Port)}
		for _, e := range spec.Env(pw) {
			args = append(args, "-e", e)
		}
		args = append(args, spec.Image)

		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			_ = st.Down(ctx)
			return nil, fmt.Errorf("启动验证依赖 %s 失败: %s", name, tail(stderr, 500))
		}
		st.containers = append(st.containers, cname)

		if err := m.waitReady(ctx, cname, spec.Ready(pw)); err != nil {
			_ = st.Down(ctx)
			return nil, fmt.Errorf("验证依赖 %s 就绪等待失败: %w", name, err)
		}

		hostPort, err := m.hostPortOf(ctx, cname, spec.Port)
		if err != nil {
			_ = st.Down(ctx)
			return nil, fmt.Errorf("读取 %s 的宿主端口失败: %w", name, err)
		}
		for k, v := range spec.HostEnv(pw, hostPort) {
			st.Env[k] = v
		}
		slog.Info("验证依赖已就绪", "task", taskID, "infra", name, "host_port", hostPort)
	}

	return st, nil
}

// hostPortOf 回读容器某个内部端口映射到的宿主端口。
//
// 必须回读而不能预先指定：-p 0:<port> 让 docker 挑端口，
// 挑到哪个只有它自己知道。
func (m *Manager) hostPortOf(ctx context.Context, container string, internal int) (int, error) {
	format := fmt.Sprintf(`{{(index (index .NetworkSettings.Ports "%d/tcp") 0).HostPort}}`, internal)
	out, stderr, err := m.exec(ctx, m.DockerBin, "inspect", "-f", format, container)
	if err != nil {
		return 0, fmt.Errorf("docker inspect 失败: %s", tail(stderr, 300))
	}
	p, cerr := strconv.Atoi(strings.TrimSpace(out))
	if cerr != nil || p <= 0 {
		return 0, fmt.Errorf("解析宿主端口失败（得到 %q）", strings.TrimSpace(out))
	}
	return p, nil
}

// Down 拆栈。尽力清理每一步并继续 —— 半个栈会让下一次启动撞同名容器。
//
// 与 Manager.Stop 一样不依赖 compose 编排文件在场：清理只靠容器名与
// 网络名，所以 worktree 已经被回收也照样能拆。
func (s *VerifyStack) Down(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var firstErr error
	for _, c := range s.containers {
		if _, stderr, err := s.m.exec(ctx, s.m.DockerBin, "rm", "-f", c); err != nil {
			slog.Warn("清理验证依赖容器失败（继续）", "container", c, "err", tail(stderr, 300))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if s.network != "" {
		if _, stderr, err := s.m.exec(ctx, s.m.DockerBin, "network", "rm", s.network); err != nil {
			// 网络删不掉通常是还有容器挂着；容器已经 rm -f 过，
			// 这里失败多半是竞态，只告警。
			slog.Warn("清理验证网络失败（继续）", "network", s.network, "err", tail(stderr, 300))
		}
	}
	s.containers = nil
	return firstErr
}

// infraNames 返回 InfraCatalog 的键，供错误信息里列出可选值。
func infraNames() []string {
	out := make([]string, 0, len(InfraCatalog))
	for k := range InfraCatalog {
		out = append(out, k)
	}
	return out
}

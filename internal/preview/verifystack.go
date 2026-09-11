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
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
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

// ErrDockerUnavailableVerify 表示 docker 守护进程不可用，验证隔离栈无从谈起。
//
// 与 ErrVerifyStackOverThreshold 分开是必须的：守卫者把 docker 挂掉
// 归进「水位超阈值」，runner 侧就会走降级分支，日志写「资源水位不允许」
// —— docker 守护进程不可用是比拉不到镜像更根本的故障，不该悄悄降级
// （那正是本项自己定的原则：镜像拉不下来、就绪超时都该让人看见）。
var ErrDockerUnavailableVerify = errors.New("preview: docker 守护进程不可用，无法起验证隔离栈")

// verifyDownTimeout 是拆栈、半栈清理与开机清扫的统一超时。
//
// 30s 量级：几条 docker 命令（逐个 rm -f + network rm）在本机是秒级，
// 给足余量又不至于让退出流程挂住。
const verifyDownTimeout = 30 * time.Second

// hostProbeTimeout 是宿主侧 TCP 探活的等待上限。
const hostProbeTimeout = 30 * time.Second

// detach 解绑父 ctx 的取消，并配一个自己的超时。
//
// 为什么必须解绑：Manager.exec 是 exec.CommandContext，ctx 一旦取消，
// 连 `docker rm -f` 都执行不了 —— 立即返回 context canceled。而拆栈
// 用的正是流水线那个根 ctx（node.work 的 ctx，SIGINT/SIGTERM 优雅退出
// 时会被取消）。后果是：heavy 验证正在跑时 Ctrl-C 重启 serve，三条
// docker 命令全部瞬时失败（只打一条 slog.Warn），postgres 容器 + 网络
// 永久留在机器上。
//
// 放在这里统一解绑而不是让每个调用点各写一遍：UpVerifyStack 内部有
// 四处半栈清理、Down 又有一处，调用方（pipeline.runHeavy 的 defer）
// 拿不到、也不该关心这个细节。本仓已有此惯例：pipeline.go 的
// pushProgress 就是 context.WithoutCancel(rc.ctx) 配 5s 超时。
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), verifyDownTimeout)
}

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
		if !rs.DockerOK {
			// docker 挂了单独走判死路径：「资源水位不允许」的措辞会把
			// 人引去调阈值，而真正该做的是把 docker 起来。
			return nil, fmt.Errorf("%w: %s", ErrDockerUnavailableVerify, rs.Reason)
		}
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

	// 名字回收（幂等）：容器名与网络名都是确定性的
	// （lathe-verify-t<id>-<infra>），上一次拆栈失败留下的残骸会让
	// 这一次的 network create / run 撞上「Conflict. The container name
	// is already in use」→ 走判死分支 → 任务永久红，直到有人手工
	// docker rm -f。做一步先删（忽略一切错误），重复起栈就成了安全的。
	//
	// 网络不删不行：`docker network create` 撞同名同样报 Conflict。
	// 刻意让它不返回错误：回收是为「本该不存在的东西」做的尽力而为，
	// 真删不掉的话，紧接着的 create 会把真实原因如实报出来。
	st.reclaimNames(ctx, infra)

	// 任务网络：同栈容器互访用。刻意不打 labelTask —— 见文件头注释。
	if _, stderr, err := m.exec(ctx, m.DockerBin, "network", "create",
		"--label", fmt.Sprintf("%s=%d", labelVerify, taskID),
		st.network); err != nil {
		return nil, fmt.Errorf("创建验证网络失败: %s", tail(stderr, 500))
	}

	for _, name := range infra {
		spec, ok := InfraCatalog[name]
		if !ok {
			st.cleanupPartial(ctx)
			return nil, fmt.Errorf("未知的基础设施 %q（可选：%s）", name, strings.Join(infraNames(), ", "))
		}
		if spec.Port == 0 || spec.HostEnv == nil {
			st.cleanupPartial(ctx)
			return nil, fmt.Errorf("基础设施 %q 未声明宿主端口，无法用于验证隔离", name)
		}

		// 口令每次启动生成，永不让人填（沿用预览流的立场）。
		pw := generatePassword()
		cname := fmt.Sprintf("lathe-verify-t%d-%s", taskID, name)

		args := []string{"run", "-d", "--name", cname,
			"--network", st.network, "--network-alias", spec.Alias,
			"--label", fmt.Sprintf("%s=%d", labelVerify, taskID),
			// -p 127.0.0.1:0:<port>：让 docker 挑一个空闲宿主端口，
			// 且**只绑回环**。预览流绑 0.0.0.0 是刻意的（用户要从
			// 局域网别的机器访问），验证栈没有这个需求 —— HostEnv
			// 写死 127.0.0.1，任务专属库不该暴露到局域网。
			//
			// 它同时修掉一个端口歧义：不显式写宿主机 IP 时，
			// `.NetworkSettings.Ports["5432/tcp"]` 在启用 IPv6 的
			// daemon 上是两条（0.0.0.0 与 ::），`index 0` 取哪条不
			// 确定，而连接串硬编码 127.0.0.1 —— 取到 IPv6 那条就
			// 连不上。
			"-p", fmt.Sprintf("127.0.0.1:0:%d", spec.Port)}
		for _, e := range spec.Env(pw) {
			args = append(args, "-e", e)
		}
		args = append(args, spec.Image)

		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			st.cleanupPartial(ctx)
			return nil, fmt.Errorf("启动验证依赖 %s 失败: %s", name, tail(stderr, 500))
		}
		st.containers = append(st.containers, cname)

		if err := m.waitReady(ctx, cname, spec.Ready(pw)); err != nil {
			st.cleanupPartial(ctx)
			return nil, fmt.Errorf("验证依赖 %s 就绪等待失败: %w", name, err)
		}

		hostPort, err := m.hostPortOf(ctx, cname, spec.Port)
		if err != nil {
			st.cleanupPartial(ctx)
			return nil, fmt.Errorf("读取 %s 的宿主端口失败: %w", name, err)
		}

		// 宿主侧 TCP 探活（必须）。容器内的就绪探测走的是 unix socket，
		// 探不到 TCP 是否已发布：postgres 官方 entrypoint 首次初始化时
		// 先起一个 `listen_addresses=''` 的临时服务器（只听 unix
		// socket）建库，跑完 initdb.d 脚本后【停掉再以正常配置重启】。
		// 探测命令 psql/mysql 不带 -h，走的正是 unix socket —— 它在
		// 临时服务器阶段就成功了。于是 waitReady 返回、回读到端口，
		// 验证命令立刻连 127.0.0.1:<port>，而 postgres 正在重启中 →
		// connection refused → 测试红。探 TCP 是唯一真正对应
		// 「验证命令怎么连」的检查（相比之下给探测命令加 -h 127.0.0.1
		// 只是让容器内那次探测也走 TCP，仍测不到宿主侧的端口发布）。
		if err := m.waitHostPort(ctx, hostPort); err != nil {
			st.cleanupPartial(ctx)
			return nil, fmt.Errorf("验证依赖 %s 的宿主端口 %d 探活失败: %w", name, hostPort, err)
		}

		for k, v := range spec.HostEnv(pw, hostPort) {
			st.Env[k] = v
		}
		slog.Info("验证依赖已就绪", "task", taskID, "infra", name, "host_port", hostPort)
	}

	return st, nil
}

// reclaimNames 逐个删掉本栈将要使用的容器名与网络名（忽略一切错误）。
//
// 目的见调用点：让重复起栈成为幂等的。刻意**只按精确名字删**，不按
// lathe.verify 标签批量删 —— 后者会误删同一任务另一个并发栈（重验与
// 首次验证理论上可以同时在跑）。
func (s *VerifyStack) reclaimNames(ctx context.Context, infra []string) {
	for _, name := range infra {
		if _, ok := InfraCatalog[name]; !ok {
			// 未知依赖名不在这里报错（调用方马上会报），但名字回收
			// 也不该拿它去拼容器名调 docker。
			continue
		}
		cname := fmt.Sprintf("lathe-verify-t%d-%s", s.taskID, name)
		// 忽略一切错误：绝大多数情况是「没有这个容器」，属预期噪音。
		// 真删不掉的话，紧接着的 run 会把真实原因如实报出来。
		_, _, _ = s.m.exec(ctx, s.m.DockerBin, "rm", "-f", cname)
	}
	if s.network != "" {
		_, _, _ = s.m.exec(ctx, s.m.DockerBin, "network", "rm", s.network)
	}
}

// cleanupPartial 清掉半栈。走 detach 后的 ctx —— 起栈失败常常正是
// ctx 被取消（SIGINT）导致的，用同一根死 ctx 清理等于一条命令都不执行。
func (s *VerifyStack) cleanupPartial(ctx context.Context) {
	dctx, cancel := detach(ctx)
	defer cancel()
	if err := s.Down(dctx); err != nil {
		slog.Warn("清理半栈未完全成功（残留容器名会在下次起栈时被回收）",
			"task", s.taskID, "err", err)
	}
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
//
// ctx 只用来取「取消之后的上下文」：清理自己解绑取消（见 detach）。
// 这里一次覆盖了全部调用点（runHeavy 的 defer、半栈清理、测试），
// 而不是让每个调用点各自记得换 ctx —— 漏掉任何一处都会永久泄漏容器。
func (s *VerifyStack) Down(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ctx, cancel := detach(ctx)
	defer cancel()

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

// waitHostPort 轮询宿主侧 TCP 直到端口可连上（或超时）。
//
// 见 UpVerifyStack 里的说明：容器内的就绪探测走 unix socket，覆盖不到
// 「端口是否已在宿主上可用」，而验证命令连的正是 127.0.0.1:<port>。
func (m *Manager) waitHostPort(ctx context.Context, port int) error {
	// hostProbe 为 nil 只可能是测试直接构造了 Manager 字面量
	// （NewManager 一定填上）。此时**回落到真探针**而不是跳过：
	// 跳过是 fail-open —— 万一将来生产路径上多了一处 Manager 字面量，
	// 探活会静默失效，而这条探活正是「偶发红」的唯一防线。
	probe := m.hostProbe
	if probe == nil {
		probe = realHostProbe
	}
	deadline := time.Now().Add(hostProbeTimeout)
	for {
		err := probe(ctx, port)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s 超过 %s 未能建立 TCP 连接", hostPortAddr(port), hostProbeTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func hostPortAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// realHostProbe 是 hostProbe 的默认实现：拨一次 127.0.0.1:<port>。
func realHostProbe(ctx context.Context, port int) error {
	addr := hostPortAddr(port)
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// SweepVerifyStacks 清理机器上所有残留的验证隔离栈资源（开机清扫）。
//
// 为什么需要它：验证栈刻意不打 labelTask（这是对的 —— 否则人在看板点
// 「停止预览」会把正在跑的验证容器一起删掉），代价是它掉出了所有既有
// 清理路径：Manager.Stop 的两路查询匹配不到它，界面上也没有入口。
// 预览容器泄漏了还有人能点停止，验证容器泄漏了**没有任何出口**。
// 而容器名是确定性的，泄漏一次之后同一任务的每次重试都会撞
// 「container name is already in use」→ 任务永久红。
//
// 为什么可以无条件全清：验证栈按定义不该跨进程存活（与预览栈相反 ——
// 预览栈跨重启保留现场是刻意的），所以不需要按 taskID 甄别，开机时
// 全删是安全的 —— 此刻不可能有本进程起的栈。
//
// 返回（清掉的容器数, 清掉的网络数, 错误）。错误只让调用方告警不阻断
// 启动：清扫失败不该阻止 serve 起来，逐名回收（reclaimNames）仍会在
// 每次起栈时兜住。
func (m *Manager) SweepVerifyStacks(ctx context.Context) (int, int, error) {
	sctx, cancel := detach(ctx)
	defer cancel()

	containers, err := m.verifyResourceIDs(sctx, false)
	if err != nil {
		return 0, 0, err
	}
	if len(containers) > 0 {
		if _, stderr, err := m.exec(sctx, m.DockerBin, append([]string{"rm", "-f"}, containers...)...); err != nil {
			return 0, 0, fmt.Errorf("preview: 清扫残留验证容器失败: %s", tail(stderr, 300))
		}
	}

	networks, err := m.verifyResourceIDs(sctx, true)
	if err != nil {
		return len(containers), 0, err
	}
	if len(networks) > 0 {
		if _, stderr, err := m.exec(sctx, m.DockerBin, append([]string{"network", "rm"}, networks...)...); err != nil {
			// 网络删不掉（还有容器接着）不挡主路：容器已清，下次起栈
			// 的逐名回收还会兜一次。
			slog.Warn("清扫残留验证网络不完整", "err", tail(stderr, 300))
		}
	}
	return len(containers), len(networks), nil
}

// verifyResourceIDs 按 labelVerify 标签列出残留容器或网络的 id。
//
// 用 id 而非名字：rm -f 接受 id，也不受「名字恰好与 id 同形」影响。
func (m *Manager) verifyResourceIDs(ctx context.Context, network bool) ([]string, error) {
	sub := []string{"ps", "-aq"}
	if network {
		sub = []string{"network", "ls", "-q"}
	}
	out, stderr, err := m.exec(ctx, m.DockerBin, append(sub, "--filter", "label="+labelVerify)...)
	if err != nil {
		return nil, fmt.Errorf("preview: 查询残留验证资源失败: %s", tail(stderr, 300))
	}
	return strings.Fields(out), nil
}

// infraNames 返回 InfraCatalog 的键，供错误信息里列出可选值。
func infraNames() []string {
	out := make([]string, 0, len(InfraCatalog))
	for k := range InfraCatalog {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// InfraNames 是 infraNames 的导出形态：API 层的校验（httpapi 的
// updateRepo）要拿同一份目录来挡错拼的依赖名，不能自己抄一份列表
// —— 抄一份就等于把知识写两遍，而这份目录在 Go 侧演进。
func InfraNames() []string { return infraNames() }

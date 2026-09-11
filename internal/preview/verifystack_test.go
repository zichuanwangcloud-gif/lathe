package preview

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// 没声明依赖时返回 (nil, nil)：「这个仓库没有依赖」不是错误，
// 是最常见的情形，调用方按无隔离执行。
func TestUpVerifyStackNoInfraIsNoop(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)

	st, err := m.UpVerifyStack(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("无依赖不该报错: %v", err)
	}
	if st != nil {
		t.Errorf("无依赖应返回 nil 栈，得到 %#v", st)
	}
	if len(fd.calls) != 0 {
		t.Errorf("无依赖不该调 docker，实际调了 %v", fd.calls)
	}
}

// 起栈：随机端口发布 + 回读宿主端口 + 宿主口径连接串。
func TestUpVerifyStackPublishesRandomPortAndInjectsHostEnv(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		// docker inspect -f ... 回读宿主端口
		"inspect": {stdout: "49173\n"},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	st, err := m.UpVerifyStack(context.Background(), 42, []string{"postgres"})
	if err != nil {
		t.Fatalf("UpVerifyStack 失败: %v", err)
	}
	if st == nil {
		t.Fatal("应返回栈")
	}
	t.Cleanup(func() { _ = st.Down(context.Background()) })

	// 必须用 -p 127.0.0.1:0:5432：让 docker 挑端口（钉死端口会让并发
	// 任务撞车），且只绑回环 —— 任务专属库不该暴露到局域网
	if !fd.has("run", "-p", "127.0.0.1:0:5432") {
		t.Errorf("应以 -p 127.0.0.1:0:5432 发布随机端口，实际调用：%v", fd.calls)
	}
	if fd.has("run", " -p 0:5432") {
		t.Errorf("不该绑 0.0.0.0：验证栈的 HostEnv 写死 127.0.0.1，没有从局域网访问的需求（且不显式写宿主 IP 时，启用 IPv6 的 daemon 上 Ports 是两条，index 0 取哪条不确定），实际调用：%v", fd.calls)
	}

	// 连接串必须是宿主口径：127.0.0.1:<回读到的端口>，
	// 不是任务网络内的别名 pg —— 验证命令跑在宿主上，解析不了 pg
	if got := st.Env["DATABASE_HOST"]; got != "127.0.0.1" {
		t.Errorf("DATABASE_HOST = %q，期望 127.0.0.1（别名 pg 在宿主上解析不了）", got)
	}
	if got := st.Env["DATABASE_PORT"]; got != "49173" {
		t.Errorf("DATABASE_PORT = %q，期望回读到的 49173", got)
	}
	url := st.Env["DATABASE_URL"]
	if !strings.Contains(url, "127.0.0.1:49173") {
		t.Errorf("DATABASE_URL 应含宿主地址与端口，得到 %q", url)
	}
	if strings.Contains(url, "@pg:") {
		t.Errorf("DATABASE_URL 不该用任务网络别名，得到 %q", url)
	}
	// 口令由系统生成，绝不为空、绝不让人填
	if st.Env["DATABASE_PASSWORD"] == "" {
		t.Error("口令应由系统生成")
	}
}

// ★ 撞车防护：验证栈绝不能被 Manager.Stop（人点「停止预览」）误删。
//
// Stop(taskID) 的清理不是 docker compose down，而是按
// lathe.preview=1 + lathe.task=<id> 标签、以及 compose 项目标签
// 逐个 rm -f。验证栈若沿用同一套标签，人点一次停止预览就会把
// 正在跑的验证容器一起删掉。
func TestVerifyStackIsolatedFromPreviewStop(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{"inspect": {stdout: "5000\n"}}}
	m, _ := newTestManager(t, fd, 100, 100)

	st, err := m.UpVerifyStack(context.Background(), 7, []string{"redis"})
	if err != nil {
		t.Fatalf("UpVerifyStack 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Down(context.Background()) })

	// 只看【创建资源】的调用：docker exec（就绪探测）与 docker inspect
	// （回读端口）本来就不带标签，把它们也纳入断言是测试自己的错。
	creating := 0
	for _, c := range fd.calls {
		joined := strings.Join(c, " ")
		isCreate := strings.Contains(joined, "run -d") || strings.Contains(joined, "network create")
		if !isCreate || !strings.Contains(joined, "lathe-verify-t7") {
			continue
		}
		creating++
		if strings.Contains(joined, labelPreview+"=") {
			t.Errorf("验证栈资源不该打 %s 标签，否则会被停止预览误删：%s", labelPreview, joined)
		}
		if strings.Contains(joined, labelTask+"=") {
			t.Errorf("验证栈资源不该打 %s 标签，否则会被停止预览误删：%s", labelTask, joined)
		}
		if !strings.Contains(joined, labelVerify+"=7") {
			t.Errorf("验证栈资源应打 %s=7，实际：%s", labelVerify, joined)
		}
	}
	// 防呆：断言至少真检查过创建调用，别让循环空转还报「通过」
	if creating < 2 {
		t.Errorf("应至少检查到 2 个创建调用（网络 + 容器），实际 %d", creating)
	}

	// compose 项目名也必须分开
	if ComposeProjectFor("verify", 7) == ComposeProject(7) {
		t.Error("验证栈与预览栈的 compose 项目名必须不同，否则 Stop 的项目标签查询会撞上")
	}
}

// 资源水位超阈值时返回可识别的错误，供调用方降级（AC5）。
func TestUpVerifyStackRefusedOverThreshold(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	// 阈值设成 1%：任何真实水位都会超
	m, _ := newTestManager(t, fd, 1, 1)

	_, err := m.UpVerifyStack(context.Background(), 3, []string{"postgres"})
	if err == nil {
		t.Fatal("水位超阈值应报错")
	}
	if !strings.Contains(err.Error(), "资源水位超阈值") {
		t.Errorf("错误应可被识别为水位问题，得到 %v", err)
	}
	// 起栈被拒时不该留下任何容器
	if fd.has("run", "-d", "--name", "lathe-verify") {
		t.Errorf("被拒时不该起容器，实际：%v", fd.calls)
	}
}

// 未知的依赖名报错，并把已起的部分清理干净 ——
// 半个栈比没有栈更糟，它会让下一次启动撞同名容器。
func TestUpVerifyStackUnknownInfraCleansUp(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{"inspect": {stdout: "5432\n"}}}
	m, _ := newTestManager(t, fd, 100, 100)

	_, err := m.UpVerifyStack(context.Background(), 5, []string{"postgres", "不存在的东西"})
	if err == nil {
		t.Fatal("未知依赖应报错")
	}
	// 已起的 postgres 必须被清掉
	if !fd.has("rm", "-f", "lathe-verify-t5-postgres") {
		t.Errorf("应清理已起的部分，实际调用：%v", fd.calls)
	}
	// 网络也要清
	if !fd.has("network", "rm", "lathe-verify-t5-net") {
		t.Errorf("应清理验证网络，实际调用：%v", fd.calls)
	}
}

// Down 清容器 + 网络，且对 nil 栈安全（调用方常写 defer st.Down()，
// 而 st 可能是「没声明依赖」时返回的 nil）。
func TestVerifyStackDownIsNilSafe(t *testing.T) {
	var st *VerifyStack
	if err := st.Down(context.Background()); err != nil {
		t.Errorf("nil 栈的 Down 应安全返回，得到 %v", err)
	}
}

func TestVerifyStackDownCleansContainersAndNetwork(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{"inspect": {stdout: "6379\n"}}}
	m, _ := newTestManager(t, fd, 100, 100)

	st, err := m.UpVerifyStack(context.Background(), 11, []string{"redis"})
	if err != nil {
		t.Fatalf("UpVerifyStack 失败: %v", err)
	}
	if err := st.Down(context.Background()); err != nil {
		t.Fatalf("Down 失败: %v", err)
	}
	if !fd.has("rm", "-f", "lathe-verify-t11-redis") {
		t.Errorf("应清掉容器，实际：%v", fd.calls)
	}
	if !fd.has("network", "rm", "lathe-verify-t11-net") {
		t.Errorf("应清掉网络，实际：%v", fd.calls)
	}
}

// ★ B2：Down 必须自己解绑父 ctx 的取消。
//
// Manager.exec 是 exec.CommandContext：ctx 一旦取消，连 `docker rm -f`
// 都执行不了，立即返回 context canceled。而拆栈用的正是流水线那个根
// ctx（SIGINT/SIGTERM 优雅退出时被取消）—— 后果是 heavy 验证正在跑时
// Ctrl-C 重启 serve，三条 docker 命令全部瞬时失败，postgres 容器 +
// 网络永久留在机器上。
func TestVerifyStackDownSurvivesCancelledContext(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{"inspect": {stdout: "6379\n"}}}
	m, _ := newTestManager(t, fd, 100, 100)

	ctx := context.Background()
	st, err := m.UpVerifyStack(ctx, 21, []string{"redis"})
	if err != nil {
		t.Fatalf("UpVerifyStack 失败: %v", err)
	}

	// 优雅退出：父 ctx 被取消（等价于 SIGINT 打到 serve）
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if err := st.Down(cancelled); err != nil {
		t.Errorf("Down 不该因父 ctx 取消而失败，得到 %v", err)
	}
	if !fd.has("rm", "-f", "lathe-verify-t21-redis") {
		t.Errorf("父 ctx 已取消时仍须删除容器，实际：%v", fd.calls)
	}
	if !fd.has("network", "rm", "lathe-verify-t21-net") {
		t.Errorf("父 ctx 已取消时仍须删除网络，实际：%v", fd.calls)
	}
}

// ★ B2（第二处）：就绪等待因 ctx 取消返回时，半栈清理也必须走安全路径。
//
// 起栈失败常常正是 ctx 被取消导致的；用同一根死 ctx 清理等于一条
// docker 命令都不执行，起了一半的容器同样留下。
func TestUpVerifyStackHalfCleanupSurvivesCancelledContext(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	// 就绪探测永远失败（docker exec 返回错误），逼出「起了一半」的路径。
	// 其余 docker 子命令照常成功，才走得到就绪探测那一步。
	fd.runHook = func(ctx context.Context, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "exec" {
			return "", fmt.Errorf("probe failed")
		}
		return "", nil
	}
	m, _ := newTestManager(t, fd, 100, 100)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就是死的 ctx —— 最严苛的形态

	// waitReady 先探到 probe 失败，随即在 select 里立刻看到死 ctx
	_, err := m.UpVerifyStack(ctx, 31, []string{"redis"})
	if err == nil {
		t.Fatal("死 ctx 下起栈应失败")
	}
	if !fd.has("rm", "-f", "lathe-verify-t31-redis") {
		t.Errorf("半栈清理必须在死 ctx 下仍执行 rm -f，实际：%v", fd.calls)
	}
	if !fd.has("network", "rm", "lathe-verify-t31-net") {
		t.Errorf("半栈清理必须在死 ctx 下仍删除网络，实际：%v", fd.calls)
	}
}

// ★ B3 第一段：起栈前按精确名字回收 —— 幂等的名字回收。
//
// 容器名是确定性的 lathe-verify-t<id>-<infra>。上一次拆栈失败留下的
// 残骸会让这一次的 run 撞上「Conflict. The container name is already
// in use」→ 走判死分支 → 任务永久红，直到有人手工 docker rm -f。
func TestUpVerifyStackReclaimsStaleNames(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{"inspect": {stdout: "5432\n"}}}
	m, _ := newTestManager(t, fd, 100, 100)

	if _, err := m.UpVerifyStack(context.Background(), 42, []string{"postgres"}); err != nil {
		t.Fatalf("UpVerifyStack 失败: %v", err)
	}

	// 起栈之前必须先删同名容器与同名网络
	rmIdx, runIdx := -1, -1
	for i, c := range fd.calls {
		joined := strings.Join(c, " ")
		if rmIdx < 0 && joined == "docker rm -f lathe-verify-t42-postgres" {
			rmIdx = i
		}
		if runIdx < 0 && strings.Contains(joined, "run -d --name lathe-verify-t42-postgres") {
			runIdx = i
		}
	}
	if rmIdx < 0 {
		t.Fatalf("起栈前应先 rm -f 同名容器，实际调用：%v", fd.calls)
	}
	if runIdx < 0 {
		t.Fatalf("应起容器，实际调用：%v", fd.calls)
	}
	if rmIdx > runIdx {
		t.Errorf("名字回收必须发生在 run 之前（回收在下标 %d，run 在下标 %d）", rmIdx, runIdx)
	}
	if !fd.has("network", "rm", "lathe-verify-t42-net") {
		t.Errorf("同名网络也要回收（network create 撞同名同样报 Conflict），实际：%v", fd.calls)
	}
	// 回收必须发生在 network create 之前
	netRm, netCreate := -1, -1
	for i, c := range fd.calls {
		joined := strings.Join(c, " ")
		if netRm < 0 && joined == "docker network rm lathe-verify-t42-net" {
			netRm = i
		}
		if netCreate < 0 && strings.Contains(joined, "network create") {
			netCreate = i
		}
	}
	if netRm < 0 || netCreate < 0 || netRm > netCreate {
		t.Errorf("网络回收必须发生在 network create 之前（回收 %d，create %d）：%v", netRm, netCreate, fd.calls)
	}
}

// ★ B3 第二段：开机清扫 —— 验证栈唯一的出口。
//
// 验证栈刻意不打 labelTask（正确：否则人点「停止预览」会删掉正在跑的
// 验证容器），代价是它掉出所有既有清理路径。预览容器泄漏了还有人能
// 点停止，验证容器泄漏了没有任何出口。按定义验证栈不该跨进程存活，
// 所以开机时按 labelVerify 无条件全清是安全的。
func TestSweepVerifyStacksRemovesLabeledResources(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps":      {stdout: "c1\nc2\n"},
		"network": {stdout: "n1\n"},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	n, nets, err := m.SweepVerifyStacks(context.Background())
	if err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if n != 2 || nets != 1 {
		t.Errorf("清扫计数 = (%d,%d)，期望 (2,1)", n, nets)
	}
	if !fd.has("ps", "-aq", "--filter", "label="+labelVerify) {
		t.Errorf("应按 %s 标签查容器，实际：%v", labelVerify, fd.calls)
	}
	if !fd.has("network", "ls", "-q", "--filter", "label="+labelVerify) {
		t.Errorf("应按 %s 标签查网络，实际：%v", labelVerify, fd.calls)
	}
	if !fd.has("rm", "-f", "c1", "c2") {
		t.Errorf("应删掉查到的容器，实际：%v", fd.calls)
	}
	if !fd.has("network", "rm", "n1") {
		t.Errorf("应删掉查到的网络，实际：%v", fd.calls)
	}
}

// 清扫按 labelVerify 查，绝不能碰预览资源 —— 预览栈跨重启保留现场
// 是刻意的，开机清扫把它删掉就是另一场事故。
func TestSweepVerifyStacksNeverTouchesPreviewLabel(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)

	if _, _, err := m.SweepVerifyStacks(context.Background()); err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	for _, c := range fd.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, labelPreview) {
			t.Errorf("清扫不该碰 %s 资源：%s", labelPreview, joined)
		}
		if strings.Contains(joined, labelTask) {
			t.Errorf("清扫不该碰 %s 资源：%s", labelTask, joined)
		}
	}
}

// 清扫查不到资源时不做任何删除调用（无谓的 rm 会污染 docker 事件流）。
func TestSweepVerifyStacksNoopWhenClean(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)

	n, nets, err := m.SweepVerifyStacks(context.Background())
	if err != nil || n != 0 || nets != 0 {
		t.Fatalf("空机器上清扫应无操作，得到 (%d,%d,%v)", n, nets, err)
	}
	if fd.has("rm", "-f") || fd.has("network", "rm") {
		t.Errorf("没有残留资源时不该调删除，实际：%v", fd.calls)
	}
}

// ★ docker 挂了必须能与「水位超阈值」区分开。
//
// CheckResources 的第一分支就是 !DockerOK → Allowed=false，与水位
// 共用错误身份时 runner 会走降级分支、日志写「资源水位不允许」——
// 把人引去调阈值，而真正该做的是把 docker 起来。
func TestUpVerifyStackDockerDownIsItsOwnError(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"version": {err: fmt.Errorf("Cannot connect to the Docker daemon")},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	_, err := m.UpVerifyStack(context.Background(), 8, []string{"postgres"})
	if err == nil {
		t.Fatal("docker 不可用应报错")
	}
	if !errors.Is(err, ErrDockerUnavailableVerify) {
		t.Errorf("应可判定为 docker 不可用，得到 %v", err)
	}
	if errors.Is(err, ErrVerifyStackOverThreshold) {
		t.Errorf("docker 不可用不该冒充「资源水位超阈值」—— 那会让 runner 悄悄降级：%v", err)
	}
	if fd.has("run", "-d") {
		t.Errorf("docker 不可用时不该尝试起容器，实际：%v", fd.calls)
	}
}

// ★ 宿主侧 TCP 探活：容器内的就绪探测走 unix socket，覆盖不到
// 「端口是否已在宿主上可用」。
//
// postgres 官方 entrypoint 首次初始化时先起一个 listen_addresses=”
// 的临时服务器（只听 unix socket）建库，跑完 initdb.d 后【停掉再以正常
// 配置重启】。psql/mysql 探测不带 -h，走的正是 unix socket —— 它在
// 临时服务器阶段就成功。于是就绪返回、回读到端口，验证命令立刻连
// 127.0.0.1:<port>，而 postgres 正在重启中 → connection refused → 测试红。
func TestWaitHostPortSucceedsOnListeningPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)
	m.hostProbe = realHostProbe // 用真探针：这一条测的就是它

	if err := m.waitHostPort(context.Background(), port); err != nil {
		t.Errorf("可连的端口应探活成功，得到 %v", err)
	}
}

func TestWaitHostPortFailsOnClosedPort(t *testing.T) {
	// 拿一个刚释放的端口：listen 后立刻关，端口上不会有人接
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)
	m.hostProbe = realHostProbe

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = m.waitHostPort(ctx, port)
	if err == nil {
		t.Error("无人接听的端口不该探活成功（否则偶发红会以「就绪」的名义放行）")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("超时应如实返回 ctx 错误，得到 %v", err)
	}
}

// UpVerifyStack 里那次探活必须真的发生（排在回读端口之后）。
// 用自建探针（测试构造的 Manager 没有默认探针）钉调用序列。
func TestUpVerifyStackProbesHostPortAfterReadingIt(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{"inspect": {stdout: "5432\n"}}}
	m, _ := newTestManager(t, fd, 100, 100)
	var probed []int
	m.hostProbe = func(ctx context.Context, port int) error {
		probed = append(probed, port)
		return nil
	}

	if _, err := m.UpVerifyStack(context.Background(), 9, []string{"postgres"}); err != nil {
		t.Fatalf("UpVerifyStack 失败: %v", err)
	}
	if len(probed) != 1 || probed[0] != 5432 {
		t.Errorf("应对回读到的宿主端口探活一次，实际探了 %v", probed)
	}
}

package preview

import (
	"context"
	"strings"
	"testing"
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

	// 必须用 -p 0:5432 让 docker 挑端口 —— 钉死端口会让并发任务撞车，
	// 而「并发任务互不干扰」正是本项的目的
	if !fd.has("run", "-p", "0:5432") {
		t.Errorf("应以 -p 0:5432 发布随机端口，实际调用：%v", fd.calls)
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

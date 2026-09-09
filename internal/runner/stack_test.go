package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeStackUp 是 VerifyStackUp 的假件。
type fakeStackUp struct {
	env       map[string]string
	upErr     error
	upCalls   [][]string
	downCalls int
}

func (f *fakeStackUp) UpVerifyStack(ctx context.Context, taskID int64, infra []string) (VerifyStackHandle, error) {
	f.upCalls = append(f.upCalls, infra)
	if f.upErr != nil {
		return nil, f.upErr
	}
	if len(infra) == 0 {
		return nil, nil
	}
	return &fakeStackHandle{f: f}, nil
}

type fakeStackHandle struct{ f *fakeStackUp }

func (h *fakeStackHandle) StackEnv() map[string]string { return h.f.env }
func (h *fakeStackHandle) Down(ctx context.Context) error {
	h.f.downCalls++
	return nil
}

// ★ AC5：水位超阈值时**降级为无隔离执行**，不让验证失败。
//
// 机器忙不是任务的错，回落仍能给出验证结论（只是并发污染风险回到
// 本项之前的水平）。排队等待不合适：水位可能长时间不降，
// 那会让验证槽位无限期挂着。
func TestStackUnavailableDegradesNotFails(t *testing.T) {
	wrapped := fmt.Errorf("%w: 磁盘占用 95%% 已达阈值 90%%", ErrStackOverThreshold)
	if !stackUnavailable(wrapped) {
		t.Error("水位错误应被判定为「该降级」")
	}
}

// ★ AC7：其它起栈错误必须判死，不能悄悄降级。
//
// 未知依赖名、镜像拉不下来、就绪超时 —— 都是配置或环境问题，
// 该让人看见。悄悄降级会让人以为隔离生效了，而实际从没起来过。
func TestOtherStackErrorsDoNotDegrade(t *testing.T) {
	for _, err := range []error{
		errors.New("未知的基础设施 \"postgres9\""),
		errors.New("镜像拉取失败"),
		fmt.Errorf("就绪等待超时: %w", context.DeadlineExceeded),
	} {
		if stackUnavailable(err) {
			t.Errorf("非水位错误不该降级：%v", err)
		}
	}
}

// ★ AC7：起栈失败有独立的错误身份，绝不冒充复现阶段的错误。
//
// 若冒充成 StepReproFail + StatusError，会被 redEnvError 归类为
// 「环境问题、失败留现场」—— 语义上恰好也对，但错误信息会让人
// 以为是复现测试跑不起来，于是去查一个根本没问题的测试命令。
// 红阶段的三分路由依赖错误身份，混进第四种来源就会误判。
func TestVerifyStackUpErrorHasOwnIdentity(t *testing.T) {
	wrapped := fmt.Errorf("%w: 启动验证依赖 postgres 失败", ErrVerifyStackUp)

	if !errors.Is(wrapped, ErrVerifyStackUp) {
		t.Error("起栈失败应可用 errors.Is 判定")
	}
	// 绝不能与复现契约违例混淆 —— 后者会被 isReproContractErr 判为
	// 「agent 能自己修」而进入修复回路，让 agent 去修一个 docker 问题
	if isReproContractErr(wrapped) {
		t.Error("起栈失败不该被当成复现契约违例（那会让 agent 去修一个 docker 问题）")
	}
	if errors.Is(wrapped, ErrNoReproTests) || errors.Is(wrapped, ErrReproManifest) {
		t.Error("起栈失败不该与复现测试的错误身份重叠")
	}
	// 也不该被当成「该降级」
	if stackUnavailable(wrapped) {
		t.Error("起栈失败（非水位原因）不该降级")
	}
}

// 未声明依赖时不起栈：返回 nil handle，且调用方的 nil 判断要能成立。
//
// 这一条防的是「包着 nil 的接口值」这个 Go 经典坑：返回
// (*fakeStackHandle)(nil) 会让 `handle != nil` 意外为真，
// 然后对 nil 指针调方法当场 panic。
func TestStackUpReturnsNilHandleWithoutInfra(t *testing.T) {
	f := &fakeStackUp{}
	h, err := f.UpVerifyStack(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("无依赖不该报错: %v", err)
	}
	if h != nil {
		t.Errorf("无依赖应返回 nil handle（而非包着 nil 的接口值），得到 %#v", h)
	}
}

// 起栈成功时连接串传出来，且 Down 被调用（生命周期对称）。
func TestStackHandleEnvAndDown(t *testing.T) {
	f := &fakeStackUp{env: map[string]string{"DATABASE_URL": "postgres://127.0.0.1:49173/app"}}
	h, err := f.UpVerifyStack(context.Background(), 1, []string{"postgres"})
	if err != nil || h == nil {
		t.Fatalf("起栈应成功，得到 h=%v err=%v", h, err)
	}
	if got := h.StackEnv()["DATABASE_URL"]; got != "postgres://127.0.0.1:49173/app" {
		t.Errorf("连接串未传出，得到 %q", got)
	}
	if err := h.Down(context.Background()); err != nil {
		t.Fatalf("Down 失败: %v", err)
	}
	if f.downCalls != 1 {
		t.Errorf("Down 应被调一次，实际 %d", f.downCalls)
	}
}

// 栈的连接串必须压过宿主环境里的同名变量。
//
// 开发机上常导出过 DATABASE_URL 指向共享库；不覆盖就等于隔离白做 ——
// 测试照样连到那个共享库上，并发污染一点没少。
func TestMergeEnvStackWins(t *testing.T) {
	base := []string{"PATH=/usr/bin", "DATABASE_URL=postgres://shared/db", "CI=1"}
	extra := map[string]string{"DATABASE_URL": "postgres://127.0.0.1:49173/app"}

	got := mergeEnv(base, extra)

	var found, dup int
	for _, kv := range got {
		if kv == "DATABASE_URL=postgres://127.0.0.1:49173/app" {
			found++
		}
		if kv == "DATABASE_URL=postgres://shared/db" {
			dup++
		}
	}
	if found != 1 {
		t.Errorf("隔离栈的连接串应存在且唯一，实际 %d 条", found)
	}
	if dup != 0 {
		t.Errorf("宿主的同名变量必须被覆盖掉，实际仍有 %d 条", dup)
	}
	// 无关变量原样保留
	var hasPath bool
	for _, kv := range got {
		if kv == "PATH=/usr/bin" {
			hasPath = true
		}
	}
	if !hasPath {
		t.Error("无关变量不该被丢掉")
	}
}

// extra 为空时原样返回，不做无谓的拷贝与重排。
func TestMergeEnvEmptyExtraIsIdentity(t *testing.T) {
	base := []string{"A=1", "B=2"}
	got := mergeEnv(base, nil)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("空 extra 应原样返回，得到 %v", got)
	}
}

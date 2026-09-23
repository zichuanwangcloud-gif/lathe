package prd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 一键生成的测试。同样全内存：这一层要验的是「顺序对不对、正文全不全、
// 失败了状态干不干净」，与 SQL 无关。

// fakeIssues 记下每次建单的入参，供断言正文内容。
type fakeIssues struct {
	specs []IssueSpec
	err   error
}

func (f *fakeIssues) Create(_ context.Context, s IssueSpec) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.specs = append(f.specs, s)
	// key 按建单顺序生成，测试里用它反查节点顺序。
	return "LT-" + string(rune('0'+len(f.specs))), nil
}

// fakeFlows 记下建图入参。
type fakeFlows struct {
	spec *FlowSpec
	err  error
}

func (f *fakeFlows) Create(_ context.Context, s FlowSpec) (*FlowResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	cp := s
	f.spec = &cp
	tasks := make([]CreatedTask, len(s.Nodes))
	for i, n := range s.Nodes {
		tasks[i] = CreatedTask{ID: int64(i + 1), IssueKey: n.IssueKey, State: "queued"}
	}
	return &FlowResult{FlowID: 42, Tasks: tasks}, nil
}

// convertFixture 造一份 approved、带两个任务块的 PRD。
//
// 任务块刻意按「后继在前」的顺序放（T2 依赖 T1，但 T2 排在切片前面），
// 这样只要 Convert 不做拓扑排序，建图时 T2 就会引用一个还不存在的前驱。
func convertFixture(t *testing.T) (*Service, *fakeStore, *fakeIssues, *fakeFlows) {
	t.Helper()

	doc := &Document{
		Type:     TypeFeature,
		OneLiner: Section{Status: StatusConfirmed, Body: "让管理员能配预览阈值"},
		Criteria: []Criterion{
			{
				ID: "AC-1", Category: ACCategory("happy_path"),
				Given: "管理员已登录", When: "把并发上限改成 5", Then: "设置页显示 5 且立即生效",
				Evidence: "GET /api/admin/settings 返回 previewMaxConcurrent=5",
				Verify:   VerifyAuto,
				Examples: []Example{{Positive: true, Text: "填 5 保存成功"}},
			},
			{
				ID: "AC-2", Category: ACCategory("boundary"),
				Given: "管理员已登录", When: "把并发上限改成 0", Then: "保存被拒绝并提示必须为正整数",
				Evidence: "HTTP 400，响应体 error 非空",
				Verify:   VerifyAuto,
			},
		},
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	blocks := &TaskBlocks{
		Version: BlocksVersion,
		Repo:    "acme/lathe",
		Tasks: []TaskBlock{
			{
				Key: "T2", Title: "设置页接上新阈值", Kind: KindFeature,
				DependsOn: "T1", Description: "前端设置页加两个输入框",
				Acceptance: []string{"AC-2"},
				Estimate:   Estimate{Lines: 120, Files: 3},
			},
			{
				Key: "T1", Title: "阈值改为可配置", Kind: KindFeature,
				Description: "把写死的常量挪进 system_settings",
				FilesHint:   []string{"internal/preview/manager.go"},
				Acceptance:  []string{"AC-1"},
				Estimate:    Estimate{Lines: 200, Files: 4},
			},
		},
	}
	blocksRaw, err := blocks.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	fs := newFakeStore()
	fs.row.State = string(StateApproved)
	fs.row.Document = raw
	fs.row.TaskBlocks = blocksRaw

	fi := &fakeIssues{}
	ff := &fakeFlows{}
	svc := &Service{Store: fs, Issues: fi, Flows: ff}
	return svc, fs, fi, ff
}

func TestConvertRequiresApproved(t *testing.T) {
	for _, state := range []State{StateDrafting, StateAwaitingAnswers, StateReadyForReview} {
		t.Run(string(state), func(t *testing.T) {
			svc, fs, fi, _ := convertFixture(t)
			fs.row.State = string(state)

			_, err := svc.Convert(context.Background(), 1, 7)
			if !errors.Is(err, ErrNotApproved) {
				t.Fatalf("状态 %s 应拒绝生成，got err=%v", state, err)
			}
			if len(fi.specs) != 0 {
				t.Errorf("被拒绝时不该建任何工单，got=%d 张", len(fi.specs))
			}
		})
	}
}

func TestConvertRejectsAlreadyConverted(t *testing.T) {
	svc, fs, fi, _ := convertFixture(t)
	existing := int64(9)
	fs.row.GeneratedFlowID = &existing

	_, err := svc.Convert(context.Background(), 1, 7)
	if !errors.Is(err, ErrAlreadyConverted) {
		t.Fatalf("已生成过的 PRD 应拒绝重复生成，got err=%v", err)
	}
	if len(fi.specs) != 0 {
		t.Errorf("重复生成被拒时不该建工单，got=%d 张", len(fi.specs))
	}
}

// 前驱必须排在后继前面，且 DependsOnIndex 指向正确的下标。
//
// fixture 故意把 T2（依赖 T1）放在切片首位：不排序的话这里必然挂。
func TestConvertOrdersTasksTopologically(t *testing.T) {
	svc, _, fi, ff := convertFixture(t)

	if _, err := svc.Convert(context.Background(), 1, 7); err != nil {
		t.Fatal(err)
	}

	if got, want := len(fi.specs), 2; got != want {
		t.Fatalf("应建 %d 张工单，got=%d", want, got)
	}
	if got, want := fi.specs[0].Title, "阈值改为可配置"; got != want {
		t.Errorf("首个建的应是无依赖的 T1\ngot  = %q\nwant = %q", got, want)
	}

	if ff.spec == nil {
		t.Fatal("没有建图")
	}
	nodes := ff.spec.Nodes
	if len(nodes) != 2 {
		t.Fatalf("图应有 2 个节点，got=%d", len(nodes))
	}
	if nodes[0].DependsOnIndex != nil {
		t.Errorf("根节点的 DependsOnIndex 应为 nil，got=%v", *nodes[0].DependsOnIndex)
	}
	if nodes[1].DependsOnIndex == nil {
		t.Fatal("后继节点的 DependsOnIndex 不该为 nil")
	}
	if got, want := *nodes[1].DependsOnIndex, 0; got != want {
		t.Errorf("后继应依赖下标 %d，got=%d", want, got)
	}
}

// 工单正文必须带 AC 全文——实现 agent 看不到 PRD，只写编号等于没写。
func TestConvertRendersACFullTextIntoIssue(t *testing.T) {
	svc, _, fi, _ := convertFixture(t)

	if _, err := svc.Convert(context.Background(), 1, 7); err != nil {
		t.Fatal(err)
	}

	body := fi.specs[0].Description
	for _, want := range []string{
		"AC-1",
		"管理员已登录",        // Given
		"把并发上限改成 5",     // When
		"设置页显示 5 且立即生效", // Then
		"GET /api/admin/settings 返回 previewMaxConcurrent=5", // 判据
		"internal/preview/manager.go",                       // 参考文件
		"T1",                                                // 来源任务块
	} {
		if !strings.Contains(body, want) {
			t.Errorf("工单正文缺少 %q\n--- 正文 ---\n%s", want, body)
		}
	}

	// 只交付 AC-1 的任务不该把 AC-2 也抄进去，否则 agent 会去实现别人的活。
	if strings.Contains(body, "把并发上限改成 0") {
		t.Errorf("正文混入了不属于本任务的 AC-2\n--- 正文 ---\n%s", body)
	}
}

func TestConvertMarksConvertedAndRecordsFlowID(t *testing.T) {
	svc, fs, _, _ := convertFixture(t)

	res, err := svc.Convert(context.Background(), 1, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.FlowID, int64(42); got != want {
		t.Errorf("FlowID got=%d want=%d", got, want)
	}
	if got, want := fs.row.State, string(StateConverted); got != want {
		t.Errorf("PRD 状态 got=%q want=%q", got, want)
	}
	if fs.row.GeneratedFlowID == nil {
		t.Fatal("generated_flow_id 没有回填")
	}
	if got, want := *fs.row.GeneratedFlowID, int64(42); got != want {
		t.Errorf("generated_flow_id got=%d want=%d", got, want)
	}
}

// 建图失败必须把 PRD 放回 approved——卡在 converted 的话人既不能重试
// 也不能改（approved 之后已冻结），这份 PRD 就废了。
func TestConvertRollsBackWhenFlowFails(t *testing.T) {
	svc, fs, _, ff := convertFixture(t)
	ff.err = errors.New("建图炸了")

	_, err := svc.Convert(context.Background(), 1, 7)
	if err == nil {
		t.Fatal("建图失败时 Convert 应报错")
	}
	if got, want := fs.row.State, string(StateApproved); got != want {
		t.Errorf("失败后应回滚到 %q，got=%q —— PRD 卡在 converted 就再也动不了了", want, got)
	}
	if fs.row.GeneratedFlowID != nil {
		t.Errorf("失败后不该留下 generated_flow_id，got=%d", *fs.row.GeneratedFlowID)
	}
}

// 建工单失败同样要回滚。
func TestConvertRollsBackWhenIssueFails(t *testing.T) {
	svc, fs, fi, _ := convertFixture(t)
	fi.err = errors.New("建单炸了")

	if _, err := svc.Convert(context.Background(), 1, 7); err == nil {
		t.Fatal("建单失败时 Convert 应报错")
	}
	if got, want := fs.row.State, string(StateApproved); got != want {
		t.Errorf("失败后应回滚到 %q，got=%q", want, got)
	}
}

func TestConvertRejectsEmptyTaskBlocks(t *testing.T) {
	svc, fs, _, _ := convertFixture(t)
	empty, err := (&TaskBlocks{Version: BlocksVersion, Repo: "acme/lathe"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fs.row.TaskBlocks = empty

	if _, err := svc.Convert(context.Background(), 1, 7); !errors.Is(err, ErrNoTasks) {
		t.Fatalf("任务块为空应报 ErrNoTasks，got=%v", err)
	}
	if got, want := fs.row.State, string(StateApproved); got != want {
		t.Errorf("被拒绝时状态不该变，got=%q want=%q", got, want)
	}
}

// 没装配 Issues / Flows 时要明确报错，而不是 panic 或静默成功。
func TestConvertWithoutWiring(t *testing.T) {
	fs := newFakeStore()
	fs.row.State = string(StateApproved)
	svc := &Service{Store: fs}

	if _, err := svc.Convert(context.Background(), 1, 7); !errors.Is(err, ErrConvertUnavailable) {
		t.Fatalf("未装配时应报 ErrConvertUnavailable，got=%v", err)
	}
}

func TestValidateDisposition(t *testing.T) {
	cases := []struct {
		name        string
		disposition string
		note        string
		wantErr     bool
	}{
		{"fix 不需要理由", DispositionFix, "", false},
		{"reject 不需要理由", DispositionReject, "", false},
		{"accept 带理由可以", DispositionAccept, "已在 §6 方案里排除", false},
		{"accept 缺理由要拒绝", DispositionAccept, "   ", true},
		{"空处置要拒绝", "", "", true},
		{"未知处置要拒绝", "maybe", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDisposition(tc.disposition, tc.note)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("ValidateDisposition(%q, %q) err=%v，want wantErr=%v",
					tc.disposition, tc.note, err, tc.wantErr)
			}
		})
	}
}

// flowName 截断必须按 rune——按 byte 切会把中文切成半个字符，
// 正是 AGENTS.md §9 那条「事件落库 UTF-8 截断整批丢弃」的形状。
func TestFlowNameTruncatesByRune(t *testing.T) {
	long := strings.Repeat("预览阈值可配置", 20)
	doc := &Document{OneLiner: Section{Body: long}}

	name := flowName(7, doc)
	if !json.Valid([]byte(`"` + name + `"`)) {
		t.Fatalf("截断后不是合法 UTF-8：%q", name)
	}
	if !strings.HasPrefix(name, "PRD #7 ") {
		t.Errorf("图名应带 PRD 编号前缀，got=%q", name)
	}
	if got := len([]rune(name)); got > 60 {
		t.Errorf("图名过长：%d 个 rune", got)
	}
}

func TestFlowNameFallsBackToID(t *testing.T) {
	name := flowName(7, &Document{})
	if got, want := name, "PRD #7"; got != want {
		t.Errorf("没有一句话时应退回编号\ngot  = %q\nwant = %q", got, want)
	}
}

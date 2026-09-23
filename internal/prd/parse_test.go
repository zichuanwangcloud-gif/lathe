package prd

import (
	"strings"
	"testing"
)

// 解析层的测试。重点在「拦幻觉」那一档：模型编出来的悬空编号、未知枚举
// 值、自封 confirmed，都必须在入口被拒，而不是几轮之后以「自检莫名不过」
// 的形式爆出来。

func TestExtractJSONToleratesSurroundingProse(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool // 能否截出
	}{
		{"裸 JSON", `{"type":"feature","document":{}}`, true},
		{"前面有客套话", "好的，这是我的分析：\n{\"type\":\"feature\",\"document\":{}}", true},
		{"包在代码围栏里", "```json\n{\"type\":\"feature\",\"document\":{}}\n```", true},
		{"前后都有话", "分析如下:\n{\"type\":\"feature\",\"document\":{}}\n以上。", true},
		{"完全没有 JSON", "我需要更多信息才能继续。", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRoundResult(tc.text, StageUnderstand)
			if tc.want && err != nil {
				t.Errorf("应当能解析，got err=%v", err)
			}
			if !tc.want && err == nil {
				t.Error("没有 JSON 却解析成功了")
			}
		})
	}
}

// 逐字段拦幻觉：每个用例只坏一处。
func TestParseRejectsHallucinations(t *testing.T) {
	tests := []struct {
		name            string
		doc             string
		wantErrContains string
	}{
		{
			name: "AC 引用不存在的目标",
			doc: `{"goalList":[{"id":"G-1"}],
			       "scenarioList":[{"id":"S-1"}],
			       "criteria":[{"id":"AC-1","category":"happy_path","goalRefs":["G-9"],"scenarioRefs":["S-1"]}]}`,
			wantErrContains: "不存在的目标 G-9",
		},
		{
			name: "AC 引用不存在的场景",
			doc: `{"goalList":[{"id":"G-1"}],
			       "criteria":[{"id":"AC-1","category":"happy_path","goalRefs":["G-1"],"scenarioRefs":["S-7"]}]}`,
			wantErrContains: "不存在的场景 S-7",
		},
		{
			name:            "category 未知",
			doc:             `{"criteria":[{"id":"AC-1","category":"happy path"}]}`,
			wantErrContains: "category",
		},
		{
			name:            "verify 未知",
			doc:             `{"criteria":[{"id":"AC-1","category":"happy_path","verify":"eyeball"}]}`,
			wantErrContains: "verify",
		},
		{
			name:            "AC 编号重复",
			doc:             `{"criteria":[{"id":"AC-1","category":"happy_path"},{"id":"AC-1","category":"boundary"}]}`,
			wantErrContains: "AC 编号 AC-1 重复",
		},
		{
			name:            "目标编号重复",
			doc:             `{"goalList":[{"id":"G-1"},{"id":"G-1"}]}`,
			wantErrContains: "目标编号 G-1 重复",
		},
		{
			name:            "任务 kind 不合法",
			doc:             `{"tasks":[{"key":"T1","kind":"chore"}]}`,
			wantErrContains: "kind",
		},
		{
			name:            "任务引用不存在的 AC",
			doc:             `{"tasks":[{"key":"T1","kind":"fix","acceptance":["AC-3"]}]}`,
			wantErrContains: "不存在的 AC-3",
		},
		{
			name:            "任务依赖不存在的前驱",
			doc:             `{"tasks":[{"key":"T1","kind":"fix","dependsOn":"T9"}]}`,
			wantErrContains: "依赖的 T9 不存在",
		},
		{
			name:            "节的 status 未知",
			doc:             `{"oneLiner":{"status":"maybe","body":"x"}}`,
			wantErrContains: "status",
		},
		{
			name:            "PRD 类型未知",
			doc:             `{}`,
			wantErrContains: "PRD 类型",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			typ := `"feature"`
			if tc.name == "PRD 类型未知" {
				typ = `"epic"`
			}
			text := `{"type":` + typ + `,"document":` + tc.doc + `}`

			_, err := ParseRoundResult(text, StageConverge)
			if err == nil {
				t.Fatalf("幻觉没被拦住，want err contains %q", tc.wantErrContains)
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("错误信息应指出问题所在\ngot:  %v\nwant contains: %q", err, tc.wantErrContains)
			}
		})
	}
}

// 模型不许自封「人已确认」——那是人的动作。
func TestParseDowngradesSelfClaimedConfirmed(t *testing.T) {
	text := `{"type":"feature","document":{
		"oneLiner": {"status":"confirmed","body":"让管理员配预览阈值"},
		"problem":  {"status":"inferred","body":"阈值写死"}
	}}`

	got, err := ParseRoundResult(text, StageUnderstand)
	if err != nil {
		t.Fatal(err)
	}
	if got.Document.OneLiner.Status != StatusInferred {
		t.Errorf("模型自封 confirmed 必须降级为 inferred（确认是人的动作），got=%q",
			got.Document.OneLiner.Status)
	}
}

// 缺 status 视作推断，不能默默当成已确认。
func TestParseDefaultsMissingStatusToInferred(t *testing.T) {
	text := `{"type":"feature","document":{"oneLiner":{"body":"让管理员配预览阈值"}}}`

	got, err := ParseRoundResult(text, StageUnderstand)
	if err != nil {
		t.Fatal(err)
	}
	if got.Document.OneLiner.Status != StatusInferred {
		t.Errorf("缺 status 应默认 inferred，got=%q", got.Document.OneLiner.Status)
	}
}

// 每轮 ≤ 3 问的纪律（D10-7）：多了截断，不让它绕过。
func TestParseTruncatesExcessQuestions(t *testing.T) {
	text := `{"type":"feature","document":{},
	          "questions":["q1","q2","q3","q4","q5"]}`

	got, err := ParseRoundResult(text, StageUnderstand)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Questions) != maxQuestionsPerRound {
		t.Errorf("每轮最多 %d 问，got=%d（问题轰炸的结果是人不答）",
			maxQuestionsPerRound, len(got.Questions))
	}
}

// 拆分阶段没产出任务 = 这一轮什么都没推进，必须报错。
func TestParseRequiresTasksAtSplitStage(t *testing.T) {
	text := `{"type":"feature","document":{"taskSplit":{"status":"inferred","body":"待补"}}}`

	if _, err := ParseRoundResult(text, StageSplit); err == nil {
		t.Error("拆分阶段没有任何任务块却解析成功了")
	}
	// 同一份输入在早期阶段是正常中间态，不该报错。
	if _, err := ParseRoundResult(text, StageUnderstand); err != nil {
		t.Errorf("阶段 A 还没拆分是正常的，不该报错: %v", err)
	}
}

// 完整的一轮输出能原样往返，不丢字段。
func TestParseRoundTripsFullDocument(t *testing.T) {
	text := `{"type":"feature","document":{
		"oneLiner":{"status":"inferred","body":"让管理员在系统设置里配置预览阈值"},
		"goalList":[{"id":"G-1","text":"阈值可配","measure":"改完立即生效"}],
		"scenarioList":[{"id":"S-1","title":"改阈值","given":"已登录","when":"改为 75","then":"80% 时被拒"}],
		"criteria":[{"id":"AC-1","category":"happy_path","given":"阈值 75","when":"占用 80%",
		             "then":"返回 409","evidence":"HTTP 409","verify":"auto_test",
		             "goalRefs":["G-1"],"scenarioRefs":["S-1"],"taskKey":"T1",
		             "reverseConfirm":"74% 不拦，接受吗"}],
		"tasks":[{"key":"T1","title":"阈值改配置项","kind":"feature",
		          "description":"改常量为读设置；交付测试证明可配且非法值被拒",
		          "acceptance":["AC-1"],"estimate":{"lines":120,"files":3}}],
		"evidences":[{"ref":"internal/preview/gate.go:31","note":"写死的常量"}]
	},"questions":["阈值是内存还是磁盘？"],"notes":"本轮判定为 feature"}`

	got, err := ParseRoundResult(text, StageSplit)
	if err != nil {
		t.Fatalf("完整输出应能解析: %v", err)
	}
	d := got.Document

	if d.Type != TypeFeature {
		t.Errorf("type: got %q, want feature", d.Type)
	}
	if len(d.GoalList) != 1 || d.GoalList[0].Measure != "改完立即生效" {
		t.Errorf("目标的达成判据丢了: %+v", d.GoalList)
	}
	if len(d.Criteria) != 1 || d.Criteria[0].Evidence != "HTTP 409" {
		t.Errorf("AC 判据丢了: %+v", d.Criteria)
	}
	if len(d.Tasks) != 1 || d.Tasks[0].Estimate.Lines != 120 {
		t.Errorf("任务估算丢了: %+v", d.Tasks)
	}
	if len(d.Evidences) != 1 || d.Evidences[0].Ref != "internal/preview/gate.go:31" {
		t.Errorf("代码证据丢了: %+v", d.Evidences)
	}
	if got.Notes != "本轮判定为 feature" {
		t.Errorf("notes 丢了: %q", got.Notes)
	}

	// 快照能序列化回 jsonb 并再解析回来（落库往返）。
	raw, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Criteria) != 1 || back.Criteria[0].ID != "AC-1" {
		t.Error("落库往返后 AC 丢了")
	}
}

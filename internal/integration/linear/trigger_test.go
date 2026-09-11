package linear

import "testing"

// ev 造一个 issue 事件。
func ev(action string, labels []string, stateType string, updatedFrom map[string]any) *WebhookEvent {
	e := &WebhookEvent{Action: action, Type: "Issue", UpdatedFrom: updatedFrom}
	for _, l := range labels {
		e.Data.Labels = append(e.Data.Labels, struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{ID: "id-" + l, Name: l})
	}
	if stateType != "" {
		e.Data.State = &struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}{Name: stateType, Type: stateType}
	}
	return e
}

// ★ AC3 的核心：标签名未配置时恒不触发。
//
// 不能让存量部署因为升级就突然开始按标签接单 —— 尤其某个仓库可能早就
// 在用 lathe:go 这个标签表示别的意思。
func TestIsLabelTriggeredOffWhenUnconfigured(t *testing.T) {
	e := ev("create", []string{"lathe:go"}, "", nil)
	if e.IsLabelTriggered("") {
		t.Error("标签名为空时绝不该触发接单")
	}
}

func TestIsLabelTriggered(t *testing.T) {
	cases := []struct {
		name  string
		e     *WebhookEvent
		label string
		want  bool
	}{
		{"create 带标签", ev("create", []string{"bug", "lathe:go"}, "", nil), "lathe:go", true},
		{"create 无标签", ev("create", []string{"bug"}, "", nil), "lathe:go", false},
		{"update 这次改了标签", ev("update", []string{"lathe:go"}, "", map[string]any{"labelIds": nil}), "lathe:go", true},
		// 这一条最要紧：issue 停在带标签的状态时改标题，不该重复接单
		{"update 改的是别的字段", ev("update", []string{"lathe:go"}, "", map[string]any{"title": "旧标题"}), "lathe:go", false},
		{"update 无 updatedFrom", ev("update", []string{"lathe:go"}, "", nil), "lathe:go", false},
		{"remove 一律不接", ev("remove", []string{"lathe:go"}, "", map[string]any{"labelIds": nil}), "lathe:go", false},
		{"大小写不敏感", ev("create", []string{"Lathe:GO"}, "", nil), "lathe:go", true},
		{"两侧空白容错", ev("create", []string{" lathe:go "}, "", nil), "lathe:go", true},
		{"非 Issue 类型", &WebhookEvent{Action: "create", Type: "Comment"}, "lathe:go", false},
		{"nil 事件", nil, "lathe:go", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.IsLabelTriggered(c.label); got != c.want {
				t.Errorf("IsLabelTriggered(%q) = %v，期望 %v", c.label, got, c.want)
			}
		})
	}
}

func TestIsCancelled(t *testing.T) {
	cases := []struct {
		name string
		e    *WebhookEvent
		want bool
	}{
		{"改成 canceled", ev("update", nil, "canceled", map[string]any{"stateId": "old"}), true},
		{"大小写不敏感", ev("update", nil, "Canceled", map[string]any{"stateId": "old"}), true},
		{"issue 被删", ev("remove", nil, "", nil), true},
		// completed 刻意不算取消：人手工标完成不代表平台任务该作废 ——
		// 那个任务可能正在跑，或已开出 PR 等人合并
		{"completed 不算取消", ev("update", nil, "completed", map[string]any{"stateId": "old"}), false},
		{"started 不算", ev("update", nil, "started", map[string]any{"stateId": "old"}), false},
		// 停在取消态时改标题不该重复触发
		{"取消态下改别的字段", ev("update", nil, "canceled", map[string]any{"title": "x"}), false},
		{"无 updatedFrom", ev("update", nil, "canceled", nil), false},
		{"create 不算", ev("create", nil, "canceled", nil), false},
		{"无 state", ev("update", nil, "", map[string]any{"stateId": "old"}), false},
		{"非 Issue 类型", &WebhookEvent{Action: "update", Type: "Comment"}, false},
		{"nil 事件", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.IsCancelled(); got != c.want {
				t.Errorf("IsCancelled() = %v，期望 %v", got, c.want)
			}
		})
	}
}

// labelIds 与 labels 两种形态并存时按 name 判定 —— 人在界面上打的是名字。
func TestLabelMatchingUsesNameNotID(t *testing.T) {
	e := ev("create", []string{"lathe:go"}, "", nil)
	e.Data.LabelIDs = []string{"some-uuid"}
	if !e.IsLabelTriggered("lathe:go") {
		t.Error("应按标签 name 匹配")
	}
	if e.IsLabelTriggered("some-uuid") {
		t.Error("不该拿 labelIds 里的 UUID 当标签名匹配")
	}
}

// TestIsLabelTriggeredIgnoresWhitespaceOnlyLabel 钉住「关闭」这条语义的纵深防御。
//
// IsLabelTriggered 判的是 label == ""，纯空白的 " " 穿得过去；若 hasLabel
// 不自己兜一道，want 会变成空串并匹配上任何**空名**标签 —— 在一个没配置
// 标签的部署上悄悄打开自动接单。生产上靠上游两道 TrimSpace 够不到这里，
// 这条测试保的是「以后有人新加一个忘了 trim 的入口」。
func TestIsLabelTriggeredIgnoresWhitespaceOnlyLabel(t *testing.T) {
	// issue 上带着空名与纯空白名的标签（Linear 不该产生这种数据，
	// 但判定逻辑不能依赖对端的自律）。
	e := ev("create", []string{"", "   "}, "", nil)
	for _, label := range []string{" ", "   ", "\t", "\n"} {
		if e.IsLabelTriggered(label) {
			t.Errorf("标签配成纯空白 %q 时不该接单（等同未配置）", label)
		}
	}

	// 反向：正常标签仍然要能命中，别把防御做成一刀切。
	ok := ev("create", []string{"lathe:go"}, "", nil)
	if !ok.IsLabelTriggered(" lathe:go ") {
		t.Error("正常标签（配置值两侧带空白）仍应命中")
	}
}

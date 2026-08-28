package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

// F7.1-AC4：空、"{}"、"null" 都返回零值 Profile，不报错——未设画像的
// 节点行为必须与画像功能加入之前完全一致。
func TestParseProfile_EmptyReturnsZeroValue(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("{}"), []byte("null"), []byte("  {}  "), []byte("  null  ")} {
		p, err := ParseProfile(raw)
		if err != nil {
			t.Fatalf("ParseProfile(%q) 返回错误: %v", raw, err)
		}
		if p == nil {
			t.Fatalf("ParseProfile(%q) 返回 nil profile", raw)
		}
		if p.ModelChannel != "" || p.VerifyTier != "" || len(p.Skills) != 0 {
			t.Errorf("ParseProfile(%q) = %+v，期望零值", raw, *p)
		}
	}
}

func TestParseProfile_ValidJSON(t *testing.T) {
	raw := []byte(`{"model_channel":"channel-x","verify_tier":"heavy","skills":[{"name":"go-testing","version":"1.0"}]}`)
	p, err := ParseProfile(raw)
	if err != nil {
		t.Fatalf("ParseProfile 失败: %v", err)
	}
	if p.ModelChannel != "channel-x" {
		t.Errorf("ModelChannel = %q，期望 channel-x", p.ModelChannel)
	}
	if p.VerifyTier != "heavy" {
		t.Errorf("VerifyTier = %q，期望 heavy", p.VerifyTier)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "go-testing" || p.Skills[0].Version != "1.0" {
		t.Errorf("Skills = %+v，期望 [{go-testing 1.0}]", p.Skills)
	}
}

func TestParseProfile_InvalidVerifyTier(t *testing.T) {
	_, err := ParseProfile([]byte(`{"verify_tier":"medium"}`))
	if err == nil {
		t.Fatal("非法 verify_tier 应报错，得到 nil")
	}
	if !strings.Contains(err.Error(), "verify_tier") {
		t.Errorf("错误信息应提及 verify_tier，得到: %v", err)
	}
}

func TestParseProfile_CorruptJSON(t *testing.T) {
	_, err := ParseProfile([]byte(`{"model_channel":`))
	if err == nil {
		t.Fatal("损坏的 JSON 应报错，得到 nil")
	}
}

// 只设 model_channel、不设 verify_tier 时应该合法（各字段独立可选）。
func TestParseProfile_PartialFieldsOK(t *testing.T) {
	p, err := ParseProfile([]byte(`{"model_channel":"channel-y"}`))
	if err != nil {
		t.Fatalf("ParseProfile 失败: %v", err)
	}
	if p.ModelChannel != "channel-y" {
		t.Errorf("ModelChannel = %q，期望 channel-y", p.ModelChannel)
	}
	if p.VerifyTier != "" {
		t.Errorf("VerifyTier = %q，期望空", p.VerifyTier)
	}
}

// F7.2-AC4：技能声明按名字 + 版本存储，不存本机目录路径——序列化后
// 恰好是 {"name":"...","version":"..."}，不含任何路径信息。
func TestSkillRef_JSONSchemaIsNameVersionOnly(t *testing.T) {
	b, err := json.Marshal(SkillRef{Name: "go-testing", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	if got, want := string(b), `{"name":"go-testing","version":"1.0.0"}`; got != want {
		t.Errorf("SkillRef 序列化 = %s，期望 %s", got, want)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("SkillRef 序列化后字段数 = %d，期望仅 name/version 两个字段: %v", len(keys), keys)
	}
}

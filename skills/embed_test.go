package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterialize_CopiesFiles 验证正常物化一个示例技能：文件内容与嵌入的
// SKILL.md/references 原样一致（既验证顶层文件也验证子目录里的文件，覆盖
// go:embed 目录递归与 Materialize 的 WalkDir 都没有漏文件）。
func TestMaterialize_CopiesFiles(t *testing.T) {
	dest := t.TempDir()
	dest = filepath.Join(dest, ".claude", "skills", "go-testing")

	if err := Materialize("go-testing", "1.0.0", dest); err != nil {
		t.Fatalf("Materialize 失败: %v", err)
	}

	skillMD, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatalf("读取物化后的 SKILL.md 失败: %v", err)
	}
	want, err := os.ReadFile("go-testing/1.0.0/SKILL.md")
	if err != nil {
		t.Fatalf("读取源 SKILL.md 失败: %v", err)
	}
	if string(skillMD) != string(want) {
		t.Errorf("物化后的 SKILL.md 内容与源不一致\n got: %q\nwant: %q", skillMD, want)
	}

	checklist, err := os.ReadFile(filepath.Join(dest, "references", "checklist.md"))
	if err != nil {
		t.Fatalf("读取物化后的 references/checklist.md 失败: %v", err)
	}
	wantChecklist, err := os.ReadFile("go-testing/1.0.0/references/checklist.md")
	if err != nil {
		t.Fatalf("读取源 checklist.md 失败: %v", err)
	}
	if string(checklist) != string(wantChecklist) {
		t.Errorf("物化后的 checklist.md 内容与源不一致")
	}
}

// TestMaterialize_NotFound 验证技能名或版本任一对不上时返回
// ErrSkillNotFound（F7.2-AC5 的判定依据）。
func TestMaterialize_NotFound(t *testing.T) {
	cases := []struct {
		name, version string
	}{
		{"does-not-exist", "1.0.0"},
		{"go-testing", "9.9.9"}, // 名字对，版本不对
		{"", "1.0.0"},
		{"go-testing", ""},
	}
	for _, c := range cases {
		err := Materialize(c.name, c.version, filepath.Join(t.TempDir(), "dest"))
		if err == nil {
			t.Errorf("Materialize(%q, %q, ...) 应失败，实际成功", c.name, c.version)
			continue
		}
		if !errors.Is(err, ErrSkillNotFound) {
			t.Errorf("Materialize(%q, %q, ...) 错误 = %v，期望 errors.Is(_, ErrSkillNotFound)", c.name, c.version, err)
		}
	}
}

// TestMaterialize_DoesNotDependOnHome 证明 Materialize 不读 HOME：即使
// HOME 指向一个不存在的目录，物化依然正常工作——因为它只读编译期嵌入的
// 内容，物理上碰不到执行机器的用户目录（F7.2-AC2）。
func TestMaterialize_DoesNotDependOnHome(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "does-not-exist-"+t.Name())
	t.Setenv("HOME", bogus)

	dest := filepath.Join(t.TempDir(), ".claude", "skills", "sql-migration")
	if err := Materialize("sql-migration", "1.0.0", dest); err != nil {
		t.Fatalf("HOME 指向不存在目录时 Materialize 仍应成功，实际: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Errorf("物化产物缺失: %v", err)
	}
}

// TestMaterialize_IdempotentOnExistingDest 证明对同一个已存在的目标目录
// 反复调用 Materialize 是安全的：不报错，产出内容不变。这是
// internal/runner.materializeSkills 去掉 fresh/resume 跳过条件、改成
// "每次 stageImplement 都无条件物化"这一修复能够成立的前提——如果
// Materialize 在目标目录已存在时会报错，无条件重跑就会把断点续跑的
// 正常路径也打挂。
func TestMaterialize_IdempotentOnExistingDest(t *testing.T) {
	dest := filepath.Join(t.TempDir(), ".claude", "skills", "go-testing")

	if err := Materialize("go-testing", "1.0.0", dest); err != nil {
		t.Fatalf("第一次 Materialize 失败: %v", err)
	}
	if err := Materialize("go-testing", "1.0.0", dest); err != nil {
		t.Fatalf("对已存在的目标目录重复 Materialize 应该成功，实际: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatalf("读取重复物化后的 SKILL.md 失败: %v", err)
	}
	want, err := os.ReadFile("go-testing/1.0.0/SKILL.md")
	if err != nil {
		t.Fatalf("读取源 SKILL.md 失败: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("重复物化后的内容与源不一致")
	}
}

// TestSourceDoesNotReferenceHome 是静态代码检查式的测试：直接读源码文件，
// 断言 Materialize 的实现里没有任何读 os.UserHomeDir()/os.Getenv("HOME")
// 之类拼接执行机器用户目录的代码路径。这与上面运行时的
// TestMaterialize_DoesNotDependOnHome 互为补充：一个证明"就算 HOME 异常也
// 工作"，这个证明"代码里压根没有读它的分支"，避免只是恰好没触发到那条分支。
func TestSourceDoesNotReferenceHome(t *testing.T) {
	src, err := os.ReadFile("embed.go")
	if err != nil {
		t.Fatalf("读取 embed.go 失败: %v", err)
	}
	forbidden := []string{"UserHomeDir", `Getenv("HOME")`, "os.Getenv(\"HOME\")"}
	for _, f := range forbidden {
		if strings.Contains(string(src), f) {
			t.Errorf("embed.go 不应出现 %q（Materialize 不该依赖执行机器的 HOME/用户目录）", f)
		}
	}
}

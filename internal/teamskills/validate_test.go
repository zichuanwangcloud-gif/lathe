package teamskills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// skillMD 造一份最小的合法 SKILL.md。
func skillMD(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# 正文\n"
}

func okFiles() []File {
	return []File{{Path: "SKILL.md", Content: skillMD("acme-testing", "x")}}
}

// AC-4：slug 规范化与拒绝规则。用例名即断言。
func TestValidateSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 空 = 期望拒绝
	}{
		{"空格与大写规范化成小写连字符", "Acme Testing", "acme-testing"},
		{"首尾空白与连续下划线压成一个连字符", "  Acme__Testing  ", "acme-testing"},
		{"连续连字符拒绝", "a--b", ""},
		{"路径穿越拒绝", "../etc", ""},
		{"斜杠拒绝", "a/b", ""},
		{"点开头拒绝", "./x", ""},
		{"team-前缀拒绝", "team-foo", ""},
		{"产品技能名go-testing拒绝", "go-testing", ""},
		{"产品技能名sql-migration拒绝", "sql-migration", ""},
		{"41字符拒绝", strings.Repeat("a", 41), ""},
		{"40字符通过", strings.Repeat("a", 40), strings.Repeat("a", 40)},
		{"空串拒绝", "", ""},
		{"只有空白拒绝", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeSlug(c.in)
			if c.want == "" {
				if err == nil {
					t.Fatalf("NormalizeSlug(%q) = %q, want 拒绝", c.in, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("NormalizeSlug(%q) = %q, %v; want %q, nil", c.in, got, err, c.want)
			}
		})
	}
}

// AC-4：文件路径逐段校验，防路径穿越的第二道。
func TestValidateFiles_PathTraversal(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{"上跳两级拒绝", "../../etc/passwd", false},
		{"绝对路径拒绝", "/etc/passwd", false},
		{"中途上跳拒绝", "a/../../b", false},
		{"反斜杠拒绝", `a\b`, false},
		{"空段拒绝", "a//b", false},
		{"单独两点拒绝", "..", false},
		{"单独一点段拒绝", "a/./b", false},
		{"深度超过4拒绝", "a/b/c/d/e.md", false},
		{"顶层gitignore是保留路径拒绝", ".gitignore", false},
		{"子目录gitignore允许", "references/.gitignore", true},
		{"普通子目录文件通过", "references/check.md", true},
		{"深度恰为4通过", "a/b/c/d.md", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := append(okFiles(), File{Path: c.path, Content: "x"})
			_, _, err := ValidateFiles(files)
			if gotOK := err == nil; gotOK != c.wantOK {
				t.Fatalf("ValidateFiles(path=%q) err = %v; wantOK %v", c.path, err, c.wantOK)
			}
		})
	}
}

// AC-4：数量、大小、编码、必备文件。
func TestValidateFiles_Limits(t *testing.T) {
	many := okFiles()
	for i := 0; len(many) < 17; i++ {
		many = append(many, File{Path: "f" + string(rune('a'+i)) + ".md", Content: "x"})
	}
	sixteen := many[:16]

	// 总量 262145：SKILL.md + 两个 ≤128KiB 的文件凑出来
	head := okFiles()[0]
	rest := 262145 - len(head.Content)
	half := rest / 2
	overTotal := []File{head,
		{Path: "a.md", Content: strings.Repeat("a", half)},
		{Path: "b.md", Content: strings.Repeat("b", rest-half)},
	}
	exactTotal := []File{head,
		{Path: "a.md", Content: strings.Repeat("a", half)},
		{Path: "b.md", Content: strings.Repeat("b", rest-half-1)},
	}

	cases := []struct {
		name    string
		files   []File
		wantOK  bool
		wantMsg string
	}{
		{"17个文件拒绝", many, false, "16"},
		{"16个文件通过", sixteen, true, ""},
		{"总量262145字节拒绝", overTotal, false, "262144"},
		{"总量262144字节通过", exactTotal, true, ""},
		{"单文件131073字节拒绝", append(okFiles(), File{Path: "big.md", Content: strings.Repeat("x", 131073)}), false, "131072"},
		{"单文件131072字节通过", append(okFiles(), File{Path: "big.md", Content: strings.Repeat("x", 131072)}), true, ""},
		{"非UTF8拒绝", append(okFiles(), File{Path: "bin.md", Content: "\xff\xfe"}), false, "UTF-8"},
		{"缺SKILL.md拒绝", []File{{Path: "README.md", Content: "x"}}, false, "SKILL.md"},
		{"SKILL.md无frontmatter拒绝", []File{{Path: "SKILL.md", Content: "# 只有正文\n"}}, false, "frontmatter"},
		{"SKILL.md无description拒绝", []File{{Path: "SKILL.md", Content: "---\nname: x\n---\n"}}, false, "description"},
		{"重复路径拒绝", []File{okFiles()[0], okFiles()[0]}, false, "重复"},
		{"没有文件拒绝", nil, false, "SKILL.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ValidateFiles(c.files)
			if gotOK := err == nil; gotOK != c.wantOK {
				t.Fatalf("err = %v; wantOK %v", err, c.wantOK)
			}
			if !c.wantOK && !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("错误信息 = %q, want 含 %q", err.Error(), c.wantMsg)
			}
		})
	}
}

// AC-4（r3）：name: 必须在第一段 frontmatter 里且非空。
func TestValidateFiles_SkillMDRequiresName(t *testing.T) {
	cases := []struct {
		name   string
		md     string
		wantOK bool
	}{
		{"没有name行拒绝", "---\ndescription: x\n---\n", false},
		{"name值为空拒绝", "---\nname:\ndescription: x\n---\n", false},
		{"name值只有空白拒绝", "---\nname:   \ndescription: x\n---\n", false},
		{"name只出现在正文拒绝", "---\ndescription: x\n---\nname: acme-testing\n", false},
		{"name与description齐全通过", "---\nname: acme-testing\ndescription: x\n---\n", true},
		{"CRLF换行也认", "---\r\nname: acme-testing\r\ndescription: x\r\n---\r\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ValidateFiles([]File{{Path: "SKILL.md", Content: c.md}})
			if gotOK := err == nil; gotOK != c.wantOK {
				t.Fatalf("err = %v; wantOK %v", err, c.wantOK)
			}
			if !c.wantOK && !strings.Contains(err.Error(), "name") {
				t.Errorf("错误信息 = %q, want 含 name", err.Error())
			}
		})
	}
}

// AC-4（r3）：title/note 按 rune 计数、超限拒绝、不截断（事故 54075f4）。
func TestValidateTextLimits_CountsRunes(t *testing.T) {
	cases := []struct {
		name    string
		fn      func(string) (string, error)
		in      string
		wantOK  bool
		wantMsg string
	}{
		{"title100个汉字通过", ValidateTitle, strings.Repeat("测", 100), true, ""},
		{"title101个汉字拒绝", ValidateTitle, strings.Repeat("测", 101), false, "100"},
		{"title100个ASCII通过", ValidateTitle, strings.Repeat("a", 100), true, ""},
		{"title101个ASCII拒绝", ValidateTitle, strings.Repeat("a", 101), false, "100"},
		{"note200个汉字通过", ValidateNote, strings.Repeat("测", 200), true, ""},
		{"note201个汉字拒绝", ValidateNote, strings.Repeat("测", 201), false, "200"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.fn(c.in)
			if gotOK := err == nil; gotOK != c.wantOK {
				t.Fatalf("err = %v; wantOK %v", err, c.wantOK)
			}
			if c.wantOK && got != c.in {
				t.Errorf("通过的输入被改写了：got %d 字节, want %d 字节（不允许截断）", len(got), len(c.in))
			}
			if !c.wantOK && !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("错误信息 = %q, want 含 %q", err.Error(), c.wantMsg)
			}
		})
	}
}

// AC-4：保留名单必须覆盖 skills/<name>/<version>/ 下的全部产品技能名。
// 将来新增产品技能忘了登记保留名，这条先红。
func TestReservedSlugsCoverAllEmbeddedSkills(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("..", "..", "skills", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			names[filepath.Base(filepath.Dir(d))] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("没扫到任何产品技能目录，路径解析错了")
	}
	reserved := map[string]bool{}
	for _, r := range ReservedSlugs {
		reserved[r] = true
	}
	var missing []string
	for n := range names {
		if !reserved[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("保留名单缺少产品技能 %v（got ReservedSlugs=%v）", missing, ReservedSlugs)
	}
}

// AC-4：真实形态的产品技能不会被校验误杀。
func TestValidateFiles_AcceptsProductShapedSkill(t *testing.T) {
	root := filepath.Join("..", "..", "skills", "go-testing", "1.0.0")
	var files []File
	for _, p := range []string{"SKILL.md", "references/checklist.md"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", p, err)
		}
		files = append(files, File{Path: p, Content: string(b)})
	}
	if _, err := NormalizeSlug("acme-testing"); err != nil {
		t.Fatalf("slug acme-testing 应通过: %v", err)
	}
	got, total, err := ValidateFiles(files)
	if err != nil {
		t.Fatalf("产品形态的技能应通过校验: %v", err)
	}
	if len(got) != 2 || got[0].Path != "SKILL.md" || total <= 0 {
		t.Errorf("got %d 个文件（首个 %q）、total %d；want 2 个按路径排序", len(got), got[0].Path, total)
	}
}

// 摘要按 path 排序，与输入顺序无关；内容变了摘要就变。
func TestContentSHA256_OrderIndependent(t *testing.T) {
	a := []File{{Path: "SKILL.md", Content: "1"}, {Path: "b.md", Content: "2"}}
	b := []File{{Path: "b.md", Content: "2"}, {Path: "SKILL.md", Content: "1"}}
	if ContentSHA256(a) != ContentSHA256(b) {
		t.Error("同一组文件换顺序，摘要应相同")
	}
	c := []File{{Path: "SKILL.md", Content: "1"}, {Path: "b.md", Content: "3"}}
	if ContentSHA256(a) == ContentSHA256(c) {
		t.Error("内容变了摘要应不同")
	}
	// path/content 边界不可混淆
	d := []File{{Path: "SKILL.md", Content: "1b.md"}}
	if ContentSHA256(a) == ContentSHA256(d) {
		t.Error("不同的切分方式不应得到相同摘要")
	}
}

// 物化时只改第一段 frontmatter 的 name 行，其余字节原样。
func TestRewriteName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"只改name一行",
			"---\nname: acme-testing\ndescription: d\nlicense: MIT\n---\n\nname: 正文里的不动\n",
			"---\nname: team-acme-testing\ndescription: d\nlicense: MIT\n---\n\nname: 正文里的不动\n"},
		{"保留CRLF",
			"---\r\nname: x\r\ndescription: d\r\n---\r\n",
			"---\r\nname: team-acme-testing\r\ndescription: d\r\n---\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RewriteName(c.in, "team-acme-testing")
			if err != nil || got != c.want {
				t.Fatalf("RewriteName = %q, %v; want %q", got, err, c.want)
			}
		})
	}
	if _, err := RewriteName("# 没有 frontmatter\n", "team-x"); err == nil {
		t.Error("没有 frontmatter 时应报错")
	}
}

// Package teamskills 是团队自定义技能（f11，docs/features/team-skills.md）的
// 纯结构校验：slug 规范化、文件路径、大小/数量上限、SKILL.md frontmatter、
// 展示文本长度，以及内容摘要与物化时的 name 改写。
//
// 只依赖标准库，不 import 仓内任何包：存储（internal/store）、API
// （internal/httpapi）、物化（internal/runner）三处都用它，反向依赖会成环。
// 凭据扫描**不在这里** —— 它要拿解密后的集成凭据，属于 httpapi 层，扫描器本身
// 在 internal/runner/redact.go（与脱敏共用一张形态表）。
//
// 所有错误信息都是给人看的中文原因，直接作为 400 的 error 返回，不含文件内容。
package teamskills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// 上限是常量而不是配置项（PRD §3.8）：唯一消费方就是保存校验，做成配置换不来
// 任何运维价值，反而要写消费方、登记 README、做非法值启动报错。
const (
	// MaxFiles 是单个版本的文件数上限（含 SKILL.md）。
	MaxFiles = 16
	// MaxVersionBytes 是单个版本全部内容的 UTF-8 字节数上限，与 0063 的
	// total_bytes CHECK 同值。
	MaxVersionBytes = 256 << 10
	// MaxFileBytes 是单个文件的字节数上限：防止一个文件吃掉整个配额。
	MaxFileBytes = 128 << 10
	// MaxPathDepth 是文件路径的最大段数。
	MaxPathDepth = 4
	// MaxSkillsPerRepo 是每个仓库在役技能数上限：每次物化都要落盘。
	MaxSkillsPerRepo = 20
	// MaxTitleRunes / MaxNoteRunes 按 rune 计数（按 byte 计会让 34 个汉字就超限）。
	MaxTitleRunes = 100
	MaxNoteRunes  = 200

	// DirPrefix 是团队技能在 .claude/skills/ 下的目录前缀。slug 不允许以它开头，
	// 所以团队目录名与产品技能目录名的集合结构上不相交。
	DirPrefix = "team-"
	// SkillFile 是必备文件。
	SkillFile = "SKILL.md"
	// SentinelFile 是物化时 Lathe 在每个团队技能目录顶层写的哨兵（内容 "*\n"），
	// 让整个目录对 git 不可见。用户版本里的顶层同名文件因此是保留路径。
	SentinelFile = ".gitignore"
)

// ReservedSlugs 是与产品技能（skills/<name>/<version>/）同名、因而不允许登记的
// slug。目录名不会相撞（团队技能落在 team-<slug>/），但同名会让人以为覆盖了产品
// 技能，直接拒更清楚。TestReservedSlugsCoverAllEmbeddedSkills 守着它 ⊇ 产品技能名。
var ReservedSlugs = []string{"go-testing", "sql-migration"}

// File 是技能版本里的一个文件。Path 以 "/" 分隔、相对技能目录。
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

var (
	// slugRe 是 Claude Code 技能名规则：小写字母数字，以单个 "-" 分隔。
	slugRe       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	slugSepRe    = regexp.MustCompile(`[\s_]+`)
	pathSegRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	maxSlugRunes = 40
)

// NormalizeSlug 把用户输入规范化成 slug：TrimSpace → ToLower → 空白与 "_" 的
// 连续段换成单个 "-" → 去首尾 "-"，再校验字符集、长度、保留名。
//
// 这是防路径穿越的第一道：合法 slug 的字符集里没有 "/"、"."、"\"。
func NormalizeSlug(in string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(in))
	s = slugSepRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "", errors.New("技能名不能为空")
	}
	if len(s) > maxSlugRunes {
		return "", fmt.Errorf("技能名不能超过 %d 个字符", maxSlugRunes)
	}
	if !slugRe.MatchString(s) {
		return "", errors.New("技能名只能包含小写字母、数字和单个连字符（-），且不能以连字符开头或结尾")
	}
	if strings.HasPrefix(s, DirPrefix) {
		return "", fmt.Errorf("技能名不能以 %q 开头：物化目录会自动加上这个前缀", DirPrefix)
	}
	for _, r := range ReservedSlugs {
		if s == r {
			return "", fmt.Errorf("技能名 %q 与 Lathe 内置技能同名，请换一个名字", s)
		}
	}
	return s, nil
}

// ValidateFiles 校验一个版本的全部文件，返回按 path 排序的副本与内容总字节数。
// 输入不被修改（不截断、不改写）。
func ValidateFiles(files []File) ([]File, int, error) {
	if len(files) > MaxFiles {
		return nil, 0, fmt.Errorf("一个技能最多 %d 个文件（含 SKILL.md），当前 %d 个", MaxFiles, len(files))
	}
	seen := map[string]bool{}
	total := 0
	var skill *File
	for i := range files {
		f := files[i]
		if err := validatePath(f.Path); err != nil {
			return nil, 0, err
		}
		if seen[f.Path] {
			return nil, 0, fmt.Errorf("文件路径 %q 重复", f.Path)
		}
		seen[f.Path] = true
		if !utf8.ValidString(f.Content) {
			return nil, 0, fmt.Errorf("文件 %s 不是合法的 UTF-8 文本（技能只能包含文本文件）", f.Path)
		}
		if len(f.Content) > MaxFileBytes {
			return nil, 0, fmt.Errorf("文件 %s 超过单文件上限 %d 字节（当前 %d 字节）", f.Path, MaxFileBytes, len(f.Content))
		}
		total += len(f.Content)
		if f.Path == SkillFile {
			skill = &files[i]
		}
	}
	if total > MaxVersionBytes {
		return nil, 0, fmt.Errorf("技能内容总量超过上限 %d 字节（当前 %d 字节）", MaxVersionBytes, total)
	}
	if skill == nil {
		return nil, 0, errors.New("技能必须包含 SKILL.md")
	}
	if err := checkFrontmatter(skill.Content); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, errors.New("技能内容不能为空")
	}
	out := append([]File(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, total, nil
}

// validatePath 是防路径穿越的第二道：逐段校验，不依赖 filepath.Join 的清理行为。
func validatePath(p string) error {
	if p == "" {
		return errors.New("文件路径不能为空")
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("文件路径 %q 不能包含反斜杠，请用 / 分隔", p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("文件路径 %q 必须是相对路径", p)
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxPathDepth {
		return fmt.Errorf("文件路径 %q 层级过深（最多 %d 层）", p, MaxPathDepth)
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || !pathSegRe.MatchString(s) {
			return fmt.Errorf("文件路径 %q 不合法：每一段只能包含字母、数字、点、下划线和连字符，且不能是「.」或「..」", p)
		}
	}
	if p == SentinelFile {
		return fmt.Errorf("顶层 %s 是保留路径（物化时由 Lathe 写入，让技能目录不进 PR），请放到子目录里", SentinelFile)
	}
	return nil
}

// frontmatterLines 返回 SKILL.md 第一段 frontmatter 的行（不含两条 --- 分隔线），
// 以及每行在原文里的起止字节区间。首行不是 --- 或找不到结束行时 ok=false。
func frontmatterLines(md string) (lines []string, spans [][2]int, ok bool) {
	pos := 0
	first := true
	for pos < len(md) {
		end := strings.IndexByte(md[pos:], '\n')
		var line string
		next := len(md)
		if end >= 0 {
			line = md[pos : pos+end]
			next = pos + end + 1
		} else {
			line = md[pos:]
		}
		bare := strings.TrimSuffix(line, "\r")
		if first {
			if bare != "---" {
				return nil, nil, false
			}
			first = false
		} else {
			if bare == "---" {
				return lines, spans, true
			}
			lines = append(lines, bare)
			spans = append(spans, [2]int{pos, pos + len(bare)})
		}
		pos = next
	}
	return nil, nil, false
}

// fieldValue 在 frontmatter 行里找顶层 key: value（不缩进），返回值与所在行号。
func fieldValue(lines []string, key string) (string, int) {
	for i, l := range lines {
		if strings.HasPrefix(l, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(l, key+":")), i
		}
	}
	return "", -1
}

func checkFrontmatter(md string) error {
	lines, _, ok := frontmatterLines(md)
	if !ok {
		return errors.New("SKILL.md 必须以 --- 开头的 frontmatter 起首，并以 --- 结束")
	}
	if v, i := fieldValue(lines, "name"); i < 0 || v == "" {
		return errors.New("SKILL.md 的 frontmatter 必须包含非空的 name:（物化时会改写成 team-<技能名>）")
	}
	if v, i := fieldValue(lines, "description"); i < 0 || v == "" {
		return errors.New("SKILL.md 的 frontmatter 必须包含非空的 description: 字段")
	}
	return nil
}

// RewriteName 把第一段 frontmatter 里的 name: 行改写成 name: <name>，其余字节
// 原样保留（含 CRLF）。只在物化落盘时调用，库里与界面上的内容是未改写的原样。
func RewriteName(md, name string) (string, error) {
	lines, spans, ok := frontmatterLines(md)
	if !ok {
		return "", errors.New("SKILL.md 缺少 frontmatter，无法改写 name")
	}
	_, i := fieldValue(lines, "name")
	if i < 0 {
		return "", errors.New("SKILL.md 的 frontmatter 缺少 name:，无法改写")
	}
	sp := spans[i]
	return md[:sp[0]] + "name: " + name + md[sp[1]:], nil
}

// ValidateTitle 校验展示名：≤ MaxTitleRunes 个字符，超限拒绝、不截断。
func ValidateTitle(s string) (string, error) {
	if n := utf8.RuneCountInString(s); n > MaxTitleRunes {
		return "", fmt.Errorf("展示名不能超过 %d 个字符（当前 %d 个）", MaxTitleRunes, n)
	}
	return s, nil
}

// ValidateNote 校验版本说明：≤ MaxNoteRunes 个字符，超限拒绝、不截断。
func ValidateNote(s string) (string, error) {
	if n := utf8.RuneCountInString(s); n > MaxNoteRunes {
		return "", fmt.Errorf("版本说明不能超过 %d 个字符（当前 %d 个）", MaxNoteRunes, n)
	}
	return s, nil
}

// ContentSHA256 是按 path 排序后 path\0content\0 串联的 SHA-256（十六进制）。
// 用于「内容没变就不产生新版本」与审计里记录「用的是哪一份内容」。
func ContentSHA256(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(f.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

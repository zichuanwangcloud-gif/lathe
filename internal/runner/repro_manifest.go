package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReproManifestPath 是复现声明在工作区里的固定路径（§5.3 输出契约的一部分）。
const ReproManifestPath = ".lathe/repro.json"

// ErrReproManifest 表示 agent 提交了复现声明但声明不合法：JSON 解析失败、
// 引用了不在 diff 里的文件、命令为空、路径越界等。
//
// 它与 ErrNoReproTests 同属【契约违例】：agent 有能力就地修掉（改对
// 这个 JSON 即可），流水线据此进修复回路 —— 而不是转 blocked_spec
// 问提单人，也不是直接判任务失败。
var ErrReproManifest = errors.New("runner: " + ReproManifestPath + " 复现声明不合法")

// ReproManifest 是 agent 随改动提交的复现测试声明。
//
// 存在理由：验证器对「怎么跑一条测试」的启发式猜测建立在单包布局假设上，
// monorepo 子包、非 go/vitest/jest 框架都会猜空（任务 #596：测试明明在
// diff 里，流水线却报"没有测试文件"，修复回路空烧两轮）。猜测失败被误记
// 在 agent 头上，而 agent 对流水线的识别逻辑无能为力。声明把这份知识交还
// 给唯一拥有它的人 —— 写测试的 agent。从 diff 里读一个 JSON 与读文件清单
// 同样是确定性输入，不违反「不解析自然语言输出」的设计原则。
type ReproManifest struct {
	Version int                 `json:"version"`
	Tests   []ReproManifestTest `json:"tests"`
}

// ReproManifestTest 声明一条复现测试的运行方式。
type ReproManifestTest struct {
	// File 是测试文件相对工作区根的路径，必须出现在本次 diff 里
	//（与启发式同一条规矩：既有测试不算复现证据）。
	File string `json:"file"`
	// Cmd 是运行该测试的命令（argv 形式，不经 shell），流水线原样执行。
	Cmd []string `json:"cmd"`
	// Dir 是命令的工作目录（相对工作区根）；空表示根目录。
	Dir string `json:"dir,omitempty"`
}

// ResolveReproTests 决定本次验证的复现测试清单：声明优先于猜测。
//
// agent 提交了声明就严格按声明来 —— 声明不合法是契约违例，不悄悄回落
// 启发式（否则 agent 永远不知道自己的声明写错了）；没提交声明才走
// IdentifyReproTests 的启发式兜底。
//
// 返回的 error 一律按契约违例处理（agent 可修），由 RunHeavy 放进红阶段
// 结果里走三分路由，不当流水线执行错误直接判失败。
func ResolveReproTests(root string, changedFiles []string) ([]ReproTest, error) {
	m, ok, err := loadReproManifest(root)
	if err != nil {
		return nil, err
	}
	if ok {
		return m.resolve(changedFiles)
	}
	return IdentifyReproTests(root, changedFiles)
}

// loadReproManifest 读取工作区里的复现声明；文件不存在返回 (nil, false, nil)，
// 存在但读不了/解析不了返回契约违例错误。
func loadReproManifest(root string) (*ReproManifest, bool, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ReproManifestPath)))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: 读取失败: %v", ErrReproManifest, err)
	}
	var m ReproManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, fmt.Errorf("%w: JSON 解析失败: %v", ErrReproManifest, err)
	}
	return &m, true, nil
}

// resolve 校验声明并转成可执行的复现测试清单。changed 是本次 diff 的
// 文件清单（根相对路径）。
func (m *ReproManifest) resolve(changed []string) ([]ReproTest, error) {
	if m.Version > 1 {
		return nil, fmt.Errorf("%w: version %d 不受支持（当前为 1）", ErrReproManifest, m.Version)
	}
	if len(m.Tests) == 0 {
		return nil, fmt.Errorf("%w: tests 为空 —— 至少要声明一条复现测试", ErrReproManifest)
	}

	inDiff := make(map[string]bool, len(changed))
	for _, f := range changed {
		inDiff[filepath.ToSlash(f)] = true
	}

	out := make([]ReproTest, 0, len(m.Tests))
	for i, t := range m.Tests {
		where := fmt.Sprintf("tests[%d]", i)

		file := filepath.ToSlash(strings.TrimSpace(t.File))
		switch {
		case file == "":
			return nil, fmt.Errorf("%w: %s 缺少 file", ErrReproManifest, where)
		case pathEscapes(file):
			return nil, fmt.Errorf("%w: %s 的 file %q 越出了工作区", ErrReproManifest, where, t.File)
		case !inDiff[file]:
			return nil, fmt.Errorf(
				"%w: %s 的 file %q 不在本次改动的文件清单里 —— 复现证据必须随改动一起提交（§5.3）",
				ErrReproManifest, where, file)
		}

		if len(t.Cmd) == 0 || strings.TrimSpace(t.Cmd[0]) == "" {
			return nil, fmt.Errorf("%w: %s 缺少可执行命令 cmd（argv 形式）", ErrReproManifest, where)
		}

		dir := filepath.ToSlash(strings.TrimSpace(t.Dir))
		if pathEscapes(dir) {
			return nil, fmt.Errorf("%w: %s 的 dir %q 越出了工作区", ErrReproManifest, where, t.Dir)
		}

		out = append(out, ReproTest{File: file, Cmd: t.Cmd, Dir: dir})
	}
	return out, nil
}

// pathEscapes 报告相对路径是否越出工作区根（绝对路径或含 .. 上跳）。
func pathEscapes(p string) bool {
	return strings.HasPrefix(p, "/") || p == ".." ||
		strings.HasPrefix(p, "../") || strings.Contains(p, "/../")
}

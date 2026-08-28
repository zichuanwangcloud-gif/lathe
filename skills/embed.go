// Package skills 把 Lathe 自己维护的技能定义（skills/<name>/<version>/）编进
// 二进制，供节点执行画像（internal/runner.Profile.Skills，F7.1）声明的技能
// 物化到 worktree。
//
// 抄 internal/webui/webui.go 顶部 "//go:embed all:dist" 的写法：整棵目录树
// 在编译期打进二进制，运行时不再需要这些源文件存在于磁盘上。
//
// 放在仓库根（与 internal/、docs/、migrations/ 同级）而不是 internal/skills/
// 下，是 go:embed 本身的限制逼出来的：embed 模式相对当前包目录解析，且不允许
// 出现 ".." 路径元素（见 https://pkg.go.dev/embed 的 Patterns 一节），所以
// 携带 //go:embed 指令的源文件必须与被嵌入的目录树同处一棵子树——放进
// internal/skills/ 就意味着技能内容也要搬进 internal/skills/ 内部，与
// docs/06-orchestration.md §6 "skills 存 Lathe 自己的 git 仓库，可 review、
// 可版本化" 的意图相悖（技能定义应该在仓库里独立、显眼地存在，不是深埋在某个
// Go 包的实现细节里）。因此把嵌入逻辑本身也放在 skills/ 目录下，而不是套一层
// internal/skills 再想办法把内容"借"过来。
//
// F7.2-AC2（不从执行机器继承 ~/.claude/skills）由此天然满足：Materialize
// 只读 embedded（编译期打进二进制的内容），物理上碰不到执行机器的用户主
// 目录或 HOME 环境变量指向的任何路径——见 embed_test.go 的
// TestMaterialize_DoesNotDependOnHome 与 TestSourceDoesNotReferenceHome。
package skills

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// embedded 是 skills/<name>/<version>/ 的整棵子树。
//
// 模式写成 all:*/*（而不是 all:.）：*/* 恰好匹配"技能名/版本号"两级目录，
// 天然把同目录下的 embed.go 自己、_test.go 排除在外，不需要额外的忽略规则。
// all: 前缀确保版本目录里以 "." 或 "_" 开头的文件（如果将来哪个技能真的需要）
// 也不会被静默丢弃——这与 webui.go 用 all: 的理由一致。
//
//go:embed all:*/*
var embedded embed.FS

// ErrSkillNotFound 是技能名 + 版本在嵌入内容里找不到时的哨兵错误。
// 调用方（pipeline.go 的 stageImplement）用 errors.Is 判断，据此把任务判
// 定为 F7.2-AC5 要求的"可读原因失败，不静默忽略"。
var ErrSkillNotFound = errors.New("skills: 技能不存在")

// Materialize 把嵌入的 skills/<name>/<version>/ 整个目录树复制到 destDir。
//
// destDir 事先不需要存在，会按需创建（含多级父目录）。name/version 任一
// 对不上嵌入内容时返回可用 errors.Is(err, ErrSkillNotFound) 判断的错误，
// 错误信息里点名具体是哪个技能名 + 版本号找不到，不吞掉细节。
func Materialize(name, version, destDir string) error {
	if name == "" || version == "" {
		return fmt.Errorf("%w: 技能名或版本号为空（name=%q version=%q）", ErrSkillNotFound, name, version)
	}

	// embed.FS 的路径永远用正斜杠，与运行系统的路径分隔符无关（哪怕在
	// Windows 上编译运行）——用 path.Join 而不是 filepath.Join 构造它。
	srcRoot := path.Join(name, version)

	info, err := fs.Stat(embedded, srcRoot)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s@%s", ErrSkillNotFound, name, version)
	}

	return fs.WalkDir(embedded, srcRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(filepath.FromSlash(srcRoot), filepath.FromSlash(p))
		if err != nil {
			return fmt.Errorf("计算 %s 相对路径失败: %w", p, err)
		}
		target := filepath.Join(destDir, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(embedded, p)
		if err != nil {
			return fmt.Errorf("读取嵌入文件 %s 失败: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", target, err)
		}
		return nil
	})
}

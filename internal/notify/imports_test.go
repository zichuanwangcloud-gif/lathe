package notify

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// AC-31：internal/notify 是叶子包，只 import 标准库。
//
// 成环由编译器挡；这条挡的是「notify 开始依赖别的业务包」这一步 ——
// 一旦它能拿到 store/runner，「怎么发」的知识就会漏进状态机旁边。
func TestNotifyImportsStdlibOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s: %v", f, err)
		}
		checked++
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/zichuanwangcloud-gif/lathe/") || strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
				bad = append(bad, f+": "+p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个非测试文件都没读到")
	}
	if len(bad) > 0 {
		t.Errorf("notify 只能 import 标准库，got 违规 import %v, want 无", bad)
	}
}

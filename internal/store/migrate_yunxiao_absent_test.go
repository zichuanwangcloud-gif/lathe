//go:build migcheck

package store

import (
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// AC-16：down×3 之后用 -tags migcheck 显式跑。用构建标签而不是环境变量
// 切换，免得为测试新增配置项（铁律 3）。不进 make test。
func TestYunxiaoSchemaAbsent(t *testing.T) {
	assertYunxiaoSchema(t, testsupport.Pool(t), false)
}

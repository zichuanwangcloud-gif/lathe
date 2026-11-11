package runner

import (
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
)

// 改动前的占位钩子：插话机制尚不存在，装配是空操作。改动后由实现替换。
func wireInterjectionsForBaseline(t *testing.T, p *Pipeline)                             {}
func assertTriageNotStreaming(t *testing.T, p agent.RunParams)                           {}
func interjectDuringImplement(t *testing.T, p *Pipeline, ag *fakeAgent, id int64, n int) {}
func supportsInterjectionsForVerifyCompare() bool                                        { return false }

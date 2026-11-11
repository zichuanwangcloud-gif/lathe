package agent

// streamInputIdleCases 是 AC-4b 的 B 类子用例（StreamInput=true 下同样输入同样结论）。
// 改动前 RunParams 没有 StreamInput 字段，先返回空集。
func streamInputIdleCases() []struct {
	name string
	p    RunParams
} {
	return nil
}

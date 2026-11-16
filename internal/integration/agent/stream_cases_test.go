package agent

// streamInputIdleCases 是 AC-4b 的 B 类子用例：StreamInput=true 下同样输入同样结论。
func streamInputIdleCases() []struct {
	name string
	p    RunParams
} {
	return []struct {
		name string
		p    RunParams
	}{
		{"流式输入模式", RunParams{Prompt: "x", SessionID: "s", StreamInput: true}},
	}
}

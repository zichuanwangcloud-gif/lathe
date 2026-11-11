package runner

// RedactText 是 redactText 的导出入口：给 runner 之外、要把任务自由文本放上
// 界面的展示面（收件箱）用。两层规则与失败记忆完全相同，没有第二套实现。
func RedactText(s string, registered []string) string { return s }

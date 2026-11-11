package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
)

// verification_log.go —— verifications.log_ref 的第一个读取方
// （docs/features/run-replay.md §3.2.3）：按块读取完整验证日志，经过脱敏。
//
// 用户输入只有 vid、part、offset、limit 四个整数；路径只取自库里的 log_ref，
// 并且必须落在 <LogDir>/verify-logs/task-<id>/ 之下（含符号链接解析后）。

const (
	logChunkDefault = 64 << 10
	logChunkMax     = 256 << 10
	// logLineMax 是块终点向后找安全切点的最远距离（「单行上限」）。超过它仍找不到
	// 安全切点就整段不返回（fail-closed），不冒半截凭据的风险。
	logLineMax = 1 << 20
	// redactContextMin 是安全切点检验的最小上下文窗口：切点两侧各取这么多字节。
	redactContextMin = 16 << 10
	// maxCutProbes 是每次请求最多检验的候选切点数（有界开销）。
	maxCutProbes = 256
	// logMtimeSkew 吸收 DB now() 与宿主机文件 mtime 的时钟偏差（覆盖判定规则 2）。
	logMtimeSkew = 5 * time.Second
)

// redactContext 返回安全切点检验的窗口 W = max(16KiB, 2 × 最长登记凭据长度)，
// 保证任何登记值与它两侧的形态上下文都落在窗口里。
func redactContext(known []string) int64 {
	w := int64(redactContextMin)
	for _, s := range known {
		if n := 2 * int64(len(s)); n > w {
			w = n
		}
	}
	return w
}

// cutter 在一段已读入内存的窗口上判定「安全切点」。
//
// 安全切点 c 的定义（docs/features/run-replay.md §3.2.3）：c 是文件头/尾，或
// 同时满足 (i) 紧跟在 \n 之后；(ii) 取 L = data[c-W:c]、R = data[c:c+W]，
// RedactForDisplay(L+R) == RedactForDisplay(L) + RedactForDisplay(R)。
// 即「在这里切开、两边各自脱敏，结果与不切完全相同」—— 按定义检验切分无害，
// 不枚举哪些规则需要上下文；redact.go 以后加规则，这条判定自动跟上。
type cutter struct {
	buf   []byte // 文件 [base, base+len(buf)) 这一段
	base  int64
	size  int64 // 文件总长
	w     int64
	known []string
}

func (c *cutter) at(pos int64) byte { return c.buf[pos-c.base] }

func (c *cutter) slice(from, to int64) string {
	if from < c.base {
		from = c.base
	}
	if hi := c.base + int64(len(c.buf)); to > hi {
		to = hi
	}
	return string(c.buf[from-c.base : to-c.base])
}

func (c *cutter) safe(pos int64) bool {
	if pos <= 0 || pos >= c.size {
		return true
	}
	if c.at(pos-1) != '\n' {
		return false
	}
	l := c.slice(pos-c.w, pos)
	r := c.slice(pos, pos+c.w)
	return runner.RedactForDisplay(l+r, c.known) == runner.RedactForDisplay(l, c.known)+runner.RedactForDisplay(r, c.known)
}

// nextNewlineCut 返回 >= from 的第一个「紧跟在 \n 之后」的位置；越过 limit
// 或窗口末尾返回 -1；到达文件末尾返回 size（文件尾天然是安全切点）。
func (c *cutter) nextNewlineCut(from, limit int64) int64 {
	if from < 1 {
		from = 1
	}
	hi := c.base + int64(len(c.buf))
	for pos := from; pos <= limit; pos++ {
		if pos >= c.size {
			return c.size
		}
		if pos > hi {
			return -1
		}
		if c.at(pos-1) == '\n' {
			return pos
		}
	}
	return -1
}

// findEnd 从标称终点 e 起向后找第一个安全切点：先试 e 之后的第一个换行，
// 不安全时下一个候选跳到「上一个候选 + W/4」之后的第一个换行；最多检验
// maxCutProbes 个候选，且不超过 maxEnd。找不到返回 -1。
func (c *cutter) findEnd(e, maxEnd int64) int64 {
	from := e
	for probe := 0; probe < maxCutProbes; probe++ {
		cand := c.nextNewlineCut(from, maxEnd)
		if cand < 0 {
			return -1
		}
		if c.safe(cand) {
			return cand
		}
		from = cand + c.w/4
	}
	return -1
}

func (a *API) verificationLog(w http.ResponseWriter, r *http.Request) {
	tk, ok := a.ownedTask(w, r)
	if !ok {
		return
	}
	if a.LogDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "验证日志未启用"})
		return
	}
	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "日志不存在"})
	}
	bad := func(msg string) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
	}

	vid, err := strconv.ParseInt(r.PathValue("vid"), 10, 64)
	if err != nil || vid <= 0 {
		notFound()
		return
	}
	q := r.URL.Query()
	intParam := func(name string, def int64) (int64, bool) {
		if !q.Has(name) {
			return def, true
		}
		v, err := strconv.ParseInt(q.Get(name), 10, 64)
		if err != nil || v < 0 {
			bad(name + " 参数须为非负整数")
			return 0, false
		}
		return v, true
	}
	part, ok := intParam("part", 0)
	if !ok {
		return
	}
	offset, ok := intParam("offset", 0)
	if !ok {
		return
	}
	limit, ok := intParam("limit", logChunkDefault)
	if !ok {
		return
	}
	if limit <= 0 {
		limit = logChunkDefault
	}
	if limit > logChunkMax {
		limit = logChunkMax
	}

	ref, vAt, found, err := a.Store.VerificationLogRef(r.Context(), tk.ID, vid)
	if err != nil {
		serverError(w, "查询验证日志失败", err)
		return
	}
	if !found || ref == nil {
		notFound()
		return
	}
	parts := strings.Fields(*ref)
	if part >= int64(len(parts)) {
		notFound()
		return
	}
	path, ok := resolveLogPath(a.LogDir, tk.ID, parts[part])
	if !ok {
		slog.Warn("验证日志路径校验未通过", "task", tk.ID, "vid", vid)
		notFound()
		return
	}

	f, err := os.Open(path)
	if err != nil {
		notFound()
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		notFound()
		return
	}
	// 重试覆盖判定（铁律 1：证据不挂错运行）。重试复用任务行，新一次运行把日志
	// 写回同一个 round-<n>/NN-<step>.log（O_TRUNC），旧行还在 —— 拿旧 vid 读到的
	// 会是后一次运行的内容。任一命中即判已覆盖，返回 404 log_overwritten：
	//   1. 库内：有 id 更大的行声明了同一段路径（确定性，不依赖时钟）；
	//   2. 时间：文件 ModTime 晚于本行 at + 5s（后一次运行正在写、还没插行）——
	//      本行自己的文件在插行前已关闭，ModTime ≤ at；偏差超限只会 fail-closed；
	//   3. 读后复核：读完再 Stat，size/ModTime 变了，本次内容丢弃。
	// 这条 404 只在 ownedTask 与 task_id 都通过后出现，对非属主不暴露任何信息。
	overwritten := func() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "日志已被后一次运行覆盖", "code": "log_overwritten"})
	}
	later, err := a.Store.VerificationLogClaimedLater(r.Context(), tk.ID, vid, parts[part])
	if err != nil {
		serverError(w, "查询验证日志失败", err)
		return
	}
	if later || st.ModTime().After(vAt.Add(logMtimeSkew)) {
		overwritten()
		return
	}

	size := st.Size()
	if offset > size {
		bad("offset 超出日志长度")
		return
	}

	// 只读 [offset-W, offset+limit+logLineMax+W] 这一段（有界），不整文件进内存。
	win := redactContext(a.KnownSecrets)
	maxEnd := offset + limit + logLineMax
	winFrom := max(offset-win, 0)
	winTo := min(maxEnd+win, size)
	buf := make([]byte, winTo-winFrom)
	if _, err := io.ReadFull(io.NewSectionReader(f, winFrom, winTo-winFrom), buf); err != nil {
		serverError(w, "读取验证日志失败", err)
		return
	}
	if st2, err := f.Stat(); err != nil || st2.Size() != size || !st2.ModTime().Equal(st.ModTime()) {
		overwritten()
		return
	}
	cut := &cutter{buf: buf, base: winFrom, size: size, w: win, known: a.KnownSecrets}

	// offset 必须是安全切点：前端只传 0 或上一块的 nextOffset；手工构造的
	// offset 不许从凭据中间开读（属主本人也不能在界面上看到明文）。
	if !cut.safe(offset) {
		bad("offset 不在块边界上")
		return
	}

	end := offset + limit
	if end >= size {
		end = size
	} else {
		end = cut.findEnd(end, maxEnd)
	}
	if end < 0 {
		// 找不到安全切点（超长单行，或 1MiB 内全是不安全的切点）：这一段
		// 一个字节都不返回。也不跳过继续读 —— 跳过后的起点同样不是安全切点。
		writeJSON(w, http.StatusOK, map[string]any{
			"text": "", "offset": offset, "nextOffset": nil, "size": size,
			"eof": false, "withheld": true, "withheldBytes": size - offset,
			"parts": len(parts), "part": part,
		})
		return
	}

	var next any
	if end < size {
		next = end
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text":       runner.RedactForDisplay(cut.slice(offset, end), a.KnownSecrets),
		"offset":     offset,
		"nextOffset": next,
		"size":       size,
		"eof":        end >= size,
		"withheld":   false,
		"parts":      len(parts),
		"part":       part,
	})
}

// resolveLogPath 把库里的一段 log_ref 解析成磁盘路径，四道校验任一不过返回 false：
//  1. Clean 后必须是相对路径；
//  2. 必须以 verify-logs/task-<id>/ 开头（<id> 已通过归属校验）；
//  3. 解析符号链接后仍位于 <LogDir>/verify-logs/task-<id>/ 之下；
//  4. 必须是常规文件。
func resolveLogPath(root string, taskID int64, ref string) (string, bool) {
	clean := filepath.Clean(ref)
	if filepath.IsAbs(clean) {
		return "", false
	}
	prefix := filepath.Join("verify-logs", fmt.Sprintf("task-%d", taskID)) + string(filepath.Separator)
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	full, err := filepath.EvalSymlinks(filepath.Join(rootReal, clean))
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(full, filepath.Join(rootReal, prefix)+string(filepath.Separator)) {
		return "", false
	}
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return full, true
}

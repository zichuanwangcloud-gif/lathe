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
	"unicode/utf8"

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
)

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

	ref, _, found, err := a.Store.VerificationLogRef(r.Context(), tk.ID, vid)
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
	size := st.Size()
	if offset > size {
		bad("offset 超出日志长度")
		return
	}

	end := offset + limit
	if end >= size {
		end = size
	}
	buf := make([]byte, end-offset)
	if _, err := io.ReadFull(io.NewSectionReader(f, offset, end-offset), buf); err != nil {
		serverError(w, "读取验证日志失败", err)
		return
	}
	// 回退到 rune 边界，保证返回合法 UTF-8
	for end < size && len(buf) > 0 && !utf8.Valid(buf) {
		buf = buf[:len(buf)-1]
		end--
	}

	var next any
	if end < size {
		next = end
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text":       runner.RedactForDisplay(string(buf), a.KnownSecrets),
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

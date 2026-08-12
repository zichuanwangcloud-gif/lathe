// Package webui 把构建好的管理界面打进二进制。
//
// 这样整个产品的交付物仍是单个文件（见 docs/03-tech-stack.md §3 理由②），
// 新增节点或换机器部署都不必额外分发静态资源。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist 是 web/dist 的构建产物。
//
// 用 all: 前缀确保以 _ 或 . 开头的文件也被包含（Vite 的产物名不会，
// 但显式声明可避免将来换构建工具时静默丢文件）。
//
//go:embed all:dist
var dist embed.FS

// Available 报告界面是否已构建进二进制。
//
// 未构建时（只跑了 go build 没跑 pnpm build）返回 false，
// 调用方据此给出可操作的提示，而不是回一个空白页。
func Available() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler 返回服务 SPA 的处理器。
//
// 单页应用的路由在前端，因此任何未命中静态文件的路径都回 index.html，
// 否则刷新 /tasks/12 会 404。
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "管理界面未构建：请先执行 make ui", http.StatusNotImplemented)
		})
	}

	files := http.FS(sub)
	fileServer := http.FileServer(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api 与 /webhooks 由各自的处理器负责，不该走到这里；
		// 万一路由配置有误，明确 404 而不是回一个 HTML 让前端困惑
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			http.NotFound(w, r)
			return
		}

		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, r, files)
			return
		}

		f, err := files.Open(p)
		if err != nil {
			// 未命中静态文件 —— 交给前端路由
			serveIndex(w, r, files)
			return
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil && st.IsDir() {
			serveIndex(w, r, files)
			return
		}

		// 带内容哈希的资源可长期缓存；index.html 不缓存，保证发版后立即生效
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, files http.FileSystem) {
	f, err := files.Open("index.html")
	if err != nil {
		http.Error(w, "管理界面未构建：请先执行 make ui", http.StatusNotImplemented)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, "读取界面资源失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", st.ModTime(), f.(interface {
		Seek(int64, int) (int64, error)
		Read([]byte) (int, error)
	}))
}

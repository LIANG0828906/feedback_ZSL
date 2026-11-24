package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var webFS embed.FS

func (m *Manager) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// 静态资源（app.js 等）。
		sub, err := fs.Sub(webFS, "web")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "界面资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

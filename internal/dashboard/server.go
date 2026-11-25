package dashboard

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed static/index.html
var indexHTML []byte

type Config struct {
	Addr       string
	ReportPath string
	AssetsDir  string
}

func ListenAndServe(cfg Config) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/api/report", func(w http.ResponseWriter, r *http.Request) {
		writeJSONFile(w, cfg.ReportPath)
	})
	mux.HandleFunc("/api/seams", func(w http.ResponseWriter, r *http.Request) {
		writeJSONFile(w, filepath.Join(cfg.AssetsDir, "seams.json"))
	})
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(cfg.AssetsDir))))
	return http.ListenAndServe(cfg.Addr, mux)
}

func writeJSONFile(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/lab"
	"ocrworkbench/internal/webui"
)

type callRequest struct {
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8123", "local lab HTTP address")
	sampleDir := flag.String("samples", "samples", "deterministic sample directory")
	dbPath := flag.String("db", filepath.Join("data", "workbench.db"), "SQLite checkpoint path")
	seed := flag.Int64("seed", domain.DefaultBaseSeed, "sample generator seed")
	flag.Parse()

	service, appErr := lab.New(*sampleDir, *dbPath, *seed, false)
	if appErr != nil {
		log.Fatalf("%s: %s", appErr.Code, appErr.Message)
	}
	defer service.Close()

	mux := http.NewServeMux()
	assets := webui.FS()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": domain.AlgorithmVersion,
		})
	})
	mux.HandleFunc("/api/call", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": map[string]any{"code": domain.CodePara, "message": "POST required"}})
			return
		}
		var req callRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": map[string]any{"code": domain.CodeDecode, "message": "invalid API request", "cause": err.Error()}})
			return
		}
		response := service.Handle(req.Method, req.Payload)
		status := http.StatusOK
		if !response.OK {
			status = http.StatusUnprocessableEntity
			if response.Error != nil && response.Error.Code == domain.CodePara {
				status = http.StatusBadRequest
			}
			if response.Error != nil && response.Error.Code == domain.CodeState {
				status = http.StatusConflict
			}
		}
		writeJSON(w, status, response)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		}
		data, err := fs.ReadFile(assets, trimLeadingSlash(path))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		switch filepath.Ext(path) {
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case ".js":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		case ".svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		case ".html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = w.Write(data)
	})
	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("OCR Workbench Lab listening on http://%s", *addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	fmt.Println("closed")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func trimLeadingSlash(path string) string {
	for len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if path == "" {
		return "index.html"
	}
	return path
}

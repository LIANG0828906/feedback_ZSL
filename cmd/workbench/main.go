package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	fog "volumefog"
)

//go:embed web/index.html
var indexHTML []byte

type server struct {
	wb *fog.Workbench
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "local HTTP listen address")
	dbPath := flag.String("db", "artifacts/workbench/checkpoints.db", "SQLite checkpoint path")
	outDir := flag.String("out", "artifacts/workbench", "sample output directory")
	seed := flag.Int64("seed", fog.DefaultSeed, "deterministic scene seed")
	width := flag.Int("width", 32, "viewport width")
	height := flag.Int("height", 16, "viewport height")
	chunk := flag.Int("chunk", 8, "screen chunk size")
	flag.Parse()

	scene, err := fog.GenerateScene(fog.GenerateOptions{
		Seed: *seed, Width: *width, Height: *height, ChunkSize: *chunk,
	})
	if err != nil {
		log.Fatalf("generate scene: %v", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}
	wb, err := fog.NewWorkbench(scene, *dbPath, *outDir)
	if err != nil {
		log.Fatalf("open workbench: %v", err)
	}
	defer wb.Close()
	if err := wb.SaveReport("scene-summary.json", map[string]any{
		"seed": scene.Seed, "scene_version": scene.SnapshotVersion(),
		"database": *dbPath, "backend": wb.Store.Backend(),
	}); err != nil {
		log.Printf("save summary: %v", err)
	}
	s := &server{wb: wb}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/state", s.state)
	mux.HandleFunc("/api/render/preview", s.renderPreview)
	mux.HandleFunc("/api/render/exact", s.renderExact)
	mux.HandleFunc("/api/render/resume", s.renderResume)
	mux.HandleFunc("/api/edit", s.edit)
	mux.HandleFunc("/api/fault", s.fault)
	mux.HandleFunc("/api/truth", s.truth)
	mux.HandleFunc("/api/compare", s.compare)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backend": wb.Store.Backend()})
	})
	log.Printf("Volume Fog workbench: http://%s", *addr)
	log.Printf("SQLite checkpoint: %s (%s)", *dbPath, wb.Store.Backend())
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.wb.State()})
}

func (s *server) renderPreview(w http.ResponseWriter, r *http.Request) {
	frame, err := s.wb.Preview()
	respondFrame(w, frame, err)
}

func (s *server) renderExact(w http.ResponseWriter, r *http.Request) {
	frame, err := s.wb.Exact()
	respondFrame(w, frame, err)
}

func (s *server) renderResume(w http.ResponseWriter, r *http.Request) {
	frame, err := s.wb.Resume()
	respondFrame(w, frame, err)
}

func respondFrame(w http.ResponseWriter, frame *fog.Frame, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err, "frame": frame})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frame": frame})
}

type editRequest struct {
	Kind    string `json:"kind"`
	MediaID string `json:"media_id"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Z       int    `json:"z"`
	Value   int    `json:"value"`
	ValueQ  int64  `json:"value_q"`
}

func (s *server) edit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	var req editRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	switch req.Kind {
	case "voxel":
		invs, frame, err := s.wb.EditAndRecomputeVoxel(req.MediaID, req.X, req.Y, req.Z, req.Value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err, "invalidations": invs})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "invalidations": invs, "frame": frame})
	case "scattering":
		valueQ := req.ValueQ
		if valueQ == 0 && req.Value != 0 {
			valueQ = int64(req.Value) * fog.CoeffScale
		}
		invs, frame, err := s.wb.EditAndRecomputeScattering(req.MediaID, valueQ)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err, "invalidations": invs})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "invalidations": invs, "frame": frame})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "kind must be voxel or scattering"})
	}
}

type faultRequest struct {
	Kind string `json:"kind"`
	CX   *int   `json:"cx"`
	CY   *int   `json:"cy"`
}

func (s *server) fault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	var req faultRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cx, cy := -1, -1
	if req.CX != nil {
		cx = *req.CX
	}
	if req.CY != nil {
		cy = *req.CY
	}
	rep := s.wb.DiagnoseFault(req.Kind, cx, cy)
	status := http.StatusOK
	if rep.Error != nil {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"ok": rep.Error == nil, "report": rep})
}

func (s *server) truth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	if err := s.wb.ValidateTruth(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "analytic truth checks passed"})
}

func (s *server) compare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	rep := s.wb.CompareThreePaths()
	if !rep.Passed {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "report": rep})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

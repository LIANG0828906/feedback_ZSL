package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"iconcut"
	"iconcut/verifier"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	workspace  string
	mu         sync.Mutex
	controller *iconcut.Controller
	http       *http.Server
}

func NewServer(workspace string) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(workspace, "samples"), 0o755); err != nil {
		return nil, err
	}
	for _, sp := range iconcut.DefaultSpecs() {
		if _, _, err := iconcut.GenerateBundle(filepath.Join(workspace, "samples"), sp); err != nil {
			return nil, err
		}
	}
	return &Server{workspace: workspace, controller: iconcut.NewController()}, nil
}

func (s *Server) ListenAndServe(addr string) error {
	s.http = &http.Server{Addr: addr, Handler: s.Handler()}
	return s.http.ListenAndServe()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/image", s.handleImage)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/control", s.handleControl)
	mux.HandleFunc("/api/compare", s.handleCompare)
	mux.HandleFunc("/api/verify", s.handleVerify)
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func StaticFS() fs.FS {
	sub, _ := fs.Sub(staticFS, "static")
	return sub
}

func (s *Server) bundleDir(imageID string) string {
	return filepath.Join(s.workspace, "samples", imageID)
}

func (s *Server) dbPath(imageID string) string {
	return filepath.Join(s.workspace, "runs", imageID+".db")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if e, ok := iconcut.AsError(err); ok {
		switch e.Code {
		case iconcut.EMeta, iconcut.EPara:
			code = http.StatusBadRequest
		}
	}
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.workspace, "samples")
	entries, err := os.ReadDir(root)
	if err != nil {
		writeErr(w, err)
		return
	}
	var out []*iconcut.Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := iconcut.LoadManifest(filepath.Join(root, e.Name(), "manifest.json"))
		if err == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, map[string]any{"ok": true, "samples": out})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ImageID string `json:"image_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ImageID == "" {
		req.ImageID = "icon-192"
	}
	b, err := iconcut.LoadBundle(s.bundleDir(req.ImageID))
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.dbPath(req.ImageID)), 0o755)
	rep, runErr := iconcut.RunBundle(r.Context(), b, iconcut.RunOptions{DBPath: s.dbPath(req.ImageID), Controller: s.controller})
	if rep != nil {
		if rep.Corrected != nil {
			_ = iconcut.WritePNG(filepath.Join(s.workspace, "runs", req.ImageID+"-corrected.png"), rep.Corrected)
		}
		if rep.Mask != nil {
			_ = iconcut.WriteLabelPNG(filepath.Join(s.workspace, "runs", req.ImageID+"-mask.png"), rep.Mask)
		}
	}
	writeJSON(w, map[string]any{"ok": runErr == nil, "error": errorString(runErr), "report": rep})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	imageID := r.URL.Query().Get("image_id")
	if imageID == "" {
		imageID = "icon-192"
	}
	st, err := iconcut.OpenStore(s.dbPath(imageID))
	if err != nil {
		writeJSON(w, map[string]any{"ok": true, "states": []iconcut.TileState{}, "commits": []iconcut.CommitRecord{}})
		return
	}
	defer st.Close()
	states, _ := st.ListTileStates(r.Context(), imageID)
	commits, _ := st.LoadCommits(r.Context(), imageID)
	writeJSON(w, map[string]any{"ok": true, "states": states, "commit_count": len(commits)})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	imageID := r.URL.Query().Get("image_id")
	if imageID == "" {
		imageID = "icon-192"
	}
	b, err := iconcut.LoadBundle(s.bundleDir(imageID))
	if err != nil {
		writeErr(w, err)
		return
	}
	params := b.Manifest.Declared.Segment
	if v := r.URL.Query().Get("color_tolerance"); v != "" {
		var f float64
		_, _ = fmt.Sscan(v, &f)
		params.ColorTolerance = f
	}
	if v := r.URL.Query().Get("gradient_threshold"); v != "" {
		var f float64
		_, _ = fmt.Sscan(v, &f)
		params.GradientThreshold = f
	}
	corrected, _, err := iconcut.CorrectWhole(b.Source, b.Manifest.Geometry, b.Manifest.Declared.InterpOrder)
	if err != nil {
		writeErr(w, err)
		return
	}
	preview, err := iconcut.RunPreview(corrected, b.Manifest, params, iconcut.PreviewOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	full, err := iconcut.RunBundle(r.Context(), b, iconcut.RunOptions{
		DBPath: filepath.Join(s.workspace, "runs", imageID+"-preview.db"), SkipTruthCheck: true, Params: &params,
	})
	var divergence *iconcut.Error
	if err == nil && full != nil {
		divergence = iconcut.ComparePreview(full.Stats, preview.Stats, full.Mask, preview.Upscaled, b.Manifest.Declared.Tolerance, preview.ParamVersion, preview.SchemeVersion)
	}
	writeJSON(w, map[string]any{"ok": true, "preview": preview, "divergence": divergence, "full_stats": statsOrEmpty(full)})
}

func statsOrEmpty(r *iconcut.Report) []iconcut.RegionStats {
	if r == nil {
		return []iconcut.RegionStats{}
	}
	return r.Stats
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	imageID := r.URL.Query().Get("image_id")
	name := r.URL.Query().Get("name")
	if imageID == "" || name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var path string
	switch name {
	case "source", "reference":
		path = filepath.Join(s.bundleDir(imageID), name+".png")
	case "corrected", "mask":
		path = filepath.Join(s.workspace, "runs", imageID+"-"+name+".png")
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ImageID string `json:"image_id"`
		Tile    int    `json:"tile"`
		Stage   string `json:"stage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	err := iconcut.RetryTile(r.Context(), s.dbPath(req.ImageID), req.ImageID, req.Tile, req.Stage)
	writeJSON(w, map[string]any{"ok": err == nil, "error": errorString(err)})
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	switch req.Action {
	case "pause":
		s.controller.Pause()
	case "resume":
		s.controller.Resume()
	case "cancel":
		s.controller.Cancel()
		s.mu.Lock()
		s.controller = iconcut.NewController()
		s.mu.Unlock()
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "action must be pause/resume/cancel"})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	imageID := r.URL.Query().Get("image_id")
	if imageID == "" {
		imageID = "icon-192"
	}
	b, err := iconcut.LoadBundle(s.bundleDir(imageID))
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx := context.Background()
	fullPath := filepath.Join(s.workspace, "runs", imageID+"-compare-full.db")
	resumePath := filepath.Join(s.workspace, "runs", imageID+"-compare-resume.db")
	full, err := iconcut.RunBundle(ctx, b, iconcut.RunOptions{DBPath: fullPath, SkipTruthCheck: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	prefix, err := iconcut.RunBundle(ctx, b, iconcut.RunOptions{DBPath: resumePath, StopAfterCommits: 7, SkipTruthCheck: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	resume, err := iconcut.RunBundle(ctx, b, iconcut.RunOptions{DBPath: resumePath, SkipTruthCheck: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	maskDiff := iconcut.CompareLabelMasks(full.Mask, resume.Mask, b.Manifest.Declared.TileWidth, b.Manifest.Declared.TileHeight)
	statsDiff := iconcut.CompareStats(full.Stats, resume.Stats)
	fullCommits, _ := iconcut.LoadCommitOrder(ctx, fullPath, imageID)
	resumeCommits, _ := iconcut.LoadCommitOrder(ctx, resumePath, imageID)
	commitDiff := iconcut.CompareCommitSequences(fullCommits, resumeCommits)
	writeJSON(w, map[string]any{
		"ok":        prefix.Interrupted && maskDiff == nil && statsDiff == nil && commitDiff == nil,
		"mask_diff": maskDiff, "stats_diff": statsDiff, "commit_diff": commitDiff,
		"full_regions": full.Stats, "resume_regions": resume.Stats,
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	imageID := r.URL.Query().Get("image_id")
	if imageID == "" {
		imageID = "icon-192"
	}
	rep, err := verifier.VerifyDB(r.Context(), s.dbPath(imageID))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, rep)
}

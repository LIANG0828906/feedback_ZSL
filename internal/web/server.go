package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"meshdoctor/internal/app"
	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
	"meshdoctor/internal/pipeline"
	"meshdoctor/internal/sample"
)

//go:embed index.html
var indexHTML []byte

type Server struct {
	app *app.App
	mux *http.ServeMux
}

func New(a *app.App) *Server {
	s := &Server{app: a, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ListenAndServe(addr string) error {
	_, _ = s.app.Verify(context.Background(), true)
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/verify", s.handleVerify)
	s.mux.HandleFunc("/api/run", s.handleRun)
	s.mux.HandleFunc("/api/cancel", s.handleCancel)
	s.mux.HandleFunc("/api/export", s.handleExport)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	samples, _ := s.app.Samples()
	manifests := make([]sample.Manifest, 0, len(samples))
	for _, sm := range samples {
		manifests = append(manifests, sm.Manifest)
	}
	writeJSON(w, map[string]any{
		"ok": true, "params": s.app.Params, "manifests": manifests,
		"report": s.app.Report, "runs": s.app.RunStatuses(),
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	parallel := r.URL.Query().Get("parallel") != "0"
	report, err := s.app.Verify(r.Context(), parallel)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, report)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	sampleID := r.URL.Query().Get("sample")
	if sampleID == "" {
		writeError(w, errs.New(errs.EMETA, "query parameter sample is required"))
		return
	}
	params := s.app.Params
	if v := r.URL.Query().Get("weight"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			writeError(w, errs.New(errs.EPARA, "invalid weight: %v", err))
			return
		}
		params.LaplacianWeight = f
	}
	if v := r.URL.Query().Get("iterations"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, errs.New(errs.EPARA, "invalid iterations: %v", err))
			return
		}
		params.SmoothIterations = n
	}
	if err := params.ValidateSmooth(); err != nil {
		writeError(w, err)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "incremental"
	}
	stopAfter := 0
	if stageQuery := r.URL.Query().Get("stage"); stageQuery != "" {
		if idx := pipeline.StageIndex(stageQuery); idx >= 0 {
			stopAfter = idx + 1
		} else if n, err := strconv.Atoi(stageQuery); err == nil && n >= 0 && n < len(pipeline.Stages) {
			stopAfter = n + 1
		}
	}
	if err := s.app.StartRun(sampleID, mode, params, stopAfter); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "sample": sampleID, "state": "running"})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sampleID := r.URL.Query().Get("sample")
	if err := s.app.CancelRun(sampleID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "sample": sampleID, "cancel": "requested"})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	sampleID := r.URL.Query().Get("sample")
	pathName := r.URL.Query().Get("path")
	stageName := r.URL.Query().Get("stage")

	if format == "report" || (sampleID == "" && format == "json") {
		writeJSON(w, s.app.Report)
		return
	}
	if s.app.Report == nil {
		writeError(w, errs.New(errs.ESTATE, "verification report not ready; run /api/verify first"))
		return
	}
	var sv *app.SampleVerification
	for i := range s.app.Report.Samples {
		if s.app.Report.Samples[i].SampleID == sampleID {
			sv = &s.app.Report.Samples[i]
			break
		}
	}
	if sv == nil {
		writeError(w, errs.New(errs.EMETA, "unknown sample %q", sampleID))
		return
	}
	result := selectPath(sv, pathName)
	if result == nil {
		writeError(w, errs.New(errs.EMETA, "unknown path %q", pathName))
		return
	}
	snap := selectStage(result, stageName)
	if snap == nil || snap.Mesh == nil {
		writeError(w, errs.New(errs.EMETA, "unknown stage %q", stageName))
		return
	}

	switch strings.ToLower(format) {
	case "obj":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.obj", sampleID, snap.Stage))
		writeOBJ(w, snap.Mesh, snap.Stage)
	case "json":
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.json", sampleID, snap.Stage))
		writeJSON(w, snap)
	default:
		writeError(w, errs.New(errs.EMETA, "unsupported format %q", format))
	}
}

func selectPath(sv *app.SampleVerification, name string) *pipeline.PathResult {
	switch name {
	case "", "resume":
		return sv.Resume
	case "one_shot", "oneshot":
		return sv.OneShot
	case "segmented":
		return sv.Segmented
	default:
		return nil
	}
}

func selectStage(r *pipeline.PathResult, name string) *pipeline.StageSnapshot {
	if r == nil {
		return nil
	}
	if name == "" || name == "final" || name == pipeline.StageStats {
		if len(r.Stages) == 0 {
			return nil
		}
		return &r.Stages[len(r.Stages)-1]
	}
	if idx, err := strconv.Atoi(name); err == nil {
		if idx < 0 || idx >= len(r.Stages) {
			return nil
		}
		return &r.Stages[idx]
	}
	for i := range r.Stages {
		if r.Stages[i].Stage == name {
			return &r.Stages[i]
		}
	}
	return nil
}

func writeOBJ(w http.ResponseWriter, m *mesh.Mesh, stage string) {
	fmt.Fprintf(w, "# MeshDoctor stage %s hash=%s\n", stage, m.Hash())
	for _, v := range m.Vertices {
		fmt.Fprintf(w, "v %.9f %.9f %.9f\n", v[0], v[1], v[2])
	}
	for _, f := range m.Faces {
		fmt.Fprintf(w, "f %d %d %d\n", f.A+1, f.B+1, f.C+1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(errs.JSON(err))
}

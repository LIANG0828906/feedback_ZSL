package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"sync"

	"maskqc"
	"maskqc/store"
)

//go:embed index.html
var webFS embed.FS

type server struct {
	mu      sync.Mutex
	samples []*maskqc.Project
	engines map[int]*maskqc.Engine
	st      *store.Store
	params  maskqc.Params
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbPath := flag.String("db", "maskqc-checkpoints.db", "SQLite checkpoint database path")
	batches := flag.Int("batches", 3, "deterministic sample batches")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open SQLite: %v", err)
	}
	defer st.Close()

	params := maskqc.DefaultParams()
	samples := maskqc.GenerateSamples(*batches)
	srv := &server{
		samples: samples,
		engines: map[int]*maskqc.Engine{},
		st:      st,
		params:  params,
	}
	for i, p := range samples {
		engine := maskqc.NewEngine(p, params, st)
		if _, err := engine.FullRebuild(context.Background()); err != nil {
			var pe *maskqc.Error
			if errors.As(err, &pe) && pe.Code == maskqc.CodeTop {
				log.Printf("batch %d initial topology diagnostic: %v", i, pe.Code)
			} else if err != nil {
				log.Fatalf("warm engine %d: %v", i, err)
			}
		}
		srv.engines[i] = engine
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.index)
	mux.HandleFunc("/api/samples", srv.handleSamples)
	mux.HandleFunc("/api/run", srv.handleRun)
	mux.HandleFunc("/api/edit", srv.handleEdit)
	mux.HandleFunc("/api/reset", srv.handleReset)
	mux.HandleFunc("/api/equivalence", srv.handleEquivalence)
	mux.HandleFunc("/api/export", srv.handleExport)
	mux.HandleFunc("/image", srv.handleImage)

	log.Printf("Mask QC diagnostic UI: http://%s/", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *server) handleSamples(w http.ResponseWriter, r *http.Request) {
	type sampleView struct {
		Batch   int                   `json:"batch"`
		ID      string                `json:"id"`
		Seed    int64                 `json:"seed"`
		Width   int                   `json:"width"`
		Height  int                   `json:"height"`
		Strokes []maskqc.Stroke       `json:"strokes"`
		Domains []maskqc.DomainMetric `json:"truth_domains"`
	}
	out := make([]sampleView, 0, len(s.samples))
	for i, p := range s.samples {
		out = append(out, sampleView{Batch: i, ID: p.ID, Seed: p.Seed, Width: p.Width, Height: p.Height, Strokes: p.Strokes, Domains: p.Truth.Domains})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	engine, ok := s.engineForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, engine.Result)
}

func (s *server) handleEdit(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, engine, ok := s.engineForRequestLocked(w, r)
	if !ok {
		return
	}
	edit, perr := editFromRequest(r)
	if perr != nil {
		writeError(w, http.StatusBadRequest, perr)
		return
	}
	result, err := engine.ApplyEdit(r.Context(), edit)
	if err != nil {
		var pe *maskqc.Error
		if errors.As(err, &pe) && pe.Code == maskqc.CodeTop {
			writeJSON(w, http.StatusOK, map[string]any{"batch": batch, "result": result, "topology_error": pe})
			return
		}
		status := http.StatusInternalServerError
		if pe, yes := maskqc.AsError(err); yes {
			switch pe.Code {
			case maskqc.CodePara, maskqc.CodeSchema:
				status = http.StatusBadRequest
			}
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": batch, "result": result})
}

func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, ok := intQuery(r, "batch")
	if !ok {
		http.Error(w, "batch required", http.StatusBadRequest)
		return
	}
	if batch < 0 || batch >= len(s.samples) {
		http.Error(w, "batch out of range", http.StatusBadRequest)
		return
	}
	engine := maskqc.NewEngine(maskqc.CloneForEdit(s.samples[batch]), s.params, s.st)
	if _, err := engine.FullRebuild(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engines[batch] = engine
	writeJSON(w, http.StatusOK, engine.Result)
}

func (s *server) handleEquivalence(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, engine, ok := s.engineForRequestLocked(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	identity := engine.Result.Identity
	validation, records, err := s.st.RecoverySnapshot(ctx, identity)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	recovery, rerr := maskqc.RunPipelineWithRecords(ctx, engine.Project, s.params, "breakpoint-recovery", s.st, records)
	if rerr != nil {
		var pe *maskqc.Error
		if !(errors.As(rerr, &pe) && pe.Code == maskqc.CodeTop) {
			writeError(w, http.StatusInternalServerError, rerr)
			return
		}
	}
	full, ferr := maskqc.RunPipeline(ctx, maskqc.CloneForEdit(engine.Project), s.params, "one-shot-full", nil)
	if ferr != nil {
		var pe *maskqc.Error
		if !(errors.As(ferr, &pe) && pe.Code == maskqc.CodeTop) {
			writeError(w, http.StatusInternalServerError, ferr)
			return
		}
	}
	report := maskqc.VerifyPipelineEquivalence(engine.Result, recovery, full)
	writeJSON(w, http.StatusOK, map[string]any{
		"report":            report,
		"snapshot":          validation,
		"durable_records":   len(records),
		"realtime_topology": engine.Result.Topology,
		"recovery_topology": recovery.Topology,
		"full_topology":     full.Topology,
	})
}

func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, ok := intQuery(r, "batch")
	if !ok || batch < 0 || batch >= len(s.samples) {
		http.Error(w, "batch required/out of range", http.StatusBadRequest)
		return
	}
	target := r.URL.Query().Get("to")
	if target == "" {
		target = maskqc.SchemaV1
	}
	result, err := maskqc.ExportProject(s.samples[batch], target)
	if err != nil && result == nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	loaded, lerr := maskqc.LoadProjectDocument(result.Data)
	loadInfo := map[string]any{"ok": lerr == nil}
	if lerr != nil {
		loadInfo["error"] = lerr.Error()
	} else {
		loadInfo["stroke_count"] = len(loaded.Project.Strokes)
		loadInfo["trail_count"] = len(loaded.Trail)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema": result.Schema, "loss": result.Loss, "trail": result.Trail,
		"roundtrip": loadInfo, "document": json.RawMessage(result.Data),
	})
}

func (s *server) handleImage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, ok := intQuery(r, "batch")
	if !ok || batch < 0 || batch >= len(s.samples) {
		http.Error(w, "batch required", http.StatusBadRequest)
		return
	}
	kind := r.URL.Query().Get("kind")
	p := s.samples[batch]
	w.Header().Set("Content-Type", "image/png")
	switch kind {
	case "image":
		_ = png.Encode(w, p.Image)
	case "candidate":
		_ = png.Encode(w, p.Candidate)
	case "truth":
		_ = png.Encode(w, p.Truth.Mask)
	default:
		engine := s.engines[batch]
		if engine == nil || engine.Result == nil {
			http.Error(w, "mask not ready", http.StatusServiceUnavailable)
			return
		}
		_ = png.Encode(w, engine.Result.Mask)
	}
}

func (s *server) engineForRequest(w http.ResponseWriter, r *http.Request) (*maskqc.Engine, bool) {
	_, e, ok := s.engineForRequestLocked(w, r)
	return e, ok
}

func (s *server) engineForRequestLocked(w http.ResponseWriter, r *http.Request) (int, *maskqc.Engine, bool) {
	batch, ok := intQuery(r, "batch")
	if !ok {
		http.Error(w, "batch required", http.StatusBadRequest)
		return 0, nil, false
	}
	e := s.engines[batch]
	if e == nil {
		http.Error(w, "batch out of range", http.StatusBadRequest)
		return batch, nil, false
	}
	return batch, e, true
}

func editFromRequest(r *http.Request) (maskqc.Edit, *maskqc.Error) {
	seq, ok := intQuery(r, "seq")
	if !ok {
		return maskqc.Edit{}, &maskqc.Error{Code: maskqc.CodeSchema, Path: "/seq", Field: "seq", Message: "seq query parameter required"}
	}
	kind := maskqc.EditKind(r.URL.Query().Get("kind"))
	edit := maskqc.Edit{Kind: kind, StrokeSeq: seq}
	switch kind {
	case maskqc.EditMoveStart, maskqc.EditMoveEnd:
		x, ok1 := intQuery(r, "x")
		y, ok2 := intQuery(r, "y")
		if !ok1 || !ok2 {
			return edit, &maskqc.Error{Code: maskqc.CodePara, Field: "x,y", Message: "move edits require x and y"}
		}
		edit.Point = maskqc.Point{X: x, Y: y}
	case maskqc.EditWeight, maskqc.EditPriority, maskqc.EditHardness, maskqc.EditRadius:
		v, ok := intQuery(r, "value")
		if !ok {
			return edit, &maskqc.Error{Code: maskqc.CodePara, Field: "value", Message: "value required"}
		}
		edit.Value = v
	default:
		return edit, &maskqc.Error{Code: maskqc.CodePara, Field: "kind", Message: "unsupported edit kind"}
	}
	return edit, nil
}

func intQuery(r *http.Request, key string) (int, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	var pe *maskqc.Error
	if errors.As(err, &pe) {
		writeJSON(w, status, pe)
		return
	}
	writeJSON(w, status, map[string]string{"code": "E_STATE", "message": err.Error()})
}

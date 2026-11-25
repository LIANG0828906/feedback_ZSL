package fogkernel

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

//go:embed web/index.html
var webFS embed.FS

type Workbench struct {
	Bundle       *GeneratedBundle
	Verification *VerificationReport
	SourceHash   string

	mu             sync.Mutex
	session        *IncrementalSession
	loadedSchema   string
	checkpointPath string
}

func NewWorkbench() (*Workbench, error) {
	bundle, err := GenerateDefaultBundle()
	if err != nil {
		return nil, err
	}
	verification, err := VerifyGeneratedBundle(bundle)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	for _, schema := range []string{SchemaV1, SchemaV2, SchemaV3} {
		_, _ = hasher.Write(bundle.Docs[schema])
	}
	cp := filepath.Join(os.TempDir(), "fogkernel-workbench-checkpoint.sqlite")
	_ = os.Remove(cp)
	w := &Workbench{
		Bundle: bundle, Verification: verification, SourceHash: hex.EncodeToString(hasher.Sum(nil)),
		session: NewIncrementalSession(bundle.Canonical), loadedSchema: "canonical",
		checkpointPath: cp,
	}
	return w, nil
}

func (w *Workbench) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bootstrap", w.handleBootstrap)
	mux.HandleFunc("/api/project", w.handleProject)
	mux.HandleFunc("/api/load", w.handleLoad)
	mux.HandleFunc("/api/solve", w.handleSolve)
	mux.HandleFunc("/api/compare", w.handleCompare)
	mux.HandleFunc("/api/edit", w.handleEdit)
	mux.HandleFunc("/api/reset", w.handleReset)
	mux.HandleFunc("/api/inject", w.handleInject)
	mux.HandleFunc("/api/checkpoint", w.handleCheckpoint)

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

type apiEnvelope struct {
	OK    bool      `json:"ok"`
	Data  any       `json:"data,omitempty"`
	Error *BizError `json:"error,omitempty"`
}

func writeData(rw http.ResponseWriter, data any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(rw).Encode(apiEnvelope{OK: true, Data: data})
}

func writeError(rw http.ResponseWriter, err error) {
	biz, ok := AsBizError(err)
	if !ok {
		biz = NewBizError("E_INTERNAL", "$", "%v", err)
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(rw).Encode(apiEnvelope{OK: false, Error: biz})
}

func decodeBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return NewBizError(CodeSchemaField, "$", "invalid JSON request: %v", err)
	}
	return nil
}

func (w *Workbench) handleBootstrap(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	scene := w.session.Scene()
	writeData(rw, map[string]any{
		"seed": w.Bundle.Seed, "project_id": scene.ProjectID, "scene_version": scene.Version(),
		"source_hash": w.SourceHash, "loaded_schema": w.loadedSchema,
		"schemas": []map[string]string{
			{"id": SchemaV1, "label": "v1 Pascal / flat / ‰ / integer mm / phase omitted"},
			{"id": SchemaV2, "label": "v2 snake / voxel tree / ‱ / decimal m / phase explicit"},
			{"id": SchemaV3, "label": "v3 lowerCamel / voxel tree / ‰ / decimal m / phase explicit"},
		},
		"media": scene.Media, "cameras": scene.Cameras, "recipe": scene.Recipe,
		"rays": scene.Rays, "truth": w.Bundle.Truth, "verification": w.Verification,
		"conversion_audits": w.Bundle.ConversionAudits,
		"checkpoint_path":   w.checkpointPath,
	})
}

func (w *Workbench) handleProject(rw http.ResponseWriter, r *http.Request) {
	schema := r.URL.Query().Get("schema")
	if schema == "" {
		schema = SchemaV1
	}
	doc, ok := w.Bundle.Docs[schema]
	if !ok {
		writeError(rw, NewBizError(CodeSchemaVersion, "$.schema", "unknown schema %q", schema))
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = rw.Write(doc)
}

type loadRequest struct {
	Schema string `json:"schema"`
}

func (w *Workbench) handleLoad(rw http.ResponseWriter, r *http.Request) {
	var req loadRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(rw, err)
		return
	}
	if req.Schema == "" {
		req.Schema = SchemaV1
	}
	loaded, err := LoadProject(w.Bundle.Docs[req.Schema])
	if err != nil {
		writeError(rw, err)
		return
	}
	w.mu.Lock()
	w.session = NewIncrementalSession(loaded)
	w.loadedSchema = req.Schema
	report := w.session.Report()
	w.mu.Unlock()
	writeData(rw, map[string]any{
		"loaded": req.Schema, "equivalent_to_canonical": ScenesEquivalent(loaded, w.Bundle.Canonical),
		"scene_version": loaded.Version(), "initial_report": report,
	})
}

func (w *Workbench) handleSolve(rw http.ResponseWriter, r *http.Request) {
	var opts SolveOptions
	if err := decodeBody(r, &opts); err != nil {
		writeError(rw, err)
		return
	}
	w.mu.Lock()
	scene := w.session.Scene()
	w.mu.Unlock()
	report, err := SolveScene(scene, opts)
	if err != nil {
		writeError(rw, err)
		return
	}
	writeData(rw, report)
}

func (w *Workbench) handleCompare(rw http.ResponseWriter, r *http.Request) {
	report, err := CrossVersionReport(w.Bundle)
	if err != nil {
		writeError(rw, err)
		return
	}
	blockAudit, err := AuditBlockTauAcrossVersions(w.Bundle)
	if err != nil {
		writeError(rw, err)
		return
	}
	writeData(rw, map[string]any{
		"cross_version": report, "block_tau_audit": blockAudit,
		"conversion_audits": w.Bundle.ConversionAudits, "verification": w.Verification,
	})
}

func (w *Workbench) handleEdit(rw http.ResponseWriter, r *http.Request) {
	var req EditRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(rw, err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	result, err := w.session.ApplyEdit(req)
	if err != nil {
		writeError(rw, err)
		return
	}
	writeData(rw, map[string]any{"result": result, "source_hash": w.SourceHash})
}

func (w *Workbench) handleReset(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	w.session = NewIncrementalSession(w.Bundle.Canonical)
	w.loadedSchema = "canonical"
	w.mu.Unlock()
	writeData(rw, map[string]any{"reset": true, "source_hash": w.SourceHash})
}

func (w *Workbench) handleInject(rw http.ResponseWriter, r *http.Request) {
	var req InjectionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(rw, err)
		return
	}
	w.mu.Lock()
	existing := 0
	if w.session != nil && w.session.lastReport != nil {
		existing = len(w.session.lastReport.Rays)
	}
	w.mu.Unlock()
	result, err := RunInjection(w.Bundle, req)
	result.SourceHash = w.SourceHash
	result.ExistingRayResults = existing
	if err != nil {
		writeError(rw, err)
		return
	}
	writeData(rw, result)
}

type checkpointRequest struct {
	Action           string `json:"action"`
	DBPath           string `json:"db_path"`
	CrashAfterBlocks int    `json:"crash_after_blocks"`
	StopAfterRays    int    `json:"stop_after_rays"`
	AutoReset        bool   `json:"auto_reset"`
}

func (w *Workbench) handleCheckpoint(rw http.ResponseWriter, r *http.Request) {
	var req checkpointRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(rw, err)
		return
	}
	if req.DBPath == "" {
		req.DBPath = w.checkpointPath
	}
	scene := w.Bundle.Canonical
	version := scene.Version()

	switch req.Action {
	case "state":
		state, err := InspectCheckpoint(req.DBPath, version)
		if err != nil {
			writeError(rw, err)
			return
		}
		writeData(rw, state)

	case "crash":
		_ = os.Remove(req.DBPath)
		store, err := CreateFreshCheckpoint(req.DBPath, version)
		if err != nil {
			writeError(rw, err)
			return
		}
		after := req.CrashAfterBlocks
		if after <= 0 {
			after = 3
		}
		runOut, runErr := RunCheckpointed(context.Background(), scene, store, nil, CheckpointRunOptions{CrashAfterBlockCommits: after})
		_ = store.Close()
		if runErr != nil && !errors.Is(runErr, ErrCrashInjected) {
			writeError(rw, runErr)
			return
		}
		inspection, inspectErr := InspectCheckpoint(req.DBPath, version)
		writeData(rw, map[string]any{"crashed": true, "run": runOut, "run_error": errorString(runErr), "inspection": inspection, "inspect_error": bizOrNil(inspectErr)})

	case "signal":
		_ = os.Remove(req.DBPath)
		store, err := CreateFreshCheckpoint(req.DBPath, version)
		if err != nil {
			writeError(rw, err)
			return
		}
		afterRays := req.StopAfterRays
		if afterRays <= 0 {
			afterRays = 2
		}
		runOut, runErr := RunCheckpointed(context.Background(), scene, store, nil, CheckpointRunOptions{StopAfterCompleteRays: afterRays})
		_ = store.Close()
		if runErr != nil {
			writeError(rw, runErr)
			return
		}
		inspection, _ := InspectCheckpoint(req.DBPath, version)
		writeData(rw, map[string]any{"signalled": true, "run": runOut, "inspection": inspection})

	case "resume":
		store, recovery, err := OpenCheckpointStore(req.DBPath, version)
		resetReason := ""
		if err != nil {
			biz, ok := AsBizError(err)
			if ok && biz.Code == CodeCheckpointVersion && req.AutoReset {
				resetReason = biz.Error()
				store, err = CreateFreshCheckpoint(req.DBPath, version)
				recovery = nil
			}
		}
		if err != nil {
			writeError(rw, err)
			return
		}
		runOut, err := RunCheckpointed(context.Background(), scene, store, recovery, CheckpointRunOptions{})
		_ = store.Close()
		if err != nil {
			writeError(rw, err)
			return
		}
		full, solveErr := SolveScene(scene, SolveOptions{Path: PathExact, Workers: 1, Buffer: 64})
		matches := solveErr == nil && reportsApproxEqual(runOut.Report, full)
		writeData(rw, map[string]any{"run": runOut, "matches_full": matches, "reset_reason": resetReason})

	default:
		writeError(rw, NewBizError(CodeEditTarget, "$.action", "unknown checkpoint action %q", req.Action))
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func bizOrNil(err error) any {
	if err == nil {
		return nil
	}
	biz, _ := AsBizError(err)
	if biz != nil {
		return biz
	}
	return err.Error()
}

func reportsApproxEqual(a, b *SolveReport) bool {
	if a == nil || b == nil || len(a.Rays) != len(b.Rays) {
		return false
	}
	for i := range a.Rays {
		if !near(a.Rays[i].Transmittance, b.Rays[i].Transmittance, 1e-10) ||
			a.Rays[i].TerminalBlock != b.Rays[i].TerminalBlock ||
			len(a.Rays[i].Blocks) != len(b.Rays[i].Blocks) {
			return false
		}
	}
	return true
}

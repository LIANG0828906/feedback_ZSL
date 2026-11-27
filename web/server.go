package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"stagepreview/stage"
)

type checkpointSummary struct {
	Name        string `json:"name"`
	Window      int    `json:"window"`
	ContentHash string `json:"content_hash"`
	Fixtures    int    `json:"fixtures"`
	Aggregates  int    `json:"aggregates"`
}

type visualPoint struct {
	FixtureID    string `json:"fixture_id"`
	WindowNumber int    `json:"window_number"`
	X            int64  `json:"x"`
	Y            int64  `json:"y"`
	Z            int64  `json:"z"`
	Intensity    int64  `json:"intensity"`
	Uncertain    bool   `json:"uncertain"`
}

type visualFixture struct {
	ID string `json:"id"`
	X  int64  `json:"x"`
	Y  int64  `json:"y"`
	Z  int64  `json:"z"`
}

type pageView struct {
	Title           string
	Flash           string
	Error           string
	Project         *stage.Project
	SchemaVersions  []string
	TraceCount      int
	DefaultCount    int
	LastRun         *stage.RunResult
	Preview         *stage.PreviewReport
	Comparison      *stage.PreviewComparison
	Verify          *stage.VerifyReport
	Invalidation    *stage.InvalidationReport
	Checkpoints     []checkpointSummary
	CheckpointError string
	SamplesDir      string
	RuntimeDir      string
}

type Server struct {
	mu          sync.Mutex
	samplesDir  string
	runtimeDir  string
	seed        int64
	currentPath string
	project     *stage.Project
	lastRun     *stage.RunResult
	lastPreview *stage.PreviewReport
	lastCmp     *stage.PreviewComparison
	lastVerify  *stage.VerifyReport
	lastInvalid *stage.InvalidationReport
	previousRun *stage.Project
	flash       string
	flashError  string
}

func New(samplesDir, runtimeDir string, seed int64) (*Server, error) {
	if strings.TrimSpace(samplesDir) == "" || strings.TrimSpace(runtimeDir) == "" {
		return nil, &stage.Error{Code: stage.CodeValue, Message: "samples and runtime directories are required", Field: "web.directories"}
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, err
	}
	v3Path := filepath.Join(samplesDir, "v3.json")
	if _, err := os.Stat(v3Path); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		bundle, err := stage.GenerateSampleBundle(seed)
		if err != nil {
			return nil, err
		}
		if err := bundle.Write(samplesDir); err != nil {
			return nil, err
		}
	}
	s := &Server{
		samplesDir: samplesDir, runtimeDir: runtimeDir, seed: seed,
		currentPath: filepath.Join(runtimeDir, "current-v3.json"),
		flash:       "确定性样本已就绪；页面运行在回环地址。",
	}
	p, err := s.loadInitialProject()
	if err != nil {
		return nil, err
	}
	s.project = p
	return s, nil
}

func (s *Server) loadInitialProject() (*stage.Project, error) {
	if data, err := os.ReadFile(s.currentPath); err == nil {
		p, err := stage.LoadProject(data)
		if err == nil {
			s.flash = "已加载 runtime/current-v3.json 中的持久化编辑状态。"
			return p, nil
		}
		log.Printf("ignoring invalid current project: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.samplesDir, "v3.json"))
	if err != nil {
		return nil, err
	}
	return stage.LoadProject(data)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/live-preview", s.handleLivePreview)
	mux.HandleFunc("POST /generate", s.handleGenerate)
	mux.HandleFunc("POST /reset", s.handleReset)
	mux.HandleFunc("POST /preview", s.handlePreview)
	mux.HandleFunc("POST /exact", s.handleExact)
	mux.HandleFunc("POST /verify", s.handleVerify)
	mux.HandleFunc("POST /fixture", s.handleFixture)
	mux.HandleFunc("POST /cue-color", s.handleCueColor)
	return mux
}

func Listen(addr string, s *Server) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return &stage.Error{
				Code: stage.CodeValue, Message: "diagnostic server must bind to a loopback address",
				Field: "addr", Actual: addr,
			}
		}
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("stagepreview listening on http://%s", addr)
	return srv.Serve(ln)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	view := s.viewLocked()
	s.flash = ""
	s.flashError = ""
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, view); err != nil {
		log.Printf("render index: %v", err)
	}
}

func (s *Server) viewLocked() pageView {
	view := pageView{
		Title: "Stage Preview", Flash: s.flash, Error: s.flashError,
		Project: s.project, SchemaVersions: stage.SupportedSchemas,
		LastRun: s.lastRun, Preview: s.lastPreview, Comparison: s.lastCmp,
		Verify: s.lastVerify, Invalidation: s.lastInvalid,
		SamplesDir: s.samplesDir, RuntimeDir: s.runtimeDir,
	}
	if s.project != nil {
		view.TraceCount = len(s.project.Traces)
		for _, tr := range s.project.Traces {
			if tr.Operation == "apply_default" {
				view.DefaultCount++
			}
		}
	}
	if s.project != nil {
		store := stage.NewCheckpointStore(filepath.Join(s.runtimeDir, "checkpoints"))
		windows, err := store.LoadAll(s.project.ParameterVersion, stage.AlgorithmVersion)
		if err != nil {
			view.CheckpointError = err.Error()
		}
		for _, w := range windows {
			view.Checkpoints = append(view.Checkpoints, checkpointSummary{
				Name: fmt.Sprintf("window-%06d.json", w.WindowNumber), Window: w.WindowNumber,
				ContentHash: w.ContentHash, Fixtures: len(w.Fixtures), Aggregates: len(w.Aggregates),
			})
		}
	}
	return view
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	payload := map[string]any{
		"project": s.project, "schemas": stage.SupportedSchemas,
		"last_run": s.lastRun, "preview": s.lastPreview, "preview_comparison": s.lastCmp,
		"three_path_verification": s.lastVerify, "last_invalidation": s.lastInvalid,
		"algorithm_version": stage.AlgorithmVersion,
		"sampling_exact":    stage.ExactSampling(), "sampling_preview": stage.PreviewSampling(),
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	bundle, err := stage.GenerateSampleBundle(s.seed)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	if err := bundle.Write(s.samplesDir); err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	p, err := stage.LoadProject(bundle.Files["v3.json"])
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	s.project = p
	s.clearResultsLocked()
	s.flash = "已按固定种子重新生成 v1/v2/v3 与 truth.json。"
	s.flashError = ""
	s.mu.Unlock()
	if err := s.persistProject(p); err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(filepath.Join(s.samplesDir, "v3.json"))
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	p, err := stage.LoadProject(data)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	s.project = p
	s.clearResultsLocked()
	s.flash = "已恢复到样本 v3 初始状态。"
	s.flashError = ""
	s.mu.Unlock()
	if err := s.persistProject(p); err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	p := s.currentProject()
	if p == nil {
		s.failAndRedirect(w, r, fmt.Errorf("no project loaded"))
		return
	}
	preview, err := stage.RunPreview(r.Context(), p, 2, 500)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	exact, err := stage.RunProject(r.Context(), p, stage.RunOptions{Workers: 4, Sampling: stage.ExactSampling()})
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	cmp, err := stage.ComparePreviewWithExact(preview.Result, exact, stage.Tolerance{})
	if err != nil {
		s.mu.Lock()
		s.lastPreview = &preview
		s.lastCmp = &cmp
		s.mu.Unlock()
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	if s.project == p {
		s.lastPreview = &preview
		s.lastCmp = &cmp
		s.lastRun = &exact
		s.flash = fmt.Sprintf("低密度预览完成：%d 点，%d ms，在 %d ms 预算内=%v。", preview.Statistics.PointCount, preview.ElapsedMS, preview.BudgetMS, preview.WithinBudget)
		s.flashError = ""
	}
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleExact(w http.ResponseWriter, r *http.Request) {
	p := s.currentProject()
	if p == nil {
		s.failAndRedirect(w, r, fmt.Errorf("no project loaded"))
		return
	}
	s.mu.Lock()
	previous := s.previousRun
	s.mu.Unlock()
	run, err := stage.RunProject(r.Context(), p, stage.RunOptions{
		Workers: 4, Sampling: stage.ExactSampling(),
		Store:    stage.NewCheckpointStore(filepath.Join(s.runtimeDir, "checkpoints")),
		Previous: previous,
	})
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	if s.project == p {
		s.lastRun = &run
		s.previousRun = nil
		s.flash = fmt.Sprintf("全精度预演完成：提交 %v，复用 %v。", run.Committed, run.Reused)
		s.flashError = ""
	}
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	p := s.currentProject()
	if p == nil {
		s.failAndRedirect(w, r, fmt.Errorf("no project loaded"))
		return
	}
	report, err := stage.VerifyThreePaths(r.Context(), p, filepath.Join(s.runtimeDir, "verify"), stage.PathVerifyOptions{
		Sampling: stage.ExactSampling(), LiveWorkers: 1, FullWorkers: 4, ResumeStopAfter: 2,
	})
	if err != nil {
		s.mu.Lock()
		s.lastVerify = &report
		s.mu.Unlock()
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	if s.project == p {
		s.lastVerify = &report
		s.flash = "实时、中断续演、一次性全量三条路径逐点一致。"
		s.flashError = ""
	}
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleFixture(w http.ResponseWriter, r *http.Request) {
	p := s.currentProject()
	next, id, err := projectWithFixtureForm(p, r)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	next.Revision = p.Revision + 1
	report, err := stage.CompareProjects(p, next)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	s.project = next
	if s.previousRun == nil {
		s.previousRun = p
	}
	s.lastInvalid = &report
	s.lastRun = nil
	s.lastPreview = nil
	s.lastCmp = nil
	s.lastVerify = nil
	s.flash = fmt.Sprintf("灯具 %s 已编辑；精确失效窗口 %v，复用窗口 %v。", id, invalidationNumbers(report.Invalidated), report.Reused)
	s.flashError = ""
	s.mu.Unlock()
	if err := s.persistProject(next); err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLivePreview(w http.ResponseWriter, r *http.Request) {
	p := s.currentProject()
	next, fixtureID, err := projectWithFixtureForm(p, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	preview, err := stage.RunPreview(r.Context(), next, 2, 250)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	var visualPoints []visualPoint
	fixtures := make(map[string]visualFixture)
	for _, window := range preview.Result.Windows {
		for _, sample := range window.Fixtures {
			fixtures[sample.FixtureID] = visualFixture{
				ID: sample.FixtureID,
				X:  sample.Pose.PositionMM[0], Y: sample.Pose.PositionMM[1], Z: sample.Pose.PositionMM[2],
			}
			for _, point := range sample.Points {
				visualPoints = append(visualPoints, visualPoint{
					FixtureID: sample.FixtureID, WindowNumber: window.WindowNumber,
					X: point.CoordinateMM[0], Y: point.CoordinateMM[1], Z: point.CoordinateMM[2],
					Intensity: point.Intensity, Uncertain: point.Uncertain,
				})
			}
		}
	}
	visualFixtures := make([]visualFixture, 0, len(fixtures))
	for _, fixture := range fixtures {
		visualFixtures = append(visualFixtures, fixture)
	}
	sort.Slice(visualFixtures, func(i, j int) bool { return visualFixtures[i].ID < visualFixtures[j].ID })
	payload := struct {
		OK           bool                    `json:"ok"`
		FixtureID    string                  `json:"fixture_id"`
		Sampling     stage.SamplingConfig    `json:"sampling"`
		ElapsedMS    int64                   `json:"elapsed_ms"`
		BudgetMS     int64                   `json:"budget_ms"`
		WithinBudget bool                    `json:"within_budget"`
		Statistics   stage.PreviewStatistics `json:"statistics"`
		Decision     string                  `json:"decision"`
		VisualPoints []visualPoint           `json:"visual_points"`
		Fixtures     []visualFixture         `json:"fixtures"`
	}{
		OK: true, FixtureID: fixtureID, Sampling: preview.Sampling,
		ElapsedMS: preview.ElapsedMS, BudgetMS: preview.BudgetMS,
		WithinBudget: preview.WithinBudget, Statistics: preview.Statistics,
		Decision: preview.Decision, VisualPoints: visualPoints, Fixtures: visualFixtures,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

func projectWithFixtureForm(p *stage.Project, r *http.Request) (*stage.Project, string, error) {
	if err := r.ParseForm(); err != nil {
		return nil, "", err
	}
	next, err := cloneProject(p)
	if err != nil {
		return nil, "", err
	}
	id := r.FormValue("id")
	idx := -1
	for i := range next.Fixtures {
		if next.Fixtures[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, "", fmt.Errorf("unknown fixture %q", id)
	}
	f := &next.Fixtures[idx]
	if v, ok, err := formInt(r, "pan_mdeg"); err != nil {
		return nil, "", err
	} else if ok {
		f.PanMicroDeg, _, err = stage.MilliDegToMicroDeg(v)
		if err != nil {
			return nil, "", err
		}
	}
	if v, ok, err := formInt(r, "tilt_mdeg"); err != nil {
		return nil, "", err
	} else if ok {
		f.TiltMicroDeg, _, err = stage.MilliDegToMicroDeg(v)
		if err != nil {
			return nil, "", err
		}
	}
	if v, ok, err := formInt(r, "inner_mdeg"); err != nil {
		return nil, "", err
	} else if ok {
		f.InnerHalfMicroDeg, _, err = stage.MilliDegToMicroDeg(v)
		if err != nil {
			return nil, "", err
		}
	}
	if v, ok, err := formInt(r, "outer_mdeg"); err != nil {
		return nil, "", err
	} else if ok {
		f.OuterHalfMicroDeg, _, err = stage.MilliDegToMicroDeg(v)
		if err != nil {
			return nil, "", err
		}
	}
	if v, ok, err := formInt(r, "level_permyriad"); err != nil {
		return nil, "", err
	} else if ok {
		f.LevelPermyriad = v
	}
	if v, ok, err := formInt(r, "cct_kelvin"); err != nil {
		return nil, "", err
	} else if ok {
		f.CCTKelvin = v
	}
	for ch, name := range []string{"r", "g", "b"} {
		if v, ok, err := formInt(r, name); err != nil {
			return nil, "", err
		} else if ok {
			f.RGBPermyriad[ch] = v
		}
	}
	next.Revision = p.Revision + 1
	if err := stage.ValidateProject(next); err != nil {
		return nil, "", err
	}
	return next, id, nil
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	payload := map[string]any{"ok": false, "error": err.Error()}
	if domain := stage.AsError(err); domain != nil {
		payload["code"] = domain.Code
		payload["field"] = domain.Field
		payload["fixture_id"] = domain.FixtureID
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleCueColor(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	p := s.currentProject()
	next, err := cloneProject(p)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	cueID := r.FormValue("cue_id")
	idx := -1
	for i := range next.Cues {
		if next.Cues[i].ID == cueID {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.failAndRedirect(w, r, fmt.Errorf("unknown cue %q", cueID))
		return
	}
	if next.Cues[idx].Kind != stage.CueColor {
		s.failAndRedirect(w, r, fmt.Errorf("cue %q is not a color cue", cueID))
		return
	}
	for i, name := range []string{"from_r", "from_g", "from_b", "to_r", "to_g", "to_b"} {
		v, ok, err := formInt(r, name)
		if err != nil {
			s.failAndRedirect(w, r, err)
			return
		}
		if !ok {
			continue
		}
		if i < 3 {
			next.Cues[idx].FromRGB[i] = v
		} else {
			next.Cues[idx].ToRGB[i-3] = v
		}
	}
	next.Revision = p.Revision + 1
	report, err := stage.CompareProjects(p, next)
	if err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	s.mu.Lock()
	s.project = next
	if s.previousRun == nil {
		s.previousRun = p
	}
	s.lastInvalid = &report
	s.lastRun = nil
	s.lastPreview = nil
	s.lastCmp = nil
	s.lastVerify = nil
	s.flash = fmt.Sprintf("颜色渐变 %s 已编辑；失效窗口 %v，复用窗口 %v。", cueID, invalidationNumbers(report.Invalidated), report.Reused)
	s.flashError = ""
	s.mu.Unlock()
	if err := s.persistProject(next); err != nil {
		s.failAndRedirect(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) currentProject() *stage.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.project
}

func (s *Server) clearResultsLocked() {
	s.lastRun = nil
	s.lastPreview = nil
	s.lastCmp = nil
	s.lastVerify = nil
	s.lastInvalid = nil
	s.previousRun = nil
}

func (s *Server) failAndRedirect(w http.ResponseWriter, r *http.Request, err error) {
	s.mu.Lock()
	s.flashError = err.Error()
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) persistProject(p *stage.Project) error {
	res, err := stage.ExportProject(p, stage.SchemaV3)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.currentPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".current-v3-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(res.Data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.currentPath)
}

func cloneProject(p *stage.Project) (*stage.Project, error) {
	if p == nil {
		return nil, fmt.Errorf("no project loaded")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out stage.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func formInt(r *http.Request, name string) (int64, bool, error) {
	raw, ok := r.Form[name]
	if !ok || len(raw) == 0 || strings.TrimSpace(raw[0]) == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw[0]), 10, 64)
	if err != nil {
		return 0, false, &stage.Error{Code: stage.CodeValue, Message: "invalid integer form value", Field: name, Actual: raw[0]}
	}
	return v, true, nil
}

func invalidationNumbers(in []stage.Invalidation) []int {
	out := make([]int, 0, len(in))
	for _, item := range in {
		out = append(out, item.WindowNumber)
	}
	return out
}

var pageTemplate = template.Must(template.New("index").Funcs(template.FuncMap{
	"mdeg": func(v int64) int64 { return v / 1000 },
	"short": func(s string) string {
		if len(s) > 16 {
			return s[:16] + "…"
		}
		return s
	},
	"json": func(v any) string {
		data, _ := json.MarshalIndent(v, "", "  ")
		return string(data)
	},
	"kind": func(v stage.CueKind) string { return string(v) },
}).Parse(indexHTML))

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { color-scheme: dark; --bg:#0d1117; --panel:#161b22; --line:#30363d; --text:#e6edf3; --muted:#8b949e; --accent:#58a6ff; --ok:#3fb950; --bad:#f85149; }
* { box-sizing: border-box; }
body { margin:0; background:radial-gradient(circle at 30% 0%, #1b263b, var(--bg) 45%); color:var(--text); font:14px/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
main { max-width:1320px; margin:0 auto; padding:28px 22px 60px; }
h1 { margin:0 0 4px; font-size:27px; letter-spacing:.04em; }
h2 { font-size:16px; margin:0 0 12px; color:#c9d1d9; letter-spacing:.04em; text-transform:uppercase; }
.sub { color:var(--muted); margin-bottom:20px; }
.grid { display:grid; grid-template-columns:repeat(12,1fr); gap:14px; }
.panel { background:rgba(22,27,34,.92); border:1px solid var(--line); border-radius:10px; padding:16px; box-shadow:0 8px 28px #0004; }
.span4 { grid-column:span 4; } .span6 { grid-column:span 6; } .span8 { grid-column:span 8; } .span12 { grid-column:span 12; }
.flash,.error { padding:10px 12px; border-radius:8px; margin-bottom:14px; border:1px solid; }
.flash { border-color:#238636; background:#12261a; color:#b7f7c8; }
.error { border-color:#8e1519; background:#2a1215; color:#ffb3b8; white-space:pre-wrap; }
.actions { display:flex; flex-wrap:wrap; gap:8px; margin-bottom:14px; }
button { border:1px solid #388bfd66; background:#1f6feb22; color:#cfe8ff; border-radius:7px; padding:7px 11px; cursor:pointer; font:inherit; }
button:hover { background:#1f6feb44; }
input { width:86px; background:#0b1016; color:var(--text); border:1px solid #3d444d; border-radius:5px; padding:4px 6px; font:inherit; }
table { width:100%; border-collapse:collapse; }
th,td { border-bottom:1px solid #252b33; padding:6px 7px; text-align:left; vertical-align:top; }
th { color:#9fb0c3; font-weight:500; font-size:12px; text-transform:uppercase; letter-spacing:.05em; }
.muted { color:var(--muted); } .ok { color:var(--ok); } .bad { color:var(--bad); }
.mono { font-variant-numeric:tabular-nums; }
.badge { display:inline-block; border:1px solid var(--line); border-radius:20px; padding:1px 8px; margin:2px 4px 2px 0; color:#b7c7d8; background:#0d1117; }
.inline { display:flex; align-items:center; gap:6px; flex-wrap:wrap; }
.inline input { width:72px; }
.live-status { min-width:230px; font-size:12px; }
#beam-preview { width:100%; height:auto; background:#070b10; border:1px solid #252b33; border-radius:8px; }
code { color:#9ecbff; }
details { margin-top:8px; } summary { cursor:pointer; color:var(--accent); }
@media (max-width:900px){ .span4,.span6,.span8 { grid-column:span 12; } }
</style>
</head>
<body><main>
<h1>舞台灯具阵列 · 预演诊断台</h1>
<div class="sub">确定性领域内核 / 三 schema 版本 / 整数单位链 / 原子检查点 / 三路径一致性</div>
{{if .Flash}}<div class="flash">{{.Flash}}</div>{{end}}
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
<div class="actions">
<form method="post" action="/generate"><button>重新生成固定种子样本</button></form>
<form method="post" action="/preview"><button>低密度交互预览 + 精确比对</button></form>
<form method="post" action="/exact"><button>全精度预演（复用检查点）</button></form>
<form method="post" action="/verify"><button>验证实时 / 续演 / 全量三路径</button></form>
<form method="post" action="/reset"><button>恢复样本初始节目</button></form>
</div>
<div class="grid">
<section class="panel span12">
<h2>交互式低密度预览画面</h2>
<canvas id="beam-preview" width="1200" height="280"></canvas>
<div id="preview-caption" class="muted">拖动任一行灯具的姿态、锥角或色彩参数；本地页面会自动请求低密度预览并在此叠加各采样窗口的俯视采样点。</div>
</section>
<section class="panel span4">
<h2>工程</h2>
{{with .Project}}
<table>
<tr><th>项目</th><td>{{.ProjectID}}</td></tr>
<tr><th>源版本</th><td><code>{{.SourceVersion}}</code></td></tr>
<tr><th>参数版本</th><td>{{.ParameterVersion}}</td></tr>
<tr><th>修订</th><td>{{.Revision}}</td></tr>
<tr><th>种子</th><td>{{.Seed}}</td></tr>
<tr><th>灯具 / 窗口</th><td>{{len .Fixtures}} / {{len .Timeline.Windows}}</td></tr>
<tr><th>转换痕迹</th><td>{{$.TraceCount}}（默认补全 {{$.DefaultCount}}）</td></tr>
<tr><th>算法</th><td><code>cone-kernel-v1</code></td></tr>
</table>
<div style="margin-top:10px">
{{range $.SchemaVersions}}<span class="badge">{{.}}</span>{{end}}
</div>
{{end}}
</section>
<section class="panel span8">
<h2>灯具阵列</h2>
<table>
<tr><th>ID</th><th>位置 mm</th><th>姿态 mdeg</th><th>锥角 mdeg</th><th>亮度 / CCT / RGB</th><th>编辑</th></tr>
{{range .Project.Fixtures}}
<tr>
<td><strong>{{.ID}}</strong><div class="muted">{{.Group}}</div></td>
<td class="mono">{{index .PositionUM 0}}, {{index .PositionUM 1}}, {{index .PositionUM 2}}</td>
<td class="mono">{{mdeg .PanMicroDeg}}, {{mdeg .TiltMicroDeg}}</td>
<td class="mono">{{mdeg .InnerHalfMicroDeg}} / {{mdeg .OuterHalfMicroDeg}}</td>
<td class="mono">{{.LevelPermyriad}} / {{.CCTKelvin}}K / {{index .RGBPermyriad 0}},{{index .RGBPermyriad 1}},{{index .RGBPermyriad 2}}</td>
<td>
<form method="post" action="/fixture" class="inline" data-fixture="{{.ID}}">
<input type="hidden" name="id" value="{{.ID}}">
<input name="pan_mdeg" value="{{mdeg .PanMicroDeg}}" title="pan mdeg">
<input name="tilt_mdeg" value="{{mdeg .TiltMicroDeg}}" title="tilt mdeg">
<input name="inner_mdeg" value="{{mdeg .InnerHalfMicroDeg}}" title="inner">
<input name="outer_mdeg" value="{{mdeg .OuterHalfMicroDeg}}" title="outer">
<input name="level_permyriad" value="{{.LevelPermyriad}}" title="level">
<input name="cct_kelvin" value="{{.CCTKelvin}}" title="cct">
<input name="r" value="{{index .RGBPermyriad 0}}" title="r">
<input name="g" value="{{index .RGBPermyriad 1}}" title="g">
<input name="b" value="{{index .RGBPermyriad 2}}" title="b">
<span class="live-status muted">拖动参数触发低密度预览</span>
<button>应用</button>
</form>
</td>
</tr>
{{end}}
</table>
</section>

<section class="panel span6">
<h2>节目单</h2>
<table><tr><th>指令</th><th>灯具</th><th>时间 ms</th><th>参数</th></tr>
{{range .Project.Cues}}
<tr><td><strong>{{.ID}}</strong><div class="muted">{{kind .Kind}}</div></td><td>{{.FixtureID}}</td><td class="mono">{{.StartMS}} → {{.EndMS}}</td>
<td>
{{if eq (kind .Kind) "fade"}}level {{.FromLevel}} → {{.ToLevel}}
{{else if eq (kind .Kind) "sweep"}}pan {{mdeg .FromPan}} → {{mdeg .ToPan}} mdeg<br>tilt {{mdeg .FromTilt}} → {{mdeg .ToTilt}} mdeg
{{else}}RGB {{index .FromRGB 0}},{{index .FromRGB 1}},{{index .FromRGB 2}} → {{index .ToRGB 0}},{{index .ToRGB 1}},{{index .ToRGB 2}}
<form method="post" action="/cue-color" class="inline" style="margin-top:6px">
<input type="hidden" name="cue_id" value="{{.ID}}">
<input name="from_r" value="{{index .FromRGB 0}}"><input name="from_g" value="{{index .FromRGB 1}}"><input name="from_b" value="{{index .FromRGB 2}}">
→ <input name="to_r" value="{{index .ToRGB 0}}"><input name="to_g" value="{{index .ToRGB 1}}"><input name="to_b" value="{{index .ToRGB 2}}">
<button>改渐变</button>
</form>
{{end}}
</td></tr>
{{end}}
</table>
</section>

<section class="panel span6">
<h2>采样窗口与最近提交</h2>
{{if .LastRun}}
<table><tr><th>#</th><th>时间</th><th>点 / 不确定</th><th>聚合格</th><th>灯具摘要</th></tr>
{{range .LastRun.Windows}}
<tr><td>{{.WindowNumber}}</td><td>{{index .SampleTimes 0}}</td><td>{{.UncertainCount}} unc.</td><td>{{len .Aggregates}}</td>
<td>{{range .Fixtures}}<span class="badge">{{.FixtureID}}:{{len .Points}}p/{{.TotalIntensity}}</span>{{end}}</td></tr>
{{end}}
</table>
<div class="muted" style="margin-top:8px">提交 {{json .LastRun.Committed}} · 复用 {{json .LastRun.Reused}} · 序列 {{json .LastRun.CommitSequence}}</div>
{{else}}
<div class="muted">尚未运行全精度预演。</div>
{{end}}
{{with .Preview}}
<details open><summary>低密度预览证据</summary>
<table>
<tr><th>点数 / 不确定</th><td>{{.Statistics.PointCount}} / {{.Statistics.UncertainCount}}</td></tr>
<tr><th>实际密度</th><td>{{.Sampling.Density}} 环 × {{.Sampling.AngularSegments}} 方位</td></tr>
<tr><th>累积精度</th><td>{{.Sampling.AccumulationBits}} bit / {{.Sampling.CellMM}} mm 格</td></tr>
<tr><th>延迟</th><td>{{.ElapsedMS}} ms / 上限 {{.BudgetMS}} ms <span class="{{if .WithinBudget}}ok{{else}}bad{{end}}">within={{.WithinBudget}}</span></td></tr>
<tr><th>判定依据</th><td>{{.Decision}}</td></tr>
</table>
</details>
{{end}}
{{with .Comparison}}<div class="muted" style="margin-top:8px">预览/精确共享点比对：{{.Compared}} 点，OK=<span class="{{if .OK}}ok{{else}}bad{{end}}">{{.OK}}</span></div>{{end}}
</section>

{{if .Invalidation}}
<section class="panel span6">
<h2>最近编辑失效清单</h2>
<table><tr><th>窗口</th><th>原因</th><th>灯具 / 指令</th><th>下游</th></tr>
{{range .Invalidation.Invalidated}}
<tr><td>{{.WindowNumber}}</td><td>{{range .Reasons}}{{.}}<br>{{end}}</td><td>{{range .FixtureIDs}}{{.}} {{end}}{{range .CueIDs}}[{{.}}]{{end}}</td><td class="muted">{{range .Downstream}}{{.}}<br>{{end}}</td></tr>
{{end}}
</table>
<div class="muted">未受影响并计划复用：{{json .Invalidation.Reused}}</div>
</section>
{{end}}

{{if .Verify}}
<section class="panel span6">
<h2>三路径一致性</h2>
<table><tr><th>路径</th><th>窗口</th><th>提交序列</th><th>结果哈希</th></tr>
{{range .Verify.Paths}}
<tr><td>{{.Name}}</td><td>{{.Windows}}</td><td>{{json .CommitSequence}}</td><td><code title="{{.ResultHash}}">{{short .ResultHash}}</code></td></tr>
{{end}}
</table>
<p class="{{if .Verify.OK}}ok{{else}}bad{{end}}">逐采样点光强、色度、不确定标记与提交序列：{{if .Verify.OK}}一致{{else}}不一致{{end}}</p>
</section>
{{end}}

<section class="panel span12">
<h2>检查点与诊断</h2>
{{if .CheckpointError}}<div class="error">{{.CheckpointError}}</div>{{end}}
{{if .Checkpoints}}
<table><tr><th>文件</th><th>窗口</th><th>内容哈希</th><th>灯具摘要</th><th>聚合格</th></tr>
{{range .Checkpoints}}<tr><td>{{.Name}}</td><td>{{.Window}}</td><td><code title="{{.ContentHash}}">{{short .ContentHash}}</code></td><td>{{.Fixtures}}</td><td>{{.Aggregates}}</td></tr>{{end}}
</table>
{{else}}<div class="muted">checkpoint 目录为空；先执行全精度预演。</div>{{end}}
<details style="margin-top:12px"><summary>查看 JSON 诊断快照</summary><pre style="white-space:pre-wrap; color:#9fb0c3">{{json .Project}}</pre></details>
<div class="muted" style="margin-top:12px">samples={{.SamplesDir}} · runtime={{.RuntimeDir}} · 仅监听回环地址</div>
</section>
</div>
<script>
document.querySelectorAll('form[data-fixture]').forEach(function (form) {
  var timer = 0;
  form.querySelectorAll('input').forEach(function (input) {
    input.addEventListener('input', function () {
      clearTimeout(timer);
      timer = setTimeout(function () {
        var status = form.querySelector('.live-status');
        status.textContent = '低密度预览中…';
        fetch('/api/live-preview', { method: 'POST', body: new FormData(form) })
          .then(function (response) {
            return response.json().then(function (data) { return { ok: response.ok, data: data }; });
          })
          .then(function (result) {
            if (!result.ok) {
              status.textContent = '预览错误: ' + (result.data.error || 'unknown');
              return;
            }
            var data = result.data;
            status.textContent = data.statistics.point_count + ' 点 · ' +
              data.elapsed_ms + ' ms / ' + data.budget_ms + ' ms · ' +
              (data.within_budget ? '预算内' : '超预算') + ' · 密度 ' +
              data.sampling.density + '×' + data.sampling.angular_segments + ' · ' +
              data.sampling.accumulation_bits + ' bit';
            drawBeamPreview(data);
          })
          .catch(function (error) { status.textContent = '预览错误: ' + error; });
      }, 160);
    });
  });
});

function drawBeamPreview(data) {
  var canvas = document.getElementById('beam-preview');
  if (!canvas || !canvas.getContext) { return; }
  var ctx = canvas.getContext('2d');
  var width = canvas.width, height = canvas.height, pad = 26;
  ctx.clearRect(0, 0, width, height);
  ctx.fillStyle = '#070b10';
  ctx.fillRect(0, 0, width, height);
  var points = data.visual_points || [];
  var fixtures = data.fixtures || [];
  if (!points.length) { return; }
  var minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  function extend(x, y) {
    minX = Math.min(minX, x); maxX = Math.max(maxX, x);
    minY = Math.min(minY, y); maxY = Math.max(maxY, y);
  }
  points.forEach(function (p) { extend(p.x, p.y); });
  fixtures.forEach(function (f) { extend(f.x, f.y); });
  var spanX = Math.max(1, maxX - minX), spanY = Math.max(1, maxY - minY);
  var scale = Math.min((width - pad * 2) / spanX, (height - pad * 2) / spanY);
  function project(x, y) {
    return [pad + (x - minX) * scale, height - pad - (y - minY) * scale];
  }
  ctx.strokeStyle = '#18212c';
  ctx.lineWidth = 1;
  for (var gx = 0; gx <= 12; gx++) {
    var px = pad + gx * (width - pad * 2) / 12;
    ctx.beginPath(); ctx.moveTo(px, pad); ctx.lineTo(px, height - pad); ctx.stroke();
  }
  for (var gy = 0; gy <= 5; gy++) {
    var py = pad + gy * (height - pad * 2) / 5;
    ctx.beginPath(); ctx.moveTo(pad, py); ctx.lineTo(width - pad, py); ctx.stroke();
  }
  points.forEach(function (p) {
    var pos = project(p.x, p.y);
    ctx.globalAlpha = 0.18 + 0.72 * Math.min(1, p.intensity / 10000);
    ctx.fillStyle = p.uncertain ? '#ffb347' : '#58a6ff';
    ctx.beginPath();
    ctx.arc(pos[0], pos[1], p.uncertain ? 2.4 : 2.0, 0, Math.PI * 2);
    ctx.fill();
  });
  ctx.globalAlpha = 1;
  fixtures.forEach(function (f) {
    var pos = project(f.x, f.y);
    ctx.fillStyle = '#3fb950';
    ctx.fillRect(pos[0] - 4, pos[1] - 4, 8, 8);
    ctx.fillStyle = '#b7f7c8';
    ctx.font = '12px ui-monospace, monospace';
    ctx.fillText(f.id, pos[0] + 7, pos[1] - 7);
  });
  var caption = document.getElementById('preview-caption');
  if (caption) {
    caption.textContent = '白色/绿色为灯具位置，蓝点为内锥确定采样，橙点为内外锥过渡带；叠加 ' +
      (data.statistics.window_count || 0) + ' 个窗口，密度 ' + data.sampling.density +
      ' 环，聚合 ' + data.sampling.accumulation_bits + ' bit，延迟 ' + data.elapsed_ms + ' ms。';
  }
}
</script>
</main></body></html>`

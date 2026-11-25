package server

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/engine"
	"ms-workbench/internal/pipeline"
	"ms-workbench/internal/samples"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/store"
)

//go:embed web/*
var webFiles embed.FS

type Options struct {
	DBDir  string
	Seed   uint64
	Config engine.Config
}

type Server struct {
	mu       sync.Mutex
	packages []*samples.Package
	byKey    map[string]*samples.Package
	opts     Options
	session  *Session
}

type Session struct {
	mu      sync.Mutex
	pkg     *samples.Package
	scene   *scene.Scene
	engine  *engine.Engine
	cp      *store.Checkpoint
	dbPath  string
	version int
	mode    engine.RunMode
	running bool
	lastErr string
	verify  *engine.VerificationReport
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(opts Options) (*Server, error) {
	if opts.DBDir == "" {
		opts.DBDir = "artifacts"
	}
	if opts.Config == (engine.Config{}) {
		opts.Config = engine.DefaultConfig()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	pkgs, err := samples.DefaultPackages(opts.Seed)
	if err != nil {
		return nil, err
	}
	s := &Server{packages: pkgs, opts: opts, byKey: map[string]*samples.Package{}}
	for _, p := range pkgs {
		s.byKey[p.Key] = p
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/scenes", s.handleScenes)
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/session/pause", s.handlePause)
	mux.HandleFunc("/api/session/resume", s.handleResume)
	mux.HandleFunc("/api/session/retry", s.handleRetry)
	mux.HandleFunc("/api/session/settings", s.handleSettings)
	mux.HandleFunc("/api/session/band", s.handleBand)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/image", s.handleImage)
	mux.HandleFunc("/api/histogram", s.handleHistogram)
	mux.HandleFunc("/api/export", s.handleExport)
	webFS, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(webFS)))
	return logMiddleware(mux)
}

func (s *Server) handleScenes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	type sceneInfo struct {
		Key      string   `json:"key"`
		Name     string   `json:"name"`
		Versions []int    `json:"versions"`
		Width    int      `json:"width"`
		Height   int      `json:"height"`
		Bands    []string `json:"bands"`
		Peak     float64  `json:"truthPeakRadiance"`
		Tol      float64  `json:"patchTolerance"`
	}
	out := make([]sceneInfo, 0, len(s.packages))
	for _, p := range s.packages {
		out = append(out, sceneInfo{
			Key: p.Key, Name: p.Scene.Name, Versions: []int{1, 2, 3},
			Width: p.Scene.Width, Height: p.Scene.Height,
			Bands: scene.SortedBandIDs(p.Scene.Bands),
			Peak:  p.Scene.Truth.PeakRadiance, Tol: p.Scene.Truth.PatchTolerance,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenes": out})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		sess := s.currentSession()
		if sess == nil {
			writeJSON(w, http.StatusOK, map[string]any{"session": nil})
			return
		}
		writeJSON(w, http.StatusOK, sess.state(s.opts))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Scene  string `json:"scene"`
		Schema int    `json:"schemaVersion"`
		Mode   string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apperr.New(apperr.EPARAM, "invalid request: %v", err))
		return
	}
	pkg := s.byKey[req.Scene]
	if pkg == nil {
		writeError(w, apperr.New(apperr.EPARAM, "unknown scene %q", req.Scene))
		return
	}
	if req.Schema < 1 || req.Schema > 3 {
		writeError(w, &apperr.Error{Code: apperr.EVER, SourceVersion: req.Schema, Message: "unsupported schema version"})
		return
	}
	mode := engine.RunMode(req.Mode)
	if mode == "" {
		mode = engine.ModeResume
	}
	if mode != engine.ModeLive && mode != engine.ModeResume && mode != engine.ModeFull {
		writeError(w, apperr.New(apperr.EPARAM, "invalid run mode %q", req.Mode))
		return
	}
	s.mu.Lock()
	if s.session != nil {
		s.session.stop()
		s.session = nil
	}
	session, err := startSession(pkg, req.Schema, mode, s.opts)
	if err != nil {
		s.mu.Unlock()
		writeError(w, err)
		return
	}
	s.session = session
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, session.state(s.opts))
}

func (s *Server) currentSession() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

func startSession(pkg *samples.Package, version int, mode engine.RunMode, opts Options) (*Session, error) {
	data, ok := pkg.Files[version]
	if !ok {
		return nil, &apperr.Error{Code: apperr.EVER, SourceVersion: version, Message: "sample version is unavailable"}
	}
	sc, err := scene.Decode(data)
	if err != nil {
		return nil, err
	}
	filename := fmt.Sprintf("%s-v%d.db", sanitize(pkg.Key), version)
	dbPath := filepath.Join(opts.DBDir, filename)
	cp, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	cfg := opts.Config
	if cfg.ChunkDelayMS == 0 {
		cfg.ChunkDelayMS = 18
	}
	eng, err := engine.New(sc, cp, cfg)
	if err != nil {
		cp.Close()
		return nil, err
	}
	sess := &Session{pkg: pkg, scene: sc, engine: eng, cp: cp, dbPath: dbPath, version: version, mode: mode}
	if err := sess.start(mode, -1); err != nil {
		cp.Close()
		return nil, err
	}
	return sess, nil
}

// start runs a full processing pass or a single segment retry. The caller must
// not hold sess.mu.
func (sess *Session) start(mode engine.RunMode, retrySegment int) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.running {
		return apperr.New(apperr.ESTATE, "session is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sess.running = true
	sess.lastErr = ""
	sess.mode = mode
	sess.cancel = cancel
	sess.done = done
	go func() {
		defer close(done)
		defer cancel()
		var err error
		if retrySegment >= 0 {
			_, err = sess.engine.RetrySegment(ctx, retrySegment)
		} else {
			_, err = sess.engine.Run(ctx, engine.RunOptions{Mode: mode})
		}
		sess.mu.Lock()
		sess.running = false
		if err != nil && ctx.Err() == nil {
			sess.lastErr = err.Error()
		}
		sess.mu.Unlock()
	}()
	return nil
}

func (sess *Session) stop() {
	sess.mu.Lock()
	running := sess.running
	cancel := sess.cancel
	done := sess.done
	sess.mu.Unlock()
	if running && cancel != nil {
		sess.engine.Pause()
		cancel()
		<-done
	}
	_ = sess.cp.Close()
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sess.engine.Pause()
	writeJSON(w, http.StatusOK, sess.state(s.opts))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sess.mu.Lock()
	running := sess.running
	sess.mu.Unlock()
	if running {
		sess.engine.Resume()
	} else if err := sess.start(engine.ModeResume, -1); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess.state(s.opts))
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	var req struct {
		Segment int `json:"segment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apperr.New(apperr.EPARAM, "invalid request: %v", err))
		return
	}
	if err := sess.start(engine.ModeResume, req.Segment); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sess.state(s.opts))
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	var req scene.RenderSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apperr.New(apperr.EPARAM, "invalid settings: %v", err))
		return
	}
	report, err := sess.engine.SetSettings(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invalidation": report, "state": sess.state(s.opts)})
}

func (s *Server) handleBand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	var req struct {
		Band    string `json:"band"`
		Variant int    `json:"variant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apperr.New(apperr.EPARAM, "invalid request: %v", err))
		return
	}
	sc := sess.engine.Scene()
	idx := sc.BandIndex(req.Band)
	if idx < 0 {
		writeError(w, apperr.New(apperr.EPARAM, "unknown band %q", req.Band))
		return
	}
	replacement := sc.Bands[idx].Clone()
	variantBand(&replacement, sc, sc.Settings, req.Variant)
	report, err := sess.engine.ReplaceBand(req.Band, replacement)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invalidation": report, "state": sess.state(s.opts)})
}

func variantBand(b *scene.Band, sc *scene.Scene, settings scene.RenderSettings, variant int) {
	cfg := engine.DefaultConfig()
	cols := (sc.Width + cfg.ChunkWidth - 1) / cfg.ChunkWidth
	total := cols * ((sc.Height + cfg.ChunkHeight - 1) / cfg.ChunkHeight)
	seg := ((variant % total) + total) % total
	cx := (seg % cols) * cfg.ChunkWidth
	cy := (seg / cols) * cfg.ChunkHeight
	maxCode := uint32(uint64(1)<<uint(sc.BitDepth) - 1)
	for y := cy + 3; y < cy+9 && y < sc.Height; y++ {
		for x := cx + 3; x < cx+9 && x < sc.Width; x++ {
			idx := y*sc.Width + x
			for e := range b.Exposures {
				if idx < len(b.Exposures[e].Codes) {
					c := uint32(b.Exposures[e].Codes[idx])
					c = (c + uint32(37+variant*13)) % (maxCode + 1)
					b.Exposures[e].Codes[idx] = uint16(c)
				}
			}
		}
	}
	for y := cy + 3; y < cy+9 && y < sc.Height; y++ {
		for x := cx + 3; x < cx+9 && x < sc.Width; x++ {
			idx := y*sc.Width + x
			b.Detail[idx] += 0.0004 * float64(1+variant%3)
		}
	}
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	sc := sess.engine.Scene()
	cfg := s.opts.Config
	report, err := engine.Verify(ctx, sc, cfg)
	if err != nil {
		writeError(w, err)
		return
	}
	sess.mu.Lock()
	sess.verify = report
	sess.mu.Unlock()
	writeJSON(w, http.StatusOK, report)
}

type stateResponse struct {
	View         engine.View                 `json:"view"`
	Running      bool                        `json:"running"`
	LastError    string                      `json:"lastError,omitempty"`
	DBPath       string                      `json:"dbPath"`
	SceneVersion int                         `json:"loadedSchemaVersion"`
	Truth        scene.Truth                 `json:"truth"`
	Patches      []pipeline.PatchMeasurement `json:"patches,omitempty"`
	Histograms   []Histogram                 `json:"histograms,omitempty"`
	Verification *engine.VerificationReport  `json:"verification,omitempty"`
	Dependencies []engine.Dependency         `json:"dependencies"`
}

func (sess *Session) state(opts Options) stateResponse {
	sess.mu.Lock()
	running := sess.running
	lastErr := sess.lastErr
	verify := sess.verify
	sess.mu.Unlock()
	view := sess.engine.View()
	out := stateResponse{
		View: view, Running: running, LastError: lastErr, DBPath: sess.dbPath,
		SceneVersion: sess.version, Truth: sess.scene.Truth, Verification: verify,
		Dependencies: sess.engine.Dependencies(),
	}
	if view.Committed == view.Total {
		comp := sess.engine.Composite()
		out.Patches = pipeline.MeasurePatches(sess.scene, comp.LinearRGB)
		out.Histograms = buildHistograms(comp)
	}
	return out
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	comp := sess.engine.Composite()
	if !comp.Complete {
		writeError(w, apperr.New(apperr.ESTATE, "composite is incomplete"))
		return
	}
	pngData, err := compositePNG(comp)
	if err != nil {
		writeError(w, apperr.New(apperr.EIO, "encode composite: %v", err))
		return
	}
	patches := pipeline.MeasurePatches(sess.scene, comp.LinearRGB)
	writeJSON(w, http.StatusOK, map[string]any{
		"sceneID":        sess.scene.SceneID,
		"sourceVersion":  sess.scene.SourceVersion,
		"width":          comp.Width,
		"height":         comp.Height,
		"bandIDs":        comp.BandIDs,
		"peakRadiance":   comp.PeakRadiance,
		"outOfGamut":     comp.OutOfGamut,
		"gamutBoundary":  comp.GamutBoundary,
		"commitSequence": comp.CommitSequence,
		"patches":        patches,
		"pngBase64":      base64.StdEncoding.EncodeToString(pngData),
	})
}

func (s *Server) handleHistogram(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	comp := sess.engine.Composite()
	writeJSON(w, http.StatusOK, map[string]any{"histograms": buildHistograms(comp)})
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := apperr.CodeOf(err)
	switch code {
	case apperr.ESTATE:
		status = http.StatusConflict
	case apperr.EIO:
		status = http.StatusInternalServerError
	case apperr.EVERSION, apperr.EHASH:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"error": err})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": apperr.New(apperr.EPARAM, "method not allowed")})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			_ = start
		}
	})
}

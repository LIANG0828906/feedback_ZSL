package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"lightprobe/internal/acceptance"
	"lightprobe/internal/checkpoint"
	"lightprobe/internal/diag"
	"lightprobe/internal/probe"
	"lightprobe/internal/scene"
)

type App struct {
	mu     sync.Mutex
	ctx    context.Context
	engine *probe.Engine
	dbPath string
}

type Bootstrap struct {
	Scene    *scene.Scene           `json:"scene"`
	Progress probe.Progress         `json:"progress"`
	Records  []probe.Record         `json:"records"`
	Pixels   []probe.PixelResult    `json:"pixels"`
	Queue    []probe.QueueEvent     `json:"queue"`
	Plan     *probe.MutationPlan    `json:"plan,omitempty"`
	Report   *probe.RecomputeReport `json:"report,omitempty"`
	DBPath   string                 `json:"db_path"`
	Hint     string                 `json:"hint"`
}

type Response struct {
	OK         bool                       `json:"ok"`
	Error      *diag.Error                `json:"error,omitempty"`
	Message    string                     `json:"message,omitempty"`
	Plan       *probe.MutationPlan        `json:"plan,omitempty"`
	Report     *probe.RecomputeReport     `json:"report,omitempty"`
	Bootstrap  *Bootstrap                 `json:"bootstrap,omitempty"`
	Acceptance *acceptance.Report         `json:"acceptance,omitempty"`
	Query      *probe.InterpolationResult `json:"query,omitempty"`
	Verify     *probe.PrecisionReport     `json:"verify,omitempty"`
}

func NewApp() *App {
	app := &App{ctx: context.Background(), dbPath: defaultDBPath()}
	_ = app.ensureEngine()
	return app
}

func (a *App) Startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	_ = a.ensureEngine()
}

func defaultDBPath() string {
	if path := os.Getenv("LIGHTPROBE_DB"); path != "" {
		return path
	}
	if info, err := os.Stat("artifacts"); err == nil && info.IsDir() {
		return filepath.Join("artifacts", "wails", "lightprobe.db")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "lightprobe-checkpoint.db"
	}
	return filepath.Join(cache, "lightprobe", "lightprobe.db")
}

func (a *App) ensureEngine() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine != nil {
		return nil
	}
	if a.dbPath != ":memory:" {
		_ = os.MkdirAll(filepath.Dir(a.dbPath), 0o755)
		_ = checkpoint.RemoveFiles(a.dbPath)
	}
	cfg := probe.DefaultConfig()
	cfg.CheckpointPath = a.dbPath
	engine, err := probe.NewEngine(scene.Generate(), cfg)
	if err != nil {
		return err
	}
	if a.dbPath != ":memory:" {
		if err := engine.EnableCheckpoint(a.ctx, a.dbPath); err != nil {
			a.dbPath = ""
		}
	}
	if err := engine.BuildAll(a.ctx); err != nil {
		return err
	}
	_ = engine.CommitRoom(a.ctx, "room-a")
	_ = engine.CommitRoom(a.ctx, "room-b")
	a.engine = engine
	return nil
}

func (a *App) getEngine() *probe.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

func (a *App) fail(err error) Response {
	response := Response{OK: false, Message: err.Error()}
	if de, ok := diag.AsError(err); ok {
		response.Error = de
		response.Message = de.Error()
	}
	return response
}

func (a *App) GetBootstrap() Bootstrap {
	if err := a.ensureEngine(); err != nil {
		return Bootstrap{Hint: "engine initialization failed: " + err.Error(), DBPath: a.dbPath}
	}
	engine := a.getEngine()
	return Bootstrap{
		Scene: engine.Scene(), Progress: engine.Progress(), Records: engine.Records(),
		Pixels: engine.PixelResults(), Queue: engine.MetricsEvents(),
		Plan: engine.LastPlan(), Report: engine.LastReport(), DBPath: a.dbPath,
		Hint: "room-a and room-b are committed/frozen; edit probe:room-c:0:0:0 to see exact invalidation.",
	}
}

func (a *App) EditProbe(probeID string, red, green, blue float64) Response {
	if err := a.ensureEngine(); err != nil {
		return a.fail(err)
	}
	engine := a.getEngine()
	plan, err := engine.EditRadiance(probeID, scene.Vec3{red, green, blue})
	if err != nil {
		return a.fail(err)
	}
	report, err := engine.RecomputeInvalid(context.Background())
	response := Response{OK: err == nil, Plan: plan, Report: report}
	if err != nil {
		response = a.fail(err)
		response.Plan = plan
		response.Report = report
	}
	bootstrap := a.GetBootstrap()
	response.Bootstrap = &bootstrap
	return response
}

func (a *App) ResetDemo() Response {
	a.mu.Lock()
	if a.engine != nil {
		_ = a.engine.Close()
	}
	a.engine = nil
	a.mu.Unlock()
	if err := a.ensureEngine(); err != nil {
		return a.fail(err)
	}
	bootstrap := a.GetBootstrap()
	return Response{OK: true, Message: "demo reset", Bootstrap: &bootstrap}
}

func (a *App) ApplyResolution(roomID string, x, y, z int) Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	plan, err := engine.SetResolution(roomID, scene.Resolution{X: x, Y: y, Z: z})
	if err != nil {
		return a.fail(err)
	}
	report, err := engine.RecomputeInvalid(context.Background())
	response := Response{OK: err == nil, Plan: plan, Report: report}
	if err != nil {
		response.Error, _ = diag.AsError(err)
		response.Message = err.Error()
	}
	bootstrap := a.GetBootstrap()
	response.Bootstrap = &bootstrap
	return response
}

func (a *App) RetryProbe(probeID string) Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	report, err := engine.RetryProbe(context.Background(), probeID)
	response := Response{OK: err == nil, Report: report}
	if err != nil {
		response = a.fail(err)
		response.Report = report
	}
	bootstrap := a.GetBootstrap()
	response.Bootstrap = &bootstrap
	return response
}

func (a *App) SimulateProbeFailure(probeID string) Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	if err := engine.InjectProbeFault(probeID, 1); err != nil {
		return a.fail(err)
	}
	report, err := engine.RetryProbe(context.Background(), probeID)
	response := Response{OK: false, Message: "injected failure consumed; call RetryProbe", Report: report}
	if err == nil {
		response.OK = true
		response.Message = "probe ran without consuming fault"
	} else if de, ok := diag.AsError(err); ok {
		response.Error = de
	}
	bootstrap := a.GetBootstrap()
	response.Bootstrap = &bootstrap
	return response
}

func (a *App) RetryRoom(roomID string) Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	report, err := engine.RetryRoom(context.Background(), roomID)
	response := Response{OK: err == nil, Report: report}
	if err != nil {
		response = a.fail(err)
		response.Report = report
	}
	bootstrap := a.GetBootstrap()
	response.Bootstrap = &bootstrap
	return response
}

func (a *App) CommitRoom(roomID string) Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	if err := engine.CommitRoom(context.Background(), roomID); err != nil {
		return a.fail(err)
	}
	bootstrap := a.GetBootstrap()
	return Response{OK: true, Message: "room committed and checkpointed", Bootstrap: &bootstrap}
}

func (a *App) QueryRadiance(queryID, roomID string, x, y, z float64) Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	result, err := engine.QueryRadiance(queryID, roomID, scene.Vec3{x, y, z})
	if err != nil {
		return a.fail(err)
	}
	return Response{OK: true, Query: &result}
}

func (a *App) VerifyTruth() Response {
	engine := a.getEngine()
	if engine == nil {
		return a.fail(diag.New(diag.CodeState, "engine not initialized"))
	}
	report, err := engine.VerifyTruth()
	response := Response{OK: err == nil, Verify: report}
	if err != nil {
		response = a.fail(err)
		response.Verify = report
	}
	return response
}

func (a *App) RunAcceptance() Response {
	report, err := acceptance.Run(acceptance.Options{WorkDir: filepath.Join("artifacts", "acceptance-ui"), MaxParallelism: 4})
	if err != nil {
		return a.fail(err)
	}
	return Response{OK: report.OK, Acceptance: report}
}

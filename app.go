//go:build wails

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"vectorlab/engine"
)

type App struct {
	mu         sync.Mutex
	baseCtx    context.Context
	scene      *engine.Scene
	v1         *engine.Scene
	v2         *engine.Scene
	truth      *engine.TruthDocument
	quality    engine.QualityDecision
	baseline   []*engine.RenderedFrame
	current    []*engine.RenderedFrame
	report     *engine.RunReport
	comparison *engine.DiffReport
	controller *engine.Controller
	running    bool
	runMode    string
	lastError  string
	dataDir    string
	dbPath     string
}

type FrameMeta struct {
	Index      int    `json:"index"`
	TimeMs     int64  `json:"timeMs"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Lines      int    `json:"lines"`
	Hash       string `json:"hash"`
	LegalState bool   `json:"legalState"`
}

type InitState struct {
	SceneID      string                 `json:"sceneId"`
	DataDir      string                 `json:"dataDir"`
	DBPath       string                 `json:"dbPath"`
	Versions     []string               `json:"versions"`
	Frames       []FrameMeta            `json:"frames"`
	CurrentIndex int                    `json:"currentIndex"`
	Quality      engine.QualityDecision `json:"quality"`
	StateMatrix  []engine.StateCell     `json:"stateMatrix"`
	Truth        string                 `json:"truth"`
	Message      string                 `json:"message"`
}

type StrokeInfo struct {
	ID        string           `json:"id"`
	Color     engine.Color     `json:"color"`
	WidthQ10  int64            `json:"widthQ10"`
	Join      engine.JoinKind  `json:"join"`
	HasFill   bool             `json:"hasFill"`
	HasStroke bool             `json:"hasStroke"`
	Blend     engine.BlendMode `json:"blend"`
}

type FrameView struct {
	Index          int                    `json:"index"`
	TimeMs         int64                  `json:"timeMs"`
	Width          int                    `json:"width"`
	Height         int                    `json:"height"`
	Scale          int64                  `json:"scale"`
	RenderBounds   engine.Rect            `json:"renderBounds"`
	GeometryBounds engine.Rect            `json:"geometryBounds"`
	RGBA           []byte                 `json:"rgba"`
	Heatmap        []byte                 `json:"heatmap"`
	Lines          []engine.FlattenedPath `json:"lines"`
	PathSummaries  []engine.PathCoverage  `json:"pathSummaries"`
	Strokes        []StrokeInfo           `json:"strokes"`
	Quality        engine.QualityDecision `json:"quality"`
	LineCount      int                    `json:"lineCount"`
	Hash           string                 `json:"hash"`
}

type UIStatus struct {
	Running        bool                   `json:"running"`
	Paused         bool                   `json:"paused"`
	Mode           string                 `json:"mode"`
	Error          string                 `json:"error,omitempty"`
	FrameCount     int                    `json:"frameCount"`
	CommitSequence []int                  `json:"commitSequence"`
	Comparison     *engine.DiffReport     `json:"comparison,omitempty"`
	StateMatrix    []engine.StateCell     `json:"stateMatrix"`
	Quality        engine.QualityDecision `json:"quality"`
	Snapshot       engine.SnapshotStatus  `json:"snapshot"`
	Partial        bool                   `json:"partial"`
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.baseCtx = ctx
}

// Initialize loads/generates the deterministic data set and renders a fine baseline.
func (a *App) Initialize(dataDir, dbPath string) (*InitState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if dataDir == "" {
		dataDir = "artifacts/sample"
	}
	if dbPath == "" {
		dbPath = "artifacts/ui/snapshots.db"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	if _, err := engine.GenerateSample(dataDir); err != nil {
		return nil, err
	}
	v1, err := engine.LoadSceneFile(filepath.Join(dataDir, "scene_v1.json"))
	if err != nil {
		return nil, err
	}
	v2, err := engine.LoadSceneFile(filepath.Join(dataDir, "scene_v2.json"))
	if err != nil {
		return nil, err
	}
	scene, err := engine.LoadSceneFile(filepath.Join(dataDir, "scene_v3.json"))
	if err != nil {
		return nil, err
	}
	if err := engine.SemanticDiff(scene, v1); err != nil {
		return nil, fmt.Errorf("v1 interoperability: %w", err)
	}
	if err := engine.SemanticDiff(scene, v2); err != nil {
		return nil, fmt.Errorf("v2 interoperability: %w", err)
	}
	truth, err := engine.LoadTruthFile(filepath.Join(dataDir, "truth.json"))
	if err != nil {
		return nil, err
	}
	q, err := engine.SelectQuality(scene, 512*1024*1024, 3)
	if err != nil {
		return nil, err
	}
	preview, err := engine.Run(a.baseCtx, scene, engine.RunOptions{
		Mode: engine.ModePreview, MaxWorkers: 3, BufferBytes: 8 * 1024 * 1024,
		BudgetBytes: 512 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		return nil, err
	}
	a.scene, a.v1, a.v2, a.truth = scene, v1, v2, truth
	a.quality = q
	a.dbPath = dbPath
	a.dataDir = dataDir
	a.baseline = cloneFrames(preview.Frames)
	a.current = cloneFrames(preview.Frames)
	a.report = preview
	state := &InitState{
		SceneID: scene.ID, DataDir: dataDir, DBPath: dbPath,
		Versions: []string{string(v1.SourceVersion), string(v2.SourceVersion), string(scene.SourceVersion)},
		Frames:   frameMetas(preview.Frames), CurrentIndex: 0, Quality: q,
		StateMatrix: preview.StateMatrix,
		Truth:       "loaded",
		Message:     "deterministic seed data loaded; baseline and current bitmaps are ready",
	}
	return state, nil
}

func (a *App) Frames() []FrameMeta {
	a.mu.Lock()
	defer a.mu.Unlock()
	return frameMetas(a.current)
}

// Frame returns the bitmap, contour/stroke overlay data and diff heatmap.
func (a *App) Frame(index int) (*FrameView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if index < 0 || index >= len(a.current) {
		return nil, &engine.Error{Code: engine.CodeSchema, Path: "frameIndex", Message: "index out of range"}
	}
	return a.frameViewLocked(index), nil
}

func (a *App) frameViewLocked(index int) *FrameView {
	f := a.current[index]
	base := f
	if index < len(a.baseline) {
		base = a.baseline[index]
	}
	strokes := make([]StrokeInfo, 0, len(a.scene.Paths))
	for _, p := range a.scene.Paths {
		strokes = append(strokes, StrokeInfo{
			ID: p.ID, Color: p.StrokeColor, WidthQ10: p.StrokeWidth,
			Join: p.LineJoin, HasFill: p.HasFill, HasStroke: p.HasStroke, Blend: p.Blend,
		})
	}
	summaries := make([]engine.PathCoverage, len(f.Paths))
	for i, p := range f.Paths {
		p.Coverage = nil
		p.FillCoverage = nil
		p.StrokeCoverage = nil
		summaries[i] = p
	}
	return &FrameView{
		Index: f.Index, TimeMs: f.TimeMs, Width: f.Width, Height: f.Height, Scale: a.scene.Scale,
		RenderBounds: f.RenderBounds, GeometryBounds: f.GeometryBounds,
		RGBA: append([]byte(nil), f.RGBA...), Heatmap: heatmap(base, f),
		Lines: f.Lines, PathSummaries: summaries, Strokes: strokes, Quality: f.Quality,
		LineCount: f.LineCount, Hash: f.ContentHash,
	}
}

// AdjustFrame recomputes one time slice only, then returns the updated overlay.
func (a *App) AdjustFrame(index int, tolerance int64, precisionBits int) (*FrameView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scene == nil || index < 0 || index >= len(a.current) {
		return nil, &engine.Error{Code: engine.CodeSchema, Path: "frameIndex", Message: "initialize first or index out of range"}
	}
	q := a.quality
	if tolerance > 0 {
		q.Tolerance = tolerance
	}
	if precisionBits > 0 {
		q.PrecisionBits = precisionBits
	}
	work := engine.ApplyQuality(a.scene, q)
	if err := engine.ValidateParameters(work); err != nil {
		return nil, err
	}
	frame, err := engine.RenderFrame(work, a.scene.Samples[index], index, q)
	if err != nil {
		return nil, err
	}
	a.current[index] = frame
	return a.frameViewLocked(index), nil
}

// RetryFrame reruns one timestamp and preserves the stable commit index.
func (a *App) RetryFrame(index int) (*FrameView, error) {
	a.mu.Lock()
	if a.scene == nil || index < 0 || index >= len(a.current) {
		a.mu.Unlock()
		return nil, &engine.Error{Code: engine.CodeState, Path: "frameIndex", Message: "invalid retry target"}
	}
	q := a.current[index].Quality
	scene := a.scene
	tm := scene.Samples[index]
	a.mu.Unlock()
	work := engine.ApplyQuality(scene, q)
	frame, err := engine.RenderFrame(work, tm, index, q)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	frame.CommitSequence = a.current[index].CommitSequence
	a.current[index] = frame
	view := a.frameViewLocked(index)
	a.mu.Unlock()
	return view, nil
}

// RunAll executes preview, full recompute and interrupted/resumed paths.
func (a *App) RunAll(dbPath string) error {
	a.mu.Lock()
	if a.scene == nil {
		a.mu.Unlock()
		return &engine.Error{Code: engine.CodeState, Path: "initialize", Message: "initialize before running"}
	}
	if a.running {
		a.mu.Unlock()
		return &engine.Error{Code: engine.CodeState, Path: "run", Message: "a run is already active"}
	}
	if dbPath == "" {
		dbPath = a.dbPath
	}
	scene := a.scene
	q := a.quality
	parent := a.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctrl, ctx := engine.NewController(parent)
	a.controller = ctrl
	a.running = true
	a.runMode = "all"
	a.lastError = ""
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()
		const budget = int64(512 * 1024 * 1024)
		preview, err := engine.Run(ctx, scene, engine.RunOptions{
			Mode: engine.ModePreview, MaxWorkers: q.MaxWorkers, BufferBytes: 8 * 1024 * 1024,
			BudgetBytes: budget, Quality: &q, Controller: ctrl,
		})
		if err != nil {
			a.setRunError(err)
			return
		}
		full, err := engine.Run(ctx, scene, engine.RunOptions{
			Mode: engine.ModeFull, DBPath: dbPath, MaxWorkers: q.MaxWorkers, BufferBytes: 8 * 1024 * 1024,
			BudgetBytes: budget, Quality: &q, Controller: ctrl,
		})
		if err != nil {
			a.setRunError(err)
			return
		}
		resumeDB := filepath.Join(filepath.Dir(dbPath), "ui-resume.db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(resumeDB + suffix)
		}
		first, err := engine.Run(ctx, scene, engine.RunOptions{
			Mode: engine.ModeResume, DBPath: resumeDB, MaxWorkers: q.MaxWorkers, BufferBytes: 8 * 1024 * 1024,
			BudgetBytes: budget, Quality: &q, Controller: ctrl, StopAfter: 2,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			a.setRunError(err)
			return
		}
		_ = first
		resumed, err := engine.Run(ctx, scene, engine.RunOptions{
			Mode: engine.ModeResume, DBPath: resumeDB, MaxWorkers: q.MaxWorkers, BufferBytes: 8 * 1024 * 1024,
			BudgetBytes: budget, Quality: &q, Controller: ctrl,
		})
		if err != nil {
			a.setRunError(err)
			return
		}
		diff, cmpErr := engine.CompareRuns(preview, full, resumed)
		a.mu.Lock()
		a.report = resumed
		a.comparison = diff
		a.current = cloneFrames(resumed.Frames)
		if cmpErr != nil {
			a.lastError = cmpErr.Error()
		}
		a.mu.Unlock()
	}()
	return nil
}

func (a *App) setRunError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastError = err.Error()
}

func (a *App) Pause() {
	a.mu.Lock()
	ctrl := a.controller
	a.mu.Unlock()
	if ctrl != nil {
		ctrl.Pause()
	}
}

func (a *App) Resume() {
	a.mu.Lock()
	ctrl := a.controller
	a.mu.Unlock()
	if ctrl != nil {
		ctrl.Resume()
	}
}

func (a *App) Cancel() {
	a.mu.Lock()
	ctrl := a.controller
	a.mu.Unlock()
	if ctrl != nil {
		ctrl.Cancel()
	}
}

func (a *App) Status() *UIStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	status := &UIStatus{
		Running: a.running, Mode: a.runMode, Error: a.lastError, Quality: a.quality,
		Comparison: a.comparison,
	}
	if a.controller != nil {
		status.Paused = a.controller.Paused()
	}
	if a.report != nil {
		status.FrameCount = len(a.report.Frames)
		status.CommitSequence = append([]int(nil), a.report.CommitSequence...)
		status.StateMatrix = a.report.StateMatrix
		status.Snapshot = a.report.SnapshotStatus
		status.Partial = a.report.Partial
	}
	return status
}

func (a *App) ComparePaths() (*engine.DiffReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.comparison == nil {
		return nil, &engine.Error{Code: engine.CodeDiff, Path: "comparison", Message: "run all three paths first"}
	}
	if !a.comparison.Consistent {
		return a.comparison, &engine.Error{Code: engine.CodeDiff, Path: "comparison", Message: "paths are inconsistent"}
	}
	return a.comparison, nil
}

func frameMetas(frames []*engine.RenderedFrame) []FrameMeta {
	out := make([]FrameMeta, 0, len(frames))
	for _, f := range frames {
		out = append(out, FrameMeta{
			Index: f.Index, TimeMs: f.TimeMs, Width: f.Width, Height: f.Height,
			Lines: f.LineCount, Hash: f.ContentHash, LegalState: len(f.Paths) > 0,
		})
	}
	return out
}

func cloneFrames(in []*engine.RenderedFrame) []*engine.RenderedFrame {
	out := make([]*engine.RenderedFrame, len(in))
	for i, f := range in {
		copyFrame := *f
		copyFrame.RGBA = append([]byte(nil), f.RGBA...)
		copyFrame.Lines = append([]engine.FlattenedPath(nil), f.Lines...)
		copyFrame.Paths = append([]engine.PathCoverage(nil), f.Paths...)
		for j := range copyFrame.Paths {
			copyFrame.Paths[j].Coverage = append([]byte(nil), f.Paths[j].Coverage...)
			copyFrame.Paths[j].FillCoverage = append([]byte(nil), f.Paths[j].FillCoverage...)
			copyFrame.Paths[j].StrokeCoverage = append([]byte(nil), f.Paths[j].StrokeCoverage...)
		}
		out[i] = &copyFrame
	}
	return out
}

func heatmap(base, frame *engine.RenderedFrame) []byte {
	if base == nil || frame == nil || base.Width != frame.Width || base.Height != frame.Height {
		return nil
	}
	out := make([]byte, len(frame.RGBA))
	for i := 0; i+3 < len(out); i += 4 {
		d := 0
		for c := 0; c < 4; c++ {
			diff := int(base.RGBA[i+c]) - int(frame.RGBA[i+c])
			if diff < 0 {
				diff = -diff
			}
			if diff > d {
				d = diff
			}
		}
		if d > 0 {
			v := uint8(math.Min(255, float64(d)*5))
			out[i], out[i+1], out[i+2], out[i+3] = v, 24, 24, 255
		} else {
			out[i+3] = 255
		}
	}
	return out
}

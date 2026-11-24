package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"virtualstage/pipeline"
	"virtualstage/sample"
)

type Config struct {
	WorkDir       string
	Workers       int
	MaxFrameBytes int64
}

type Service struct {
	WorkDir      string
	WindowSize   int
	TruthPath    string
	StatePath    string
	ManifestPath string
	engine       *pipeline.Engine
	baseModel    *pipeline.Model
	previousDir  string
	opMu         sync.Mutex
}

type Snapshot struct {
	Model            *pipeline.Model           `json:"model"`
	Windows          []pipeline.Window         `json:"windows"`
	Committed        int                       `json:"committed_windows"`
	Total            int                       `json:"total_windows"`
	SourceHash       string                    `json:"source_hash"`
	ModelHash        string                    `json:"model_hash"`
	Recovery         pipeline.RecoveryResult   `json:"recovery"`
	LastInvalidation pipeline.InvalidationList `json:"last_invalidation"`
}

func OpenService(cfg Config) (*Service, error) {
	if cfg.WorkDir == "" {
		return nil, pipelineError("E_PARA", "work directory is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.MaxFrameBytes <= 0 {
		cfg.MaxFrameBytes = 64 << 20
	}
	manifestPath := filepath.Join(cfg.WorkDir, "scene.bundle.json")
	model, mf, err := pipeline.LoadProject(manifestPath)
	if err != nil {
		return nil, err
	}
	stateDir := filepath.Join(cfg.WorkDir, "state")
	statePath := filepath.Join(stateDir, "engine-state.json")
	baseModel := model.Clone()
	if _, err := os.Stat(statePath); err == nil {
		model, err = pipeline.LoadModelJSON(statePath)
		if err != nil {
			return nil, err
		}
	}
	engine, err := pipeline.OpenEngine(model, pipeline.EngineConfig{
		WindowSize: mf.WindowSize, MaxWorkers: cfg.Workers, MaxFrameBytes: cfg.MaxFrameBytes,
		CheckpointPath: filepath.Join(cfg.WorkDir, "checkpoints", "pipeline.db"),
		FrameDir:       filepath.Join(cfg.WorkDir, "cache", "frames"),
		StateDir:       stateDir,
	})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		_ = pipeline.WriteModelJSON(statePath, model)
	}
	return &Service{
		WorkDir: cfg.WorkDir, WindowSize: mf.WindowSize, TruthPath: filepath.Join(cfg.WorkDir, mf.Truth),
		StatePath: statePath, ManifestPath: manifestPath, engine: engine, baseModel: baseModel,
		previousDir: filepath.Join(stateDir, "previous-windows"),
	}, nil
}

func (s *Service) Close() error {
	if s.engine == nil {
		return nil
	}
	return s.engine.Close()
}

func (s *Service) Engine() *pipeline.Engine {
	return s.engine
}

func (s *Service) BaseModel() *pipeline.Model {
	return s.baseModel.Clone()
}

func (s *Service) Snapshot() Snapshot {
	committed, total := s.engine.Progress()
	return Snapshot{
		Model: s.engine.Model(), Windows: s.engine.Windows(),
		Committed: committed, Total: total,
		SourceHash: s.engine.SourceHash(), ModelHash: s.engine.ModelHash(),
		Recovery: s.engine.Recovery(), LastInvalidation: s.engine.LastInvalidation(),
	}
}

func (s *Service) RunFull(ctx context.Context) (Snapshot, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.engine.RunFull(ctx); err != nil {
		return s.Snapshot(), err
	}
	_ = os.RemoveAll(s.previousDir)
	return s.Snapshot(), nil
}

func (s *Service) ApplyIncremental(ctx context.Context, edit pipeline.EditRequest) (pipeline.InvalidationList, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.backupCommittedWindows()
	return s.engine.ApplyIncremental(ctx, edit)
}

func (s *Service) Export(ctx context.Context, targetVersion int) (pipeline.ExportReport, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.engine.Export(ctx, targetVersion, filepath.Join(s.WorkDir, "exports", fmt.Sprintf("v%d", targetVersion)))
}

type TruthValidation struct {
	OK             bool     `json:"ok"`
	FramesChecked  int      `json:"frames_checked"`
	SamplesChecked int      `json:"samples_checked"`
	MaxAbsError    float64  `json:"max_abs_error"`
	SourceVersions []int    `json:"source_versions"`
	LayerVersions  []int    `json:"layer_versions"`
	LayerOrder     []string `json:"layer_order"`
	Errors         []string `json:"errors"`
}

func (s *Service) ValidateTruth(ctx context.Context) (TruthValidation, error) {
	raw, err := os.ReadFile(s.TruthPath)
	if err != nil {
		return TruthValidation{}, pipelineError("E_DECODE", "read truth: "+err.Error())
	}
	var truth sample.Truth
	if err := json.Unmarshal(raw, &truth); err != nil {
		return TruthValidation{}, pipelineError("E_DECODE", "decode truth: "+err.Error())
	}
	model := s.baseModel.Clone()
	report := TruthValidation{
		OK: true, SourceVersions: append([]int(nil), model.SourceVersions...),
		LayerOrder: append([]string(nil), model.Stack...),
	}
	seenVersions := map[int]bool{}
	for _, layer := range model.Layers {
		if !seenVersions[layer.SourceVersion] {
			seenVersions[layer.SourceVersion] = true
			report.LayerVersions = append(report.LayerVersions, layer.SourceVersion)
		}
		if endpoint, ok := truth.VisibilityEnd[layer.ID]; ok && endpoint != layer.VisibleUntilFrame {
			report.OK = false
			report.Errors = append(report.Errors, fmt.Sprintf("visibility endpoint for %s: model=%d truth=%d", layer.ID, layer.VisibleUntilFrame, endpoint))
		}
	}
	sort.Ints(report.LayerVersions)
	if truth.FrameCount != model.FrameCount {
		report.OK = false
		report.Errors = append(report.Errors, fmt.Sprintf("frame count model=%d truth=%d", model.FrameCount, truth.FrameCount))
	}
	for _, frameTruth := range truth.Frames {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		frame, err := pipeline.CompositeFrame(model, frameTruth.Frame, pipeline.CompositeConfig{MaxFrameBytes: 64 << 20})
		if err != nil {
			return report, err
		}
		if frame.Timestamp != frameTruth.Timestamp {
			report.OK = false
			report.Errors = append(report.Errors, fmt.Sprintf("frame %d timestamp mismatch", frameTruth.Frame))
		}
		for _, expected := range frameTruth.Samples {
			actual, err := frame.Sample(expected.X, expected.Y)
			if err != nil {
				return report, err
			}
			for channel := 0; channel < 4; channel++ {
				delta := math.Abs(actual.RGBA[channel] - expected.RGBA[channel])
				if delta > report.MaxAbsError {
					report.MaxAbsError = delta
				}
				if delta > 1e-6 {
					report.OK = false
					report.Errors = append(report.Errors, fmt.Sprintf("frame %d sample (%d,%d) channel %d delta %.9g", frameTruth.Frame, expected.X, expected.Y, channel, delta))
				}
			}
			report.SamplesChecked++
		}
		report.FramesChecked++
	}
	return report, nil
}

type IncrementalVerification struct {
	Match        bool                      `json:"match"`
	Comparison   pipeline.CompareReport    `json:"comparison"`
	Invalidation pipeline.InvalidationList `json:"invalidation"`
	Edit         pipeline.EditRequest      `json:"edit"`
	FullFrames   int                       `json:"full_frames"`
	Reused       []int                     `json:"reused"`
	Affected     []int                     `json:"affected"`
}

func (s *Service) VerifyIncremental(ctx context.Context, edit pipeline.EditRequest) (IncrementalVerification, error) {
	tempRoot, err := os.MkdirTemp("", "vsp-incremental-*")
	if err != nil {
		return IncrementalVerification{}, err
	}
	defer os.RemoveAll(tempRoot)
	base := s.engine.Model()
	incEngine, err := pipeline.OpenEngine(base, pipeline.EngineConfig{
		WindowSize: s.WindowSize, MaxWorkers: 4, MaxFrameBytes: 64 << 20,
		CheckpointPath: filepath.Join(tempRoot, "inc", "checkpoint.db"),
		FrameDir:       filepath.Join(tempRoot, "inc", "frames"),
		StateDir:       filepath.Join(tempRoot, "inc", "state"),
	})
	if err != nil {
		return IncrementalVerification{}, err
	}
	defer incEngine.Close()
	if err := incEngine.RunFull(ctx); err != nil {
		return IncrementalVerification{}, err
	}
	list, err := incEngine.ApplyIncremental(ctx, edit)
	if err != nil {
		return IncrementalVerification{}, err
	}
	edited := incEngine.Model()
	fullEngine, err := pipeline.OpenEngine(edited, pipeline.EngineConfig{
		WindowSize: s.WindowSize, MaxWorkers: 4, MaxFrameBytes: 64 << 20,
		CheckpointPath: filepath.Join(tempRoot, "full", "checkpoint.db"),
		FrameDir:       filepath.Join(tempRoot, "full", "frames"),
		StateDir:       filepath.Join(tempRoot, "full", "state"),
	})
	if err != nil {
		return IncrementalVerification{}, err
	}
	defer fullEngine.Close()
	if err := fullEngine.RunFull(ctx); err != nil {
		return IncrementalVerification{}, err
	}
	comparison, err := pipeline.CompareCachedWindows(ctx,
		filepath.Join(tempRoot, "full", "frames"), filepath.Join(tempRoot, "inc", "frames"), len(incEngine.Windows()))
	if err != nil {
		return IncrementalVerification{}, err
	}
	out := IncrementalVerification{
		Match: comparison.Match, Comparison: comparison, Invalidation: list, Edit: edit,
		FullFrames: comparison.Frames, Reused: append([]int(nil), list.Reused...),
	}
	for _, inv := range list.Affected {
		out.Affected = append(out.Affected, inv.Window)
	}
	return out, nil
}

type ExportVerification struct {
	Report          pipeline.ExportReport   `json:"report"`
	RoundTripOK     bool                    `json:"round_trip_ok"`
	StackEqual      bool                    `json:"stack_equal"`
	VisibilityEqual bool                    `json:"visibility_equal"`
	Deterministic   bool                    `json:"deterministic"`
	Warnings        []pipeline.LossyWarning `json:"warnings"`
	ReloadedFormat  int                     `json:"reloaded_source_version"`
}

func (s *Service) ExportAndVerify(ctx context.Context, targetVersion int) (ExportVerification, error) {
	report, err := s.Export(ctx, targetVersion)
	if err != nil {
		return ExportVerification{}, err
	}
	layerPath := filepath.Join(s.WorkDir, "exports", fmt.Sprintf("v%d", targetVersion), report.LayerFile)
	reloaded, err := pipeline.LoadFile(layerPath)
	if err != nil {
		return ExportVerification{}, err
	}
	current := s.engine.Model()
	sameStack := equalStrings(current.Stack, reloaded.Stack)
	sameVisibility := true
	for _, layer := range current.Layers {
		other, ok := reloaded.Layer(layer.ID)
		if !ok || other.Visible != layer.Visible || other.VisibleUntilFrame != layer.VisibleUntilFrame {
			sameVisibility = false
			break
		}
	}
	second, err := pipeline.ExportModel(current, targetVersion)
	if err != nil {
		return ExportVerification{}, err
	}
	return ExportVerification{
		Report: report, RoundTripOK: sameStack && sameVisibility,
		StackEqual: sameStack, VisibilityEqual: sameVisibility,
		Deterministic: second.Report.ContentHash == report.ContentHash,
		Warnings:      report.Warnings, ReloadedFormat: reloaded.SourceVersion,
	}, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type Heatmap struct {
	Available bool        `json:"available"`
	Window    int         `json:"window"`
	Frame     int         `json:"frame"`
	CellsX    int         `json:"cells_x"`
	CellsY    int         `json:"cells_y"`
	MaxDelta  float64     `json:"max_delta"`
	Diff      [][]float64 `json:"diff"`
	Message   string      `json:"message,omitempty"`
}

func (s *Service) Heatmap(window, frame, cellsX, cellsY int) (Heatmap, error) {
	current, err := s.engine.Frame(window, frame)
	if err != nil {
		return Heatmap{}, err
	}
	previousStore := pipeline.FrameStore{Dir: s.previousDir}
	previousWindow, err := previousStore.ReadWindow(window)
	if err != nil {
		return Heatmap{
			Window: window, Frame: frame, CellsX: cellsX, CellsY: cellsY,
			Message: "no previous committed frame for this window",
		}, nil
	}
	var previous *pipeline.Frame
	for _, candidate := range previousWindow.Frames {
		if candidate.Frame == frame {
			previous = candidate
			break
		}
	}
	if previous == nil {
		return Heatmap{Window: window, Frame: frame, CellsX: cellsX, CellsY: cellsY, Message: "frame was not present in the previous window"}, nil
	}
	grid, err := pipeline.DiffHeatmap(previous, current, cellsX, cellsY)
	if err != nil {
		return Heatmap{}, err
	}
	maxDelta := 0.0
	for _, row := range grid {
		for _, value := range row {
			if value > maxDelta {
				maxDelta = value
			}
		}
	}
	return Heatmap{
		Available: true, Window: window, Frame: frame, CellsX: cellsX, CellsY: cellsY,
		MaxDelta: maxDelta, Diff: grid,
	}, nil
}

func (s *Service) FramePNG(window, frame int) ([]byte, error) {
	selected, err := s.engine.Frame(window, frame)
	if err != nil {
		return nil, err
	}
	return selected.PNG()
}

func (s *Service) FrameStats(window, frame int) (pipeline.FrameSummary, error) {
	selected, err := s.engine.Frame(window, frame)
	if err != nil {
		return pipeline.FrameSummary{}, err
	}
	return pipeline.SummarizeFrame(selected), nil
}

func (s *Service) backupCommittedWindows() {
	_ = os.RemoveAll(s.previousDir)
	if err := os.MkdirAll(s.previousDir, 0o755); err != nil {
		return
	}
	for _, window := range s.engine.Windows() {
		if window.State != pipeline.WindowCommitted {
			continue
		}
		name := fmt.Sprintf("window-%05d.vspw", window.Index)
		sourcePath := filepath.Join(s.WorkDir, "cache", "frames", name)
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(s.previousDir, name), raw, 0o644)
	}
}

type simpleError struct {
	code    string
	message string
}

func (e *simpleError) Error() string { return e.code + ": " + e.message }

func pipelineError(code, message string) error {
	return &simpleError{code: code, message: message}
}

// Package demo runs the complete deterministic acceptance scenario.
package demo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"diffrender/errs"
	"diffrender/optim"
	"diffrender/render"
	"diffrender/sample"
	"diffrender/scene"
)

// Config controls the local acceptance run.
type Config struct {
	BaseSeed int64
	Tasks    int
	OutDir   string
	Optim    optim.Config
}

// DefaultConfig returns the reproducible three-task demonstration.
func DefaultConfig() Config {
	cfg := optim.DefaultConfig()
	cfg.Iterations = 64
	cfg.Tolerance = 1e-12
	cfg.WindowSize = 4
	cfg.Render = render.Config{MaxWorkers: 4, BlockPixels: 128, QueueCapacity: 8, Retries: 1}
	return Config{BaseSeed: 20261124, Tasks: 3, OutDir: "artifacts/demo", Optim: cfg}
}

// Report contains the machine-readable result of the full local run.
type Report struct {
	Seed               int64        `json:"seed"`
	GoVersion          string       `json:"goVersion"`
	TaskCount          int          `json:"taskCount"`
	AllThreePathsEqual bool         `json:"allThreePathsEqual"`
	AllConverged       bool         `json:"allConverged"`
	AllChecksPassed    bool         `json:"allChecksPassed"`
	Tasks              []TaskReport `json:"tasks"`
}

type TaskReport struct {
	Name                string             `json:"name"`
	Seed                int64              `json:"seed"`
	Directory           string             `json:"directory"`
	Versions            []VersionReport    `json:"versions"`
	InitialLoss         float64            `json:"initialLoss"`
	FinalLoss           float64            `json:"finalLoss"`
	Tolerance           float64            `json:"tolerance"`
	Converged           bool               `json:"converged"`
	LossSequence        []float64          `json:"lossSequence"`
	WindowHashes        []string           `json:"windowHashes"`
	Windows             []optim.WindowInfo `json:"windows"`
	LiveCheckpoints     string             `json:"liveCheckpoints"`
	ResumeCheckpoints   string             `json:"resumeCheckpoints"`
	SerialCheckpoints   string             `json:"serialCheckpoints"`
	PausedAtWindow      int                `json:"pausedAtWindow"`
	ThreePathsEqual     bool               `json:"threePathsEqual"`
	ParallelSerialEqual bool               `json:"parallelSerialEqual"`
	RecoveryChecked     bool               `json:"checkpointRecoveryChecked"`
	Exports             []ExportReport     `json:"exports"`
	Checks              []CheckReport      `json:"checks"`
	Recovery            RecoveryReport     `json:"recovery"`
}

type VersionReport struct {
	Version           string `json:"version"`
	SceneFile         string `json:"sceneFile"`
	InitialFile       string `json:"initialFile"`
	SourceVersion     string `json:"sourceVersion"`
	TraceEntries      int    `json:"traceEntries"`
	EquivalentToTruth bool   `json:"equivalentToTruth"`
}

type ExportReport struct {
	Version     string `json:"version"`
	File        string `json:"file"`
	SHA256      string `json:"sha256"`
	ByteStable  bool   `json:"byteStable"`
	RoundTripOK bool   `json:"roundTripOK"`
}

type CheckReport struct {
	Name   string `json:"name"`
	Code   string `json:"code,omitempty"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type RecoveryReport struct {
	CenterError       float64 `json:"centerError"`
	RadiusError       float64 `json:"radiusError"`
	AlbedoError       float64 `json:"albedoError"`
	LightPowerPct     float64 `json:"lightPowerPct"`
	TruthWithinBounds bool    `json:"truthWithinBounds"`
}

// Run generates tasks, executes all optimization paths, exports every target
// version, checks failure codes, and writes report.json and task reports.
func Run(cfg Config) (*Report, error) {
	if cfg.Tasks <= 0 {
		return nil, errs.New(errs.CodeConfig, "<demo>", "tasks", "must be > 0")
	}
	out, err := filepath.Abs(cfg.OutDir)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(out); err != nil {
		return nil, fmt.Errorf("clean output: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	report := &Report{
		Seed: cfg.BaseSeed, GoVersion: runtime.Version(), TaskCount: cfg.Tasks,
		AllThreePathsEqual: true, AllConverged: true, AllChecksPassed: true,
	}
	for i := 0; i < cfg.Tasks; i++ {
		seed := cfg.BaseSeed + int64(i)*1_000_003
		task, err := sample.GenerateTask(seed)
		if err != nil {
			return report, fmt.Errorf("generate task %d: %w", seed, err)
		}
		taskDir := filepath.Join(out, task.Name)
		if err := sample.WriteTask(taskDir, task); err != nil {
			return report, fmt.Errorf("write task %d: %w", seed, err)
		}
		taskReport, err := runTask(cfg, task, taskDir, i == 0)
		if err != nil {
			return report, fmt.Errorf("task %d: %w", seed, err)
		}
		report.Tasks = append(report.Tasks, *taskReport)
		report.AllThreePathsEqual = report.AllThreePathsEqual && taskReport.ThreePathsEqual && taskReport.ParallelSerialEqual
		report.AllConverged = report.AllConverged && taskReport.Converged
		report.AllChecksPassed = report.AllChecksPassed && allChecksPassed(taskReport.Checks)
	}
	if err := writeJSON(filepath.Join(out, "report.json"), report); err != nil {
		return report, err
	}
	if !report.AllThreePathsEqual || !report.AllConverged || !report.AllChecksPassed {
		return report, fmt.Errorf("acceptance run failed: paths=%v converged=%v checks=%v",
			report.AllThreePathsEqual, report.AllConverged, report.AllChecksPassed)
	}
	return report, nil
}

func runTask(cfg Config, task *sample.Task, taskDir string, checkRecovery bool) (*TaskReport, error) {
	tr := &TaskReport{
		Name: task.Name, Seed: task.Seed, Directory: displayPath(taskDir),
		Tolerance: cfg.Optim.Tolerance, ThreePathsEqual: true, ParallelSerialEqual: true,
	}
	scenePaths := task.ScenePaths(taskDir)
	initialPaths := task.InitialPaths(taskDir)
	for _, version := range scene.SupportedVersions {
		loaded, err := scene.LoadFile(scenePaths[version])
		if err != nil {
			return nil, err
		}
		if err := scene.CompareSemantics(task.Truth, loaded, scene.Version3); err != nil {
			return nil, fmt.Errorf("version %s unification: %w", version, err)
		}
		tr.Versions = append(tr.Versions, VersionReport{
			Version: version, SceneFile: displayPath(scenePaths[version]), InitialFile: displayPath(initialPaths[version]),
			SourceVersion: loaded.SourceVersion, TraceEntries: len(loaded.Trace), EquivalentToTruth: true,
		})
	}
	initial, err := scene.LoadFile(initialPaths[scene.Version1])
	if err != nil {
		return nil, err
	}
	targetFiles := task.TargetPaths(taskDir)
	targets := make([]render.Image, len(targetFiles))
	for i, path := range targetFiles {
		im, err := render.ReadTargetFile(path)
		if err != nil {
			return nil, err
		}
		if im.CameraID != task.Targets[i].CameraID {
			return nil, fmt.Errorf("target camera mismatch at %s", path)
		}
		for p := range im.Pixels {
			if math.Float64bits(im.Pixels[p]) != math.Float64bits(task.Targets[i].Pixels[p]) {
				return nil, fmt.Errorf("target file %s does not match generated target", path)
			}
		}
		targets[i] = im
	}
	tr.Checks = append(tr.Checks, pass("target files decode and match generator", "all float64 pixels match"))

	liveDir := filepath.Join(taskDir, "checkpoints", "live")
	live, err := runOptimization(initial, targets, cfg.Optim, liveDir)
	if err != nil {
		return nil, fmt.Errorf("live optimization: %w", err)
	}
	tr.LiveCheckpoints = displayPath(liveDir)
	tr.LossSequence = append([]float64(nil), live.Losses...)
	tr.InitialLoss = live.Losses[0]
	tr.FinalLoss = live.State.Loss
	tr.Converged = live.Converged
	tr.Windows = append([]optim.WindowInfo(nil), live.Windows...)
	for _, w := range live.Windows {
		tr.WindowHashes = append(tr.WindowHashes, w.ContentHash)
	}
	if !live.Converged {
		tr.Checks = append(tr.Checks, fail("optimization converged", fmt.Sprintf("loss %.17g > tolerance %.17g", live.State.Loss, cfg.Optim.Tolerance)))
	} else {
		tr.Checks = append(tr.Checks, pass("optimization converged", fmt.Sprintf("loss %.17g <= tolerance %.17g", live.State.Loss, cfg.Optim.Tolerance)))
	}
	monotonic := true
	for i := 1; i < len(live.Losses); i++ {
		if live.Losses[i] > live.Losses[i-1] {
			monotonic = false
			break
		}
	}
	if monotonic {
		tr.Checks = append(tr.Checks, pass("loss sequence monotonic", fmt.Sprintf("%d losses", len(live.Losses))))
	} else {
		tr.Checks = append(tr.Checks, fail("loss sequence monotonic", "loss increased"))
	}

	oneShotDir := filepath.Join(taskDir, "checkpoints", "oneshot")
	oneShot, err := runOptimization(initial, targets, cfg.Optim, oneShotDir)
	if err != nil {
		return nil, fmt.Errorf("one-shot optimization: %w", err)
	}
	if err := optim.CompareResults(live, oneShot); err != nil {
		tr.Checks = append(tr.Checks, fail("one-shot equality", err.Error()))
		tr.ThreePathsEqual = false
	} else {
		tr.Checks = append(tr.Checks, pass("one-shot equality", "parameters, losses, and windows are bit-identical"))
	}

	resumeDir := filepath.Join(taskDir, "checkpoints", "resume")
	pausedCfg := cfg.Optim
	pausedCfg.PauseAfterWindow = 1
	paused, err := runOptimization(initial, targets, pausedCfg, resumeDir)
	if err != nil {
		return nil, fmt.Errorf("paused optimization: %w", err)
	}
	tr.ResumeCheckpoints = displayPath(resumeDir)
	tr.PausedAtWindow = 1
	if !paused.Paused || len(paused.Windows) != 2 {
		tr.Checks = append(tr.Checks, fail("pause after window 1", fmt.Sprintf("paused=%v windows=%d", paused.Paused, len(paused.Windows))))
	} else {
		tr.Checks = append(tr.Checks, pass("pause after window 1", fmt.Sprintf("iteration=%d", paused.State.Iteration)))
	}
	resumed, err := runOptimization(initial, targets, cfg.Optim, resumeDir)
	if err != nil {
		return nil, fmt.Errorf("resume optimization: %w", err)
	}
	if err := optim.CompareResults(live, resumed); err != nil {
		tr.Checks = append(tr.Checks, fail("resume equality", err.Error()))
		tr.ThreePathsEqual = false
	} else {
		tr.Checks = append(tr.Checks, pass("resume equality", "parameters, losses, and windows are bit-identical"))
	}
	if len(live.Windows) == 0 && len(oneShot.Windows) == 0 && len(resumed.Windows) == 0 {
		tr.ThreePathsEqual = false
	}

	serialCfg := cfg.Optim
	serialCfg.Render.MaxWorkers = 1
	serialCfg.Render.QueueCapacity = 0
	serialDir := filepath.Join(taskDir, "checkpoints", "serial")
	serial, err := runOptimization(initial, targets, serialCfg, serialDir)
	if err != nil {
		return nil, fmt.Errorf("serial optimization: %w", err)
	}
	tr.SerialCheckpoints = displayPath(serialDir)
	if err := optim.CompareResults(live, serial); err != nil {
		tr.ParallelSerialEqual = false
		tr.Checks = append(tr.Checks, fail("parallel/serial equality", err.Error()))
	} else {
		tr.ParallelSerialEqual = true
		tr.Checks = append(tr.Checks, pass("parallel/serial equality", "MaxWorkers 4 and 1 are bit-identical"))
	}

	final := live.FinalScene
	if err := exportVersions(final, taskDir, tr); err != nil {
		return nil, err
	}
	tr.Recovery = recoveryMetrics(final, task.Truth)
	if tr.Recovery.TruthWithinBounds {
		tr.Checks = append(tr.Checks, pass("truth reconstruction bounds", fmt.Sprintf("center=%.3g radius=%.3g albedo=%.3g power=%.3g%%",
			tr.Recovery.CenterError, tr.Recovery.RadiusError, tr.Recovery.AlbedoError, tr.Recovery.LightPowerPct)))
	} else {
		tr.Checks = append(tr.Checks, fail("truth reconstruction bounds", fmt.Sprintf("center=%.3g radius=%.3g albedo=%.3g power=%.3g%%",
			tr.Recovery.CenterError, tr.Recovery.RadiusError, tr.Recovery.AlbedoError, tr.Recovery.LightPowerPct)))
	}

	tr.Checks = append(tr.Checks, checkpointChecks(live, liveDir)...)
	tr.Checks = append(tr.Checks, errorPathChecks(task, initial, targets, live, targetFiles[0], cfg.Optim, taskDir)...)
	if checkRecovery {
		recoveryCheck, err := verifyCorruptionRecovery(initial, targets, cfg.Optim, taskDir, live)
		if err != nil {
			tr.Checks = append(tr.Checks, fail("checkpoint corruption recovery", err.Error()))
		} else {
			tr.Checks = append(tr.Checks, pass("checkpoint corruption recovery", recoveryCheck))
			tr.RecoveryChecked = true
		}
	}
	if err := writeJSON(filepath.Join(taskDir, "task-report.json"), tr); err != nil {
		return nil, err
	}
	return tr, nil
}

func runOptimization(initial *scene.Scene, targets []render.Image, cfg optim.Config, dir string) (*optim.RunResult, error) {
	runner, err := optim.NewRunner(initial, targets, cfg, dir)
	if err != nil {
		return nil, err
	}
	return runner.Run(context.Background())
}

func exportVersions(final *scene.Scene, taskDir string, tr *TaskReport) error {
	dir := filepath.Join(taskDir, "reconstructed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, version := range scene.SupportedVersions {
		opts := scene.ExportOptions{Version: version}
		first, err := scene.Export(final, opts)
		if err != nil {
			return err
		}
		second, err := scene.Export(final, opts)
		if err != nil {
			return err
		}
		stable := bytes.Equal(first.Data, second.Data)
		path := filepath.Join(dir, "reconstructed-"+version+".json")
		if err := os.WriteFile(path, first.Data, 0o644); err != nil {
			return err
		}
		reloaded, err := scene.Load(first.Data, path)
		if err != nil {
			return err
		}
		if err := scene.CompareSemantics(final, reloaded, version); err != nil {
			return err
		}
		sum := sha256.Sum256(first.Data)
		exp := ExportReport{
			Version: version, File: displayPath(path), SHA256: hex.EncodeToString(sum[:]),
			ByteStable: stable, RoundTripOK: true,
		}
		tr.Exports = append(tr.Exports, exp)
		if stable {
			tr.Checks = append(tr.Checks, pass("byte-stable export "+version, exp.SHA256))
		} else {
			tr.Checks = append(tr.Checks, fail("byte-stable export "+version, "repeated export differed"))
		}
	}
	return nil
}

func checkpointChecks(result *optim.RunResult, dir string) []CheckReport {
	var checks []CheckReport
	runnerStore := optim.NewStore(dir, optim.Metadata{
		SourceVersion: result.SourceVersion, AlgorithmVersion: result.AlgorithmVersion,
		TaskHash: result.TaskHash, ConfigHash: result.ConfigHash,
	})
	snap, err := runnerStore.Load()
	if err != nil {
		return []CheckReport{fail("checkpoint snapshot valid", err.Error())}
	}
	if !snap.Valid || len(snap.Checkpoints) != len(result.Windows) {
		return []CheckReport{fail("checkpoint snapshot valid", fmt.Sprintf("valid=%v windows=%d", snap.Valid, len(snap.Checkpoints)))}
	}
	checks = append(checks, pass("checkpoint snapshot valid", fmt.Sprintf("%d windows, all hashes and versions verified", len(snap.Windows))))
	for i, cp := range snap.Checkpoints {
		if cp.Window != i || len(cp.BlockGradients) == 0 || len(cp.Parameters) == 0 {
			checks = append(checks, fail("checkpoint payload complete", fmt.Sprintf("window %d", i)))
			return checks
		}
	}
	checks = append(checks, pass("checkpoint payload complete", "scene, optimizer state, parameters, losses, and block gradients present"))
	return checks
}

func errorPathChecks(task *sample.Task, initial *scene.Scene, targets []render.Image, live *optim.RunResult, targetPath string, cfg optim.Config, taskDir string) []CheckReport {
	var checks []CheckReport
	raw, _ := os.ReadFile(targetPath)
	corrupt := append([]byte(nil), raw...)
	corrupt[len(corrupt)-40] ^= 0x80
	_, err := render.DecodeTarget(corrupt, targetPath)
	checks = append(checks, expected("corrupt target", err, errs.CodeDecode))

	_, err = scene.Load([]byte(`{"schema_version":"99.0"}`), "unknown.json")
	checks = append(checks, expected("unknown schema version", err, errs.CodeVersion))

	var doc map[string]json.RawMessage
	if json.Unmarshal(task.VersionFiles[scene.Version1], &doc) == nil {
		delete(doc, "objects")
		broken, _ := json.Marshal(doc)
		_, err = scene.Load(broken, "missing-required.json")
	} else {
		err = fmt.Errorf("cannot build missing-field fixture")
	}
	checks = append(checks, expected("missing required field", err, errs.CodeVersion))

	bad := initial.Clone()
	bad.Spheres[0].Radius = 0
	_, err = render.Compute(bad, targets, render.Config{MaxWorkers: 1})
	checks = append(checks, expected("parameter bounds", err, errs.CodeParam))

	lossy := live.FinalScene.Clone()
	lossy.Spheres[0].Coat = 0.5
	_, err = scene.Export(lossy, scene.ExportOptions{Version: scene.Version1})
	checks = append(checks, expected("explicit lossy downgrade", err, errs.CodeLossy))

	round := task.Truth.Clone()
	round.Spheres[0].Center.X += 1e-5
	err = scene.CompareSemantics(task.Truth, round, scene.Version3)
	checks = append(checks, expected("round-trip mismatch", err, errs.CodeRound))

	gradeCfg := cfg
	gradeCfg.Iterations = 2
	gradeCfg.WindowSize = 2
	gradeCfg.GradientHook = func(iteration int, gradient []float64) error {
		gradient[0] = math.NaN()
		return nil
	}
	gradeDir := filepath.Join(taskDir, "checkpoints", "egrad")
	runner, err := optim.NewRunner(initial, targets, gradeCfg, gradeDir)
	if err == nil {
		_, err = runner.Run(context.Background())
	}
	checks = append(checks, expected("non-finite gradient", err, errs.CodeGradient))
	if err != nil && runner != nil {
		if snap, loadErr := runner.Store().Load(); loadErr == nil && len(snap.Windows) == 0 {
			checks = append(checks, pass("E_GRAD transactional rollback", "no window was committed"))
		} else {
			checks = append(checks, fail("E_GRAD transactional rollback", "unexpected committed state"))
		}
	}

	altered := *live
	altered.FinalScene = live.FinalScene.Clone()
	altered.FinalScene.Spheres[0].Center.X += 1e-9
	err = optim.CompareResults(live, &altered)
	checks = append(checks, expected("run divergence", err, errs.CodeDiverge))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelDir := filepath.Join(taskDir, "checkpoints", "cancelled")
	runner, err = optim.NewRunner(initial, targets, cfg, cancelDir)
	if err == nil {
		_, err = runner.Run(ctx)
	}
	checks = append(checks, expected("cancelled run", err, errs.CodeCancel))

	faultCfg := render.Config{
		MaxWorkers: 2, BlockPixels: 64, QueueCapacity: 0, Retries: 0,
		Fault: func(view, start, end, attempt int) error {
			if view == 0 && start == 0 {
				return fmt.Errorf("injected render fault")
			}
			return nil
		},
	}
	batch, err := render.Compute(initial, targets, faultCfg)
	checks = append(checks, expected("isolated block failure", err, errs.CodeRender))
	if batch != nil {
		retryErr := batch.RetryFailed(render.Config{MaxWorkers: 2, BlockPixels: 64, QueueCapacity: 0, Retries: 0})
		if retryErr == nil && batch.Metrics.FailedBlockCount == 0 {
			checks = append(checks, pass("failed block retry", "only failed block recomputed; aggregate rebuilt"))
		} else {
			checks = append(checks, fail("failed block retry", fmt.Sprint(retryErr)))
		}
	}
	return checks
}

func verifyCorruptionRecovery(initial *scene.Scene, targets []render.Image, cfg optim.Config, taskDir string, baseline *optim.RunResult) (string, error) {
	dir := filepath.Join(taskDir, "checkpoints", "recovery")
	first, err := runOptimization(initial, targets, cfg, dir)
	if err != nil {
		return "", err
	}
	if err := optim.CompareResults(baseline, first); err != nil {
		return "", err
	}
	windowPath := filepath.Join(dir, "window-000000.json")
	data, err := os.ReadFile(windowPath)
	if err != nil {
		return "", err
	}
	idx := bytes.Index(data, []byte(`"contentHash": "`))
	if idx < 0 {
		return "", fmt.Errorf("content hash field missing")
	}
	idx += len(`"contentHash": "`)
	if data[idx] == '0' {
		data[idx] = '1'
	} else {
		data[idx] = '0'
	}
	if err := os.WriteFile(windowPath, data, 0o644); err != nil {
		return "", err
	}
	store := optim.NewStore(dir, optim.Metadata{
		SourceVersion: baseline.SourceVersion, AlgorithmVersion: baseline.AlgorithmVersion,
		TaskHash: baseline.TaskHash, ConfigHash: baseline.ConfigHash,
	})
	snap, err := store.Load()
	if err != nil {
		return "", err
	}
	if !snap.Invalidated {
		return "", fmt.Errorf("tampered snapshot was not invalidated")
	}
	recovered, err := runOptimization(initial, targets, cfg, dir)
	if err != nil {
		return "", err
	}
	if !recovered.Invalidated || recovered.Resumed {
		return "", fmt.Errorf("recovery did not discard the full snapshot")
	}
	if err := optim.CompareResults(baseline, recovered); err != nil {
		return "", err
	}
	return "tampered window invalidated the whole snapshot and recomputation matched the baseline bit-for-bit", nil
}

func recoveryMetrics(got, truth *scene.Scene) RecoveryReport {
	r := RecoveryReport{}
	if len(got.Spheres) == 0 || len(truth.Spheres) == 0 {
		return r
	}
	g, w := got.Spheres[0], truth.Spheres[0]
	r.CenterError = vecDistance(g.Center, w.Center)
	r.RadiusError = math.Abs(g.Radius - w.Radius)
	r.AlbedoError = vecDistance(g.Albedo, w.Albedo)
	if len(got.Lights) > 0 && len(truth.Lights) > 0 && truth.Lights[0].RadiantPowerW != 0 {
		r.LightPowerPct = math.Abs(got.Lights[0].RadiantPowerW-truth.Lights[0].RadiantPowerW) / truth.Lights[0].RadiantPowerW * 100
	}
	r.TruthWithinBounds = r.CenterError <= 0.02 && r.RadiusError <= 0.02 && r.AlbedoError <= 0.04 && r.LightPowerPct <= 5
	return r
}

func expected(name string, err error, code errs.Code) CheckReport {
	if err == nil {
		return fail(name, "expected "+string(code)+", got no error")
	}
	got, ok := errs.CodeOf(err)
	if !ok || got != code {
		return fail(name, fmt.Sprintf("expected %s, got %v", code, err))
	}
	return CheckReport{Name: name, Code: string(code), Passed: true, Detail: err.Error()}
}

func pass(name, detail string) CheckReport {
	return CheckReport{Name: name, Passed: true, Detail: detail}
}

func fail(name, detail string) CheckReport {
	return CheckReport{Name: name, Passed: false, Detail: detail}
}

func allChecksPassed(checks []CheckReport) bool {
	for _, c := range checks {
		if !c.Passed {
			return false
		}
	}
	return true
}

func vecDistance(a, b scene.Vec3) float64 {
	d := a.Sub(b)
	return math.Sqrt(d.X*d.X + d.Y*d.Y + d.Z*d.Z)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func displayPath(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || len(rel) > len(path) {
		return path
	}
	return rel
}

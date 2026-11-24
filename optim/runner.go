package optim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"diffrender/errs"
	"diffrender/render"
	"diffrender/scene"
)

const AlgorithmVersion = "photometric-consistent-sgd-v1"

// Config controls deterministic photometric optimization. PauseAfterWindow,
// GradientHook, and execution-only render worker settings are excluded from the
// persisted config hash.
type Config struct {
	LearningRate     float64
	Iterations       int
	GradClip         float64
	Tolerance        float64
	WindowSize       int
	MaxBacktracks    int
	MinStep          float64
	Render           render.Config
	PauseAfterWindow int
	// GradientHook can inspect or mutate a copy before clipping. Returning an
	// error stops the round without applying any parameter.
	GradientHook func(iteration int, gradient []float64) error
}

func DefaultConfig() Config {
	return Config{
		LearningRate: 0.20, Iterations: 64, GradClip: 1e6, Tolerance: 1e-12,
		WindowSize: 4, MaxBacktracks: 28, MinStep: 1e-10,
		Render: render.DefaultConfig(), PauseAfterWindow: -1,
	}
}

func (c Config) validate() error {
	if !finite(c.LearningRate) || c.LearningRate <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "learningRate", "must be finite and > 0")
	}
	if c.Iterations <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "iterations", "must be > 0")
	}
	if !finite(c.GradClip) || c.GradClip <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "gradClip", "must be finite and > 0")
	}
	if !finite(c.Tolerance) || c.Tolerance <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "tolerance", "must be finite and > 0")
	}
	if c.WindowSize <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "windowSize", "must be > 0")
	}
	if c.MaxBacktracks <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "maxBacktracks", "must be > 0")
	}
	if !finite(c.MinStep) || c.MinStep <= 0 {
		return errs.New(errs.CodeConfig, "<optim>", "minStep", "must be finite and > 0")
	}
	return nil
}

// Runner executes or resumes one deterministic task.
type Runner struct {
	initial *scene.Scene
	targets []render.Image
	config  Config
	store   *Store
	meta    Metadata
	specs   []render.ParamSpec
}

// NewRunner binds an initial scene, target images, config, and checkpoint store.
func NewRunner(initial *scene.Scene, targets []render.Image, cfg Config, checkpointDir string) (*Runner, error) {
	if initial == nil {
		return nil, errs.New(errs.CodeConfig, "<optim>", "initial", "initial scene is nil")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(targets) != len(initial.Cameras) {
		return nil, errs.New(errs.CodeDecode, "<optim>", "targets", "expected %d targets, got %d", len(initial.Cameras), len(targets))
	}
	for i := range targets {
		if err := targets[i].Validate(fmt.Sprintf("<target:%d>", i)); err != nil {
			return nil, err
		}
		if targets[i].CameraID != initial.Cameras[i].ID ||
			targets[i].Width != initial.Cameras[i].Width ||
			targets[i].Height != initial.Cameras[i].Height {
			return nil, errs.New(errs.CodeDecode, "<optim>", "targets", "target %d does not match camera %q", i, initial.Cameras[i].ID)
		}
	}
	specs, err := render.ParameterSpecs(initial)
	if err != nil {
		return nil, err
	}
	taskHash, err := TaskHash(initial, targets)
	if err != nil {
		return nil, err
	}
	configHash, err := ConfigHash(cfg)
	if err != nil {
		return nil, err
	}
	meta := Metadata{
		SourceVersion: initial.SourceVersion, AlgorithmVersion: AlgorithmVersion,
		TaskHash: taskHash, ConfigHash: configHash,
	}
	return &Runner{
		initial: initial.Clone(), targets: cloneImages(targets), config: cfg,
		store: NewStore(checkpointDir, meta), meta: meta, specs: specs,
	}, nil
}

func (r *Runner) Store() *Store      { return r.store }
func (r *Runner) Metadata() Metadata { return r.meta }

// RunResult is the deterministic output of full, live, or resumed execution.
type RunResult struct {
	FinalScene       *scene.Scene  `json:"finalScene"`
	State            State         `json:"state"`
	Losses           []float64     `json:"losses"`
	Windows          []WindowInfo  `json:"windows"`
	Checkpoints      []*Checkpoint `json:"checkpoints,omitempty"`
	Paused           bool          `json:"paused"`
	Converged        bool          `json:"converged"`
	Resumed          bool          `json:"resumed"`
	Invalidated      bool          `json:"invalidated"`
	InvalidationNote string        `json:"invalidationNote,omitempty"`
	SourceVersion    string        `json:"sourceVersion"`
	AlgorithmVersion string        `json:"algorithmVersion"`
	TaskHash         string        `json:"taskHash"`
	ConfigHash       string        `json:"configHash"`
}

// Run resumes a matching snapshot when present, otherwise starts from zero.
// Cancellation and mid-window errors return the last committed snapshot.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.checkInitialParameters(); err != nil {
		return nil, err
	}
	snap, err := r.store.Load()
	if err != nil {
		return nil, err
	}
	result := &RunResult{
		SourceVersion: r.meta.SourceVersion, AlgorithmVersion: r.meta.AlgorithmVersion,
		TaskHash: r.meta.TaskHash, ConfigHash: r.meta.ConfigHash,
	}
	if snap.Invalidated {
		result.Invalidated = true
		result.InvalidationNote = snap.Reason
		if err := r.store.Invalidate(snap.Reason); err != nil {
			return nil, err
		}
		snap = Snapshot{}
	}
	current := r.initial.Clone()
	state := State{Iteration: 0, Loss: 0, StepScale: 1}
	losses := []float64(nil)
	nextWindow := 0
	lastCommittedIteration := 0
	savedScene := current.Clone()
	savedState := state
	savedLosses := []float64(nil)
	var acc []BlockGradient
	if len(snap.Checkpoints) > 0 {
		result.Resumed = true
		last := snap.Checkpoints[len(snap.Checkpoints)-1]
		current = last.Scene.Clone()
		state = last.State
		losses = append([]float64(nil), last.Losses...)
		nextWindow = last.Window + 1
		lastCommittedIteration = state.Iteration
		savedScene = current.Clone()
		savedState = state
		savedLosses = append([]float64(nil), losses...)
	}
	result.Checkpoints = append(result.Checkpoints, snap.Checkpoints...)
	result.Windows = append(result.Windows, snap.Windows...)
	fail := func(err error) (*RunResult, error) {
		result.FinalScene = savedScene.Clone()
		result.State = savedState
		result.Losses = append([]float64(nil), savedLosses...)
		result.Converged = savedState.Loss > 0 && savedState.Loss <= r.config.Tolerance
		return result, err
	}
	seenInitial := len(losses) > 0
	for state.Iteration < r.config.Iterations {
		if err := ctx.Err(); err != nil {
			return fail(errs.Wrap(err, errs.CodeCancel, "<optim>", "iteration", "optimization cancelled before iteration %d", state.Iteration+1))
		}
		batch, err := r.evaluate(current)
		if err != nil {
			return fail(err)
		}
		if !seenInitial {
			state.Loss = batch.Loss
			losses = []float64{batch.Loss}
			seenInitial = true
			savedScene = current.Clone()
			savedState = state
			savedLosses = append([]float64(nil), losses...)
		} else if math.Float64bits(batch.Loss) != math.Float64bits(state.Loss) {
			return fail(errs.New(errs.CodeCheckpt, "<optim>", "loss", "restored loss %.17g does not reproduce snapshot %.17g", batch.Loss, state.Loss))
		}
		if state.Loss <= r.config.Tolerance {
			result.Converged = true
			break
		}
		grad := append([]float64(nil), batch.Grad...)
		if err := checkFiniteGradient(grad, batch.ParamIDs); err != nil {
			return fail(err)
		}
		if r.config.GradientHook != nil {
			if err := r.config.GradientHook(state.Iteration, grad); err != nil {
				return fail(err)
			}
		}
		if err := checkFiniteGradient(grad, batch.ParamIDs); err != nil {
			return fail(err)
		}
		norm := norm2(grad)
		state.GradientNorm = norm
		if !finite(norm) {
			return fail(errs.New(errs.CodeGradient, "<optim>", "gradient", "gradient norm is non-finite"))
		}
		if norm > r.config.GradClip {
			scale := r.config.GradClip / norm
			for i := range grad {
				grad[i] *= scale
			}
			norm = r.config.GradClip
		}
		acc, err = accumulateBlockGradients(acc, batch.Blocks)
		if err != nil {
			return fail(err)
		}
		values := render.ParameterValues(current, r.specs)
		direction := preconditionDirection(r.specs, grad)
		candidateScene, candidateLoss, accepted, finalFactor, err := r.lineSearch(current, values, direction, state.Loss, state.StepScale)
		if err != nil {
			return fail(err)
		}
		state.Iteration++
		if accepted {
			current = candidateScene
			state.Loss = candidateLoss
			state.AcceptedSteps++
			state.StepScale = math.Min(1, finalFactor*1.5)
		} else {
			state.RejectedSteps++
			state.StepScale = math.Max(r.config.MinStep/r.config.LearningRate, finalFactor)
		}
		losses = append(losses, state.Loss)
		if state.Iteration%r.config.WindowSize == 0 {
			if err := r.commit(nextWindow, current, state, losses, acc); err != nil {
				return fail(err)
			}
			nextWindow++
			lastCommittedIteration = state.Iteration
			acc = nil
			savedScene = current.Clone()
			savedState = state
			savedLosses = append([]float64(nil), losses...)
			updated, loadErr := r.store.Load()
			if loadErr != nil {
				return fail(loadErr)
			}
			if !updated.Valid || len(updated.Windows) != nextWindow {
				return fail(errs.New(errs.CodeCheckpt, "<optim>", "checkpoint", "committed snapshot did not reload as valid"))
			}
			result.Checkpoints = append([]*Checkpoint(nil), updated.Checkpoints...)
			result.Windows = append([]WindowInfo(nil), updated.Windows...)
			if r.config.PauseAfterWindow >= 0 && r.config.PauseAfterWindow == nextWindow-1 {
				result.Paused = true
				break
			}
		}
		if state.Loss <= r.config.Tolerance {
			result.Converged = true
			break
		}
	}
	if !result.Paused && state.Iteration > lastCommittedIteration {
		if err := r.commit(nextWindow, current, state, losses, acc); err != nil {
			return fail(err)
		}
		savedScene = current.Clone()
		savedState = state
		savedLosses = append([]float64(nil), losses...)
		updated, loadErr := r.store.Load()
		if loadErr != nil {
			return fail(loadErr)
		}
		if !updated.Valid {
			return fail(errs.New(errs.CodeCheckpt, "<optim>", "checkpoint", "final snapshot did not reload as valid"))
		}
		result.Checkpoints = append([]*Checkpoint(nil), updated.Checkpoints...)
		result.Windows = append([]WindowInfo(nil), updated.Windows...)
	}
	result.FinalScene = current.Clone()
	result.State = state
	result.Losses = append([]float64(nil), losses...)
	result.Converged = result.Converged || (state.Loss > 0 && state.Loss <= r.config.Tolerance)
	return result, nil
}

func (r *Runner) checkInitialParameters() error {
	values := render.ParameterValues(r.initial, r.specs)
	for i, v := range values {
		if !finite(v) {
			return errs.New(errs.CodeGradient, "<optim>", r.specs[i].ID, "initial parameter is non-finite")
		}
	}
	return nil
}

func (r *Runner) evaluate(s *scene.Scene) (*render.Batch, error) {
	batch, err := render.Compute(s, r.targets, r.config.Render)
	if err != nil {
		return batch, err
	}
	if !finite(batch.Loss) {
		return batch, errs.New(errs.CodeGradient, "<optim>", "loss", "photometric loss is non-finite")
	}
	return batch, nil
}

func (r *Runner) lineSearch(current *scene.Scene, values, direction []float64, currentLoss, startScale float64) (*scene.Scene, float64, bool, float64, error) {
	factor := startScale
	if factor <= 0 {
		factor = 1
	}
	minFactor := r.config.MinStep / r.config.LearningRate
	var lastParamErr error
	for attempt := 0; attempt < r.config.MaxBacktracks; attempt++ {
		candidateValues := make([]float64, len(values))
		for i := range values {
			candidateValues[i] = values[i] - direction[i]*r.config.LearningRate*factor
		}
		for i, v := range candidateValues {
			if !finite(v) {
				return nil, 0, false, factor, errs.New(errs.CodeGradient, "<optim>", r.specs[i].ID, "candidate parameter is non-finite")
			}
		}
		candidate := current.Clone()
		if err := render.ApplyParameterValues(candidate, r.specs, candidateValues); err != nil {
			if e, ok := errs.As(err); ok && e.Code == errs.CodeParam {
				lastParamErr = err
				factor *= 0.5
				if factor < minFactor {
					break
				}
				continue
			}
			return nil, 0, false, factor, err
		}
		batch, err := r.evaluate(candidate)
		if err != nil {
			return nil, 0, false, factor, err
		}
		if !finite(batch.Loss) {
			return nil, 0, false, factor, errs.New(errs.CodeGradient, "<optim>", "loss", "candidate loss is non-finite")
		}
		if batch.Loss <= currentLoss {
			return candidate, batch.Loss, true, factor, nil
		}
		factor *= 0.5
		if factor < minFactor {
			break
		}
	}
	if lastParamErr != nil {
		if e, ok := errs.As(lastParamErr); ok {
			clone := *e
			clone.Message = "no in-bounds candidate was accepted during line search: " + e.Message
			return nil, 0, false, factor, &clone
		}
	}
	return nil, 0, false, factor, nil
}

func preconditionDirection(specs []render.ParamSpec, grad []float64) []float64 {
	groups := make(map[string]float64)
	keys := make([]string, len(specs))
	for i, spec := range specs {
		key := groupKey(spec)
		keys[i] = key
		if a := math.Abs(grad[i]); a > groups[key] {
			groups[key] = a
		}
	}
	dir := make([]float64, len(grad))
	for i, spec := range specs {
		denom := groups[keys[i]]
		if denom <= 1e-30 {
			continue
		}
		dir[i] = grad[i] / denom * stepUnit(spec)
	}
	return dir
}

func groupKey(spec render.ParamSpec) string {
	switch {
	case spec.Light >= 0:
		return fmt.Sprintf("light:%d", spec.Light)
	case spec.Plane >= 0:
		return fmt.Sprintf("plane-albedo:%d", spec.Plane)
	case spec.Kind == render.ParamAlbedoR || spec.Kind == render.ParamAlbedoG || spec.Kind == render.ParamAlbedoB:
		return fmt.Sprintf("sphere-albedo:%d", spec.Sphere)
	default:
		return fmt.Sprintf("sphere-geometry:%d", spec.Sphere)
	}
}

func stepUnit(spec render.ParamSpec) float64 {
	if spec.Light >= 0 {
		return 20
	}
	if spec.Plane >= 0 {
		return 0.5
	}
	if spec.Kind == render.ParamAlbedoR || spec.Kind == render.ParamAlbedoG || spec.Kind == render.ParamAlbedoB {
		return 0.7
	}
	return 1.0
}

func (r *Runner) commit(window int, s *scene.Scene, state State, losses []float64, acc []BlockGradient) error {
	params := render.ParameterValues(s, r.specs)
	ids := make([]string, len(r.specs))
	for i, spec := range r.specs {
		ids[i] = spec.ID
	}
	cp := &Checkpoint{
		Window: window, State: state, Scene: s.Clone(), Parameters: params, ParameterIDs: ids,
		BlockGradients: cloneBlockGradients(acc), Losses: append([]float64(nil), losses...),
	}
	return r.store.Commit(cp)
}

func accumulateBlockGradients(acc []BlockGradient, blocks []render.BlockResult) ([]BlockGradient, error) {
	if len(blocks) == 0 {
		return acc, nil
	}
	if len(acc) == 0 {
		acc = make([]BlockGradient, len(blocks))
		for i, block := range blocks {
			acc[i] = BlockGradient{
				View: block.View, Index: block.Index, Start: block.Start, End: block.End,
				Values: make([]float64, len(block.Gradient)),
			}
		}
	}
	if len(acc) != len(blocks) {
		return acc, errs.New(errs.CodeGradient, "<optim>", "blockGradients", "block count changed from %d to %d", len(acc), len(blocks))
	}
	for i, block := range blocks {
		if len(block.Gradient) != len(acc[i].Values) {
			return acc, errs.New(errs.CodeGradient, "<optim>", "blockGradients", "block %d gradient length changed", i)
		}
		for p, v := range block.Gradient {
			acc[i].Values[p] += v
		}
	}
	return acc, nil
}

func cloneBlockGradients(in []BlockGradient) []BlockGradient {
	out := make([]BlockGradient, len(in))
	for i, bg := range in {
		out[i] = bg
		out[i].Values = append([]float64(nil), bg.Values...)
	}
	return out
}

func checkFiniteGradient(grad []float64, ids []string) error {
	for i, v := range grad {
		if !finite(v) {
			field := fmt.Sprintf("gradient[%d]", i)
			if i < len(ids) {
				field = ids[i]
			}
			return errs.New(errs.CodeGradient, "<optim>", field, "gradient is non-finite")
		}
	}
	return nil
}

func norm2(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	return math.Sqrt(sum)
}

func cloneImages(in []render.Image) []render.Image {
	out := make([]render.Image, len(in))
	for i, im := range in {
		out[i] = im.Clone()
	}
	return out
}

// TaskHash binds a checkpoint to the canonical initial scene and target bytes.
func TaskHash(initial *scene.Scene, targets []render.Image) (string, error) {
	exported, err := scene.Export(initial, scene.ExportOptions{Version: scene.Version3, AllowLossy: true})
	if err != nil {
		return "", err
	}
	doc := struct {
		SourceVersion string   `json:"sourceVersion"`
		Scene         []byte   `json:"scene"`
		Targets       [][]byte `json:"targets"`
	}{SourceVersion: initial.SourceVersion, Scene: exported.Data}
	for _, im := range targets {
		b, err := render.EncodeTarget(im)
		if err != nil {
			return "", err
		}
		doc.Targets = append(doc.Targets, b)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeCheckpt, "<task>", "hash", "cannot hash task")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ConfigHash excludes only execution controls and test hooks.
func ConfigHash(cfg Config) (string, error) {
	doc := struct {
		LearningRate  float64 `json:"learningRate"`
		Iterations    int     `json:"iterations"`
		GradClip      float64 `json:"gradClip"`
		Tolerance     float64 `json:"tolerance"`
		WindowSize    int     `json:"windowSize"`
		MaxBacktracks int     `json:"maxBacktracks"`
		MinStep       float64 `json:"minStep"`
		BlockPixels   int     `json:"blockPixels"`
		Retries       int     `json:"retries"`
	}{
		cfg.LearningRate, cfg.Iterations, cfg.GradClip, cfg.Tolerance, cfg.WindowSize,
		cfg.MaxBacktracks, cfg.MinStep, cfg.Render.BlockPixels, cfg.Render.Retries,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeCheckpt, "<config>", "hash", "cannot hash config")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// SortedParameterIDs is a helper for stable report rendering.
func SortedParameterIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

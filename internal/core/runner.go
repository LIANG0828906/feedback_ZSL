package core

import (
	"context"
	"encoding/json"
	"sort"
)

type WindowCheckpoint struct {
	ScenarioID        string
	WindowIndex       int
	StartTick         int64
	EndTick           int64
	StartIntentOffset int
	EndIntentOffset   int
	State             *State
	StateJSON         []byte
	StateHash         string
	ParamsVersion     string
	SceneHash         string
	AlgorithmVersion  string
}

type WindowFailure struct {
	ScenarioID  string
	WindowIndex int
	StartTick   int64
	EndTick     int64
	Code        Code
	Message     string
	Retryable   bool
}

type ResumePoint struct {
	State      *State
	NextWindow int
	Found      bool
	Reset      bool
}

type CheckpointStore interface {
	LoadResume(ctx context.Context, scene *Scenario) (ResumePoint, error)
	CommitWindow(ctx context.Context, cp WindowCheckpoint) error
	RecordFailure(ctx context.Context, failure WindowFailure) error
}

type WindowOptions struct {
	WindowSize       int64
	StopAfterWindows int
	SampleTicks      []int64
}

type WindowResult struct {
	Index             int      `json:"index"`
	StartTick         int64    `json:"start_tick"`
	EndTick           int64    `json:"end_tick"`
	StartIntentOffset int      `json:"start_intent_offset"`
	EndIntentOffset   int      `json:"end_intent_offset"`
	Snapshot          Snapshot `json:"snapshot"`
}

type WindowRunResult struct {
	ScenarioID string         `json:"scenario_id"`
	Resumed    bool           `json:"resumed"`
	Reset      bool           `json:"reset"`
	Stopped    bool           `json:"stopped"`
	Final      Snapshot       `json:"final"`
	Windows    []WindowResult `json:"windows"`
	Snapshots  []Snapshot     `json:"snapshots"`
	State      *State         `json:"-"`
}

func RunWindowed(ctx context.Context, scene *Scenario, sink CheckpointStore, opts WindowOptions) (*WindowRunResult, error) {
	if opts.WindowSize <= 0 {
		return nil, NewError(CodeParam, "window size must be positive").WithField("window_size")
	}
	if opts.StopAfterWindows < 0 {
		return nil, NewError(CodeParam, "StopAfterWindows must be non-negative").WithField("stop_after_windows")
	}
	for _, tick := range opts.SampleTicks {
		if tick < 0 || tick > scene.FinalTick {
			return nil, NewError(CodeParam, "sample tick is outside the scenario").
				WithField("sample_ticks").WithValues(tick, scene.FinalTick)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, NewError(CodeCancel, "settlement cancelled before start").Wrap(err)
	}

	result := &WindowRunResult{ScenarioID: scene.ID}
	var engine *Engine
	var nextWindow int
	if sink != nil {
		resume, err := sink.LoadResume(ctx, scene)
		if err != nil {
			return nil, err
		}
		result.Reset = resume.Reset
		if resume.Found && resume.State != nil {
			var e error
			engine, e = NewEngineAt(scene, resume.State)
			if e != nil {
				return nil, e
			}
			nextWindow = resume.NextWindow
			result.Resumed = true
		}
	}
	if engine == nil {
		var e error
		engine, e = NewEngine(scene)
		if e != nil {
			return nil, e
		}
	}
	startTick := engine.State().Tick
	if nextWindow > 0 {
		expectedStart := int64(nextWindow) * opts.WindowSize
		finalPartialComplete := startTick == scene.FinalTick && expectedStart > scene.FinalTick
		if startTick != expectedStart && !finalPartialComplete {
			return nil, NewError(CodeCheckpt, "checkpoint boundary does not match window size").
				WithValues(startTick, expectedStart)
		}
	}

	samples := make(map[int64]Snapshot)
	sampleSet := make(map[int64]struct{}, len(opts.SampleTicks))
	for _, t := range opts.SampleTicks {
		sampleSet[t] = struct{}{}
	}
	if _, ok := sampleSet[startTick]; ok {
		samples[startTick] = engine.Snapshot()
	}

	startedWindows := 0
	for engine.State().Tick < scene.FinalTick {
		if opts.StopAfterWindows > 0 && startedWindows >= opts.StopAfterWindows {
			result.Stopped = true
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, NewError(CodeCancel, "settlement cancelled between windows").Wrap(err)
		}
		startState := engine.CloneState()
		startTick := startState.Tick
		endTick := startTick + opts.WindowSize
		if endTick > scene.FinalTick {
			endTick = scene.FinalTick
		}
		windowIndex := nextWindow
		if windowIndex == 0 {
			windowIndex = int(startTick / opts.WindowSize)
		}
		for engine.State().Tick < endTick {
			if err := ctx.Err(); err != nil {
				engine.Restore(startState)
				return nil, NewError(CodeCancel, "settlement cancelled inside window").
					WithValues(windowIndex, engine.State().Tick).Wrap(err)
			}
			if err := engine.Step(); err != nil {
				engine.Restore(startState)
				if sink != nil {
					failure := WindowFailure{
						ScenarioID:  scene.ID,
						WindowIndex: windowIndex,
						StartTick:   startTick,
						EndTick:     endTick,
						Code:        CodeOf(err),
						Message:     err.Error(),
						Retryable:   true,
					}
					if recordErr := sink.RecordFailure(ctx, failure); recordErr != nil {
						return nil, recordErr
					}
				}
				return nil, err
			}
			tick := engine.State().Tick
			if _, ok := sampleSet[tick]; ok {
				samples[tick] = engine.Snapshot()
			}
		}
		snap := engine.Snapshot()
		window := WindowResult{
			Index:             windowIndex,
			StartTick:         startTick,
			EndTick:           endTick,
			StartIntentOffset: startState.IntentOffset,
			EndIntentOffset:   engine.State().IntentOffset,
			Snapshot:          snap,
		}
		if sink != nil {
			stateBytes, err := engine.State().MarshalCanonical()
			if err != nil {
				return nil, NewError(CodeState, "failed to encode checkpoint state").Wrap(err)
			}
			cp := WindowCheckpoint{
				ScenarioID:        scene.ID,
				WindowIndex:       windowIndex,
				StartTick:         startTick,
				EndTick:           endTick,
				StartIntentOffset: startState.IntentOffset,
				EndIntentOffset:   engine.State().IntentOffset,
				State:             engine.CloneState(),
				StateJSON:         stateBytes,
				StateHash:         HashBytes(stateBytes),
				ParamsVersion:     scene.ParamsVersion,
				SceneHash:         scene.SceneHash,
				AlgorithmVersion:  AlgorithmVersion,
			}
			if err := sink.CommitWindow(ctx, cp); err != nil {
				return nil, err
			}
		}
		result.Windows = append(result.Windows, window)
		nextWindow = windowIndex + 1
		startedWindows++
	}

	final := engine.Snapshot()
	result.Final = final
	if _, ok := sampleSet[final.Tick]; ok {
		samples[final.Tick] = final
	}
	for _, tick := range opts.SampleTicks {
		if snap, ok := samples[tick]; ok {
			result.Snapshots = append(result.Snapshots, snap)
		}
	}
	sort.Slice(result.Snapshots, func(i, j int) bool {
		return result.Snapshots[i].Tick < result.Snapshots[j].Tick
	})
	result.State = engine.CloneState()
	return result, nil
}

func RunFull(scene *Scenario, sampleTicks []int64) (*WindowRunResult, error) {
	engine, err := NewEngine(scene)
	if err != nil {
		return nil, err
	}
	sampleSet := make(map[int64]struct{}, len(sampleTicks))
	for _, t := range sampleTicks {
		if t < 0 || t > scene.FinalTick {
			return nil, NewError(CodeParam, "sample tick is outside the scenario").WithField("sample_ticks")
		}
		sampleSet[t] = struct{}{}
	}
	result := &WindowRunResult{ScenarioID: scene.ID}
	byTick := make(map[int64]Snapshot)
	if _, ok := sampleSet[0]; ok {
		byTick[0] = engine.Snapshot()
	}
	for engine.State().Tick < scene.FinalTick {
		if err := engine.Step(); err != nil {
			return nil, err
		}
		if _, ok := sampleSet[engine.State().Tick]; ok {
			byTick[engine.State().Tick] = engine.Snapshot()
		}
	}
	for _, tick := range sampleTicks {
		if snap, ok := byTick[tick]; ok {
			result.Snapshots = append(result.Snapshots, snap)
		}
	}
	sort.Slice(result.Snapshots, func(i, j int) bool {
		return result.Snapshots[i].Tick < result.Snapshots[j].Tick
	})
	result.Final = engine.Snapshot()
	result.State = engine.CloneState()
	return result, nil
}

type PreviewPoint struct {
	Tick     int64    `json:"tick"`
	Step     int64    `json:"step"`
	Snapshot Snapshot `json:"snapshot"`
}

type PreviewResult struct {
	Step       int64          `json:"step"`
	CutoffTick int64          `json:"cutoff_tick"`
	Points     []PreviewPoint `json:"points"`
	Final      Snapshot       `json:"final"`
}

// RunPreview intentionally integrates the same one-tick deterministic unit as
// exact settlement, but only publishes coarse-grained checkpoints. This keeps
// preview and exact endpoint states byte-for-byte comparable while preserving
// the large reporting step requested by the editor.
func RunPreview(scene *Scenario, step int64, sampleTicks []int64, cutoff int64) (*PreviewResult, error) {
	if step <= 0 || step > 1<<20 {
		return nil, NewError(CodeParam, "preview step is outside 1..1048576").WithField("step")
	}
	if cutoff < 0 || cutoff > scene.FinalTick {
		return nil, NewError(CodeParam, "preview cutoff is outside the scenario").WithField("cutoff_tick")
	}
	for _, t := range sampleTicks {
		if t < 0 || t > cutoff {
			return nil, NewError(CodeParam, "preview sample tick exceeds cutoff").WithField("sample_ticks")
		}
	}
	engine, err := NewEngine(scene)
	if err != nil {
		return nil, err
	}
	sampleSet := make(map[int64]struct{}, len(sampleTicks))
	for _, t := range sampleTicks {
		sampleSet[t] = struct{}{}
	}
	points := make(map[int64]PreviewPoint)
	if _, ok := sampleSet[0]; ok {
		points[0] = PreviewPoint{Tick: 0, Step: step, Snapshot: engine.Snapshot()}
	}
	nextReport := step
	for engine.State().Tick < cutoff {
		chunkEnd := engine.State().Tick + step
		if chunkEnd > cutoff {
			chunkEnd = cutoff
		}
		for engine.State().Tick < chunkEnd {
			if err := engine.Step(); err != nil {
				return nil, err
			}
			tick := engine.State().Tick
			if _, ok := sampleSet[tick]; ok {
				points[tick] = PreviewPoint{Tick: tick, Step: step, Snapshot: engine.Snapshot()}
			}
		}
		if engine.State().Tick == nextReport || engine.State().Tick == cutoff {
			tick := engine.State().Tick
			points[tick] = PreviewPoint{Tick: tick, Step: step, Snapshot: engine.Snapshot()}
		}
		nextReport += step
	}
	result := &PreviewResult{Step: step, CutoffTick: cutoff, Final: engine.Snapshot()}
	for _, p := range points {
		result.Points = append(result.Points, p)
	}
	sort.Slice(result.Points, func(i, j int) bool { return result.Points[i].Tick < result.Points[j].Tick })
	return result, nil
}

func (cp *WindowCheckpoint) Validate() error {
	if cp.ScenarioID == "" || cp.WindowIndex < 0 {
		return NewError(CodeCheckpt, "checkpoint identity is invalid")
	}
	if cp.StartTick < 0 || cp.EndTick <= cp.StartTick {
		return NewError(CodeCheckpt, "checkpoint tick range is invalid")
	}
	if cp.State == nil || cp.State.Tick != cp.EndTick {
		return NewError(CodeCheckpt, "checkpoint state tick does not match window end")
	}
	if cp.State.IntentOffset != cp.EndIntentOffset {
		return NewError(CodeCheckpt, "checkpoint intent offset does not match state")
	}
	if len(cp.StateJSON) == 0 || HashBytes(cp.StateJSON) != cp.StateHash {
		return NewError(CodeCheckpt, "checkpoint state hash is invalid")
	}
	if cp.ParamsVersion == "" || cp.SceneHash == "" || cp.AlgorithmVersion != AlgorithmVersion {
		return NewError(CodeCheckpt, "checkpoint version fields are incomplete")
	}
	var decoded State
	if err := json.Unmarshal(cp.StateJSON, &decoded); err != nil {
		return NewError(CodeCheckpt, "checkpoint state payload is invalid").Wrap(err)
	}
	if decoded.Tick != cp.State.Tick || decoded.IntentOffset != cp.State.IntentOffset {
		return NewError(CodeCheckpt, "checkpoint state payload does not match state")
	}
	canonical, err := cp.State.MarshalCanonical()
	if err != nil || HashBytes(canonical) != cp.StateHash {
		return NewError(CodeCheckpt, "checkpoint state payload is not the canonical state")
	}
	return nil
}

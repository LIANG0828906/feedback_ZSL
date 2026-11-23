package gait

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func testDataset(t *testing.T) Dataset {
	t.Helper()
	ds, err := GenerateDataset(DefaultGeneratorConfig(42))
	if err != nil {
		t.Fatalf("generate dataset: %v", err)
	}
	return ds
}

func testInputs(t *testing.T) (Dataset, SourceLoop, PhaseTrack, Truth, TargetSkeleton) {
	t.Helper()
	ds := testDataset(t)
	source, phases, truth, err := SelectSource(ds, "src-walk-001")
	if err != nil {
		t.Fatal(err)
	}
	target, err := SelectTarget(ds, "target-compact-01")
	if err != nil {
		t.Fatal(err)
	}
	return ds, source, phases, truth, target
}

func cloneDataset(t *testing.T, ds Dataset) Dataset {
	t.Helper()
	data, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	var clone Dataset
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func cloneResult(t *testing.T, result Result) Result {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var clone Result
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func requireCode(t *testing.T, err error, code string) *AppError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var app *AppError
	if !errors.As(err, &app) {
		t.Fatalf("expected AppError %s, got %T: %v", code, err, err)
	}
	if app.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, app.Code, err)
	}
	return app
}

func TestDeterministicGeneration(t *testing.T) {
	cfg := DefaultGeneratorConfig(12345)
	left, err := GenerateDataset(cfg)
	if err != nil {
		t.Fatal(err)
	}
	right, err := GenerateDataset(cfg)
	if err != nil {
		t.Fatal(err)
	}
	leftDir, rightDir := t.TempDir(), t.TempDir()
	if err := WriteDataset(leftDir, left); err != nil {
		t.Fatal(err)
	}
	if err := WriteDataset(rightDir, right); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sources.json", "phases.json", "targets.json", "truths.json", "params.json"} {
		a, err := os.ReadFile(filepath.Join(leftDir, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(rightDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
	loaded, err := LoadDataset(leftDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Sources) != 2 || len(loaded.Targets) != 6 || len(loaded.Truths) != 2 {
		t.Fatalf("unexpected generated counts: sources=%d targets=%d truths=%d",
			len(loaded.Sources), len(loaded.Targets), len(loaded.Truths))
	}
}

func TestValidationErrorCodes(t *testing.T) {
	t.Run("E_SCHEMA", func(t *testing.T) {
		ds := testDataset(t)
		ds.Sources[0].ID = ""
		app := requireCode(t, ValidateDataset(ds), CodeSchema)
		if app.Frame < 0 || app.Field == "" {
			t.Fatalf("schema error lacks location: %+v", app)
		}
	})
	t.Run("E_CYCLE", func(t *testing.T) {
		ds := testDataset(t)
		for i := range ds.Sources[0].Skeleton.Nodes {
			if ds.Sources[0].Skeleton.Nodes[i].Name == "hip_L" {
				ds.Sources[0].Skeleton.Nodes[i].Parent = "toe_L_2"
				break
			}
		}
		requireCode(t, ValidateDataset(ds), CodeCycle)
	})
	t.Run("E_CYCLE root self reference", func(t *testing.T) {
		ds := testDataset(t)
		for i := range ds.Sources[0].Skeleton.Nodes {
			if ds.Sources[0].Skeleton.Nodes[i].Name == "root" {
				ds.Sources[0].Skeleton.Nodes[i].Parent = "root"
				break
			}
		}
		requireCode(t, ValidateDataset(ds), CodeCycle)
	})
	t.Run("E_PHASE coverage", func(t *testing.T) {
		ds := testDataset(t)
		track := &ds.Phases[0]
		track.Frames = track.Frames[:len(track.Frames)-1]
		app := requireCode(t, ValidateDataset(ds), CodePhase)
		if app.Frame != ds.Sources[0].FrameCount-1 {
			t.Fatalf("phase coverage frame=%d want=%d", app.Frame, ds.Sources[0].FrameCount-1)
		}
		if app.Cycle != 2 {
			t.Fatalf("phase coverage cycle=%d want=2", app.Cycle)
		}
	})
	t.Run("E_PHASE illegal transition", func(t *testing.T) {
		ds := testDataset(t)
		// stance -> swing is illegal; double support must separate them.
		ds.Phases[0].Frames[10].Global = PhaseSwing
		app := requireCode(t, ValidateDataset(ds), CodePhase)
		if app.Frame != 11 {
			t.Fatalf("phase transition frame=%d want=11", app.Frame)
		}
	})
	t.Run("truth frame count", func(t *testing.T) {
		ds := testDataset(t)
		ds.Truths[0].FrameCount++
		app := requireCode(t, ValidateDataset(ds), CodeSchema)
		if app.Field != "truth.frame_count" {
			t.Fatalf("field=%q want truth.frame_count", app.Field)
		}
	})
}

func TestInjectionErrorCodes(t *testing.T) {
	t.Run("E_JUMP", func(t *testing.T) {
		ds := testDataset(t)
		ds.Sources[0].Feet[0].Frames[10].Position.X += 2
		app := requireCode(t, ValidateDataset(ds), CodeJump)
		if app.Frame != 10 || app.Cycle != 0 || app.Foot != "left" {
			t.Fatalf("jump location cycle=%d frame=%d foot=%q", app.Cycle, app.Frame, app.Foot)
		}
	})
	t.Run("E_RATIO", func(t *testing.T) {
		ds := testDataset(t)
		ds.Targets[0].LegLengthRatio = 0
		app := requireCode(t, ValidateDataset(ds), CodeRatio)
		if app.Field != "leg_length_ratio" || app.Cycle != 0 || app.Frame != 0 {
			t.Fatalf("ratio location cycle=%d frame=%d field=%q", app.Cycle, app.Frame, app.Field)
		}
	})
	t.Run("E_DUR", func(t *testing.T) {
		ds := testDataset(t)
		ds.Targets[0].CycleFrames[0] = 0
		app := requireCode(t, ValidateDataset(ds), CodeDuration)
		if app.Cycle != 0 || app.Field != "cycle_frames" {
			t.Fatalf("duration location cycle=%d field=%q", app.Cycle, app.Field)
		}
	})
	t.Run("E_PARA", func(t *testing.T) {
		ds := testDataset(t)
		ds.Params.ContactHeight = 0
		app := requireCode(t, ValidateDataset(ds), CodePara)
		if app.Field != "contact_height" {
			t.Fatalf("field=%q want contact_height", app.Field)
		}
	})
}

func TestMappingTraceAndAlignment(t *testing.T) {
	ds, source, phases, _, target := testInputs(t)
	mapping, err := BuildMapping(source, phases, target, ds.Params)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping.Cycles) != len(source.Cycles) {
		t.Fatalf("mapped cycles=%d want=%d", len(mapping.Cycles), len(source.Cycles))
	}
	for i, cycle := range mapping.Cycles {
		if len(cycle.SourceFrameMap) != target.CycleFrames[i] {
			t.Fatalf("cycle %d frame map length=%d want=%d", i, len(cycle.SourceFrameMap), target.CycleFrames[i])
		}
		for _, field := range []struct {
			name      string
			source    float64
			target    float64
			tolerance float64
		}{
			{"stance", cycle.SourceProportions.Stance, cycle.TargetProportions.Stance, ds.Params.PhaseRatioTolerance},
			{"swing", cycle.SourceProportions.Swing, cycle.TargetProportions.Swing, ds.Params.PhaseRatioTolerance},
			{"double support", cycle.SourceProportions.DoubleSupport, cycle.TargetProportions.DoubleSupport, ds.Params.PhaseRatioTolerance},
		} {
			if abs(field.source-field.target) > field.tolerance+1e-12 {
				t.Fatalf("cycle %d %s proportion drift %.8f", i, field.name, abs(field.source-field.target))
			}
		}
		for _, sourceFrame := range []int{cycle.SourceStartFrame, (cycle.SourceStartFrame + cycle.SourceEndFrame) / 2, cycle.SourceEndFrame} {
			mappedFrame, err := mapping.QuerySourceFrame(i, sourceFrame)
			if err != nil {
				t.Fatal(err)
			}
			if mappedFrame < cycle.TargetStartFrame || mappedFrame > cycle.TargetEndFrame {
				t.Fatalf("query source frame %d -> %d outside target cycle %d..%d",
					sourceFrame, mappedFrame, cycle.TargetStartFrame, cycle.TargetEndFrame)
			}
		}
	}

	bad := target
	bad.CycleFrames = append([]int(nil), target.CycleFrames...)
	bad.CycleFrames[0] = 2
	_, err = BuildMapping(source, phases, bad, ds.Params)
	app := requireCode(t, err, CodeAlign)
	if len(app.Conflicts) == 0 || app.Conflicts[0].Cycle != 0 {
		t.Fatalf("alignment error lacks conflict/proportions: %+v", app)
	}
}

func TestFullRetargetInvariantsAndHeightError(t *testing.T) {
	ds, source, phases, truth, target := testInputs(t)
	result, _, err := FullRetarget(source, phases, truth, target, ds.Params)
	if err != nil {
		t.Fatal(err)
	}
	if result.FrameCount != 138 {
		t.Fatalf("frame count=%d want=138", result.FrameCount)
	}
	if len(result.Events) != len(truth.FootEvents) {
		t.Fatalf("events=%d truth events=%d", len(result.Events), len(truth.FootEvents))
	}
	toeCounts := make(map[string]int)
	for _, leg := range target.Legs {
		toeCounts[leg.Foot] = len(leg.Toes)
	}
	for _, frame := range result.Frames {
		for _, foot := range frame.Feet {
			if len(foot.Joints) != 3 {
				t.Fatalf("frame %d foot %s joint count=%d", frame.Frame, foot.Foot, len(foot.Joints))
			}
			if len(foot.Toes) != toeCounts[foot.Foot] {
				t.Fatalf("frame %d foot %s toe count=%d want=%d", frame.Frame, foot.Foot, len(foot.Toes), toeCounts[foot.Foot])
			}
			if foot.Contact {
				if abs(foot.Position.Y-target.Skeleton.GroundPlane) > 1e-9 {
					t.Fatalf("frame %d foot %s stance height %.9f", frame.Frame, foot.Foot, foot.Position.Y)
				}
				if foot.Speed.HorizontalLength() > ds.Params.StanceSlideMax+1e-12 {
					t.Fatalf("frame %d foot %s slide %.9f exceeds %.9f",
						frame.Frame, foot.Foot, foot.Speed.HorizontalLength(), ds.Params.StanceSlideMax)
				}
			}
		}
	}
	if !sort.SliceIsSorted(result.Events, func(i, j int) bool {
		if result.Events[i].Frame != result.Events[j].Frame {
			return result.Events[i].Frame < result.Events[j].Frame
		}
		if result.Events[i].Foot != result.Events[j].Foot {
			return result.Events[i].Foot < result.Events[j].Foot
		}
		return result.Events[i].Type < result.Events[j].Type
	}) {
		t.Fatal("event timeline is not sorted")
	}

	lowReach := target
	lowReach.FootReachable.Max = 0.01
	_, _, err = FullRetarget(source, phases, truth, lowReach, ds.Params)
	app := requireCode(t, err, CodeHeight)
	if app.Frame < 0 || app.Foot == "" || app.Field != "foot_reachable_height" {
		t.Fatalf("height error lacks location: %+v", app)
	}
}

func TestCycleStateMachine(t *testing.T) {
	ds, source, phases, truth, target := testInputs(t)
	engine, err := NewEngine(source, phases, truth, target, ds.Params, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.TransitionCycle(0, CycleCommitted); err == nil || CodeOf(err) != CodeState {
		t.Fatalf("pending -> committed should be E_STATE, got %v", err)
	}
	if state, _ := engine.StateOf(0); state != CyclePending {
		t.Fatalf("illegal transition changed state to %s", state)
	}
	for _, next := range []CycleState{CycleAligning, CycleSolving, CycleCommitted} {
		if err := engine.TransitionCycle(0, next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	if err := engine.TransitionCycle(0, CycleAligning); err == nil || CodeOf(err) != CodeState {
		t.Fatalf("committed cycle transition should be E_STATE, got %v", err)
	}
	if state, _ := engine.StateOf(0); state != CycleCommitted {
		t.Fatalf("committed cycle changed state to %s", state)
	}
	if err := engine.TransitionCycle(1, CycleFailed); err != nil {
		t.Fatal(err)
	}
	if err := engine.TransitionCycle(1, CycleAligning); err != nil {
		t.Fatalf("failed -> aligning retry transition: %v", err)
	}
	status := engine.Status()
	if status.Total != len(source.Cycles) || status.Committed != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestPauseBeforeCycleBoundary(t *testing.T) {
	ds, source, phases, truth, target := testInputs(t)
	engine, err := NewEngine(source, phases, truth, target, ds.Params, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	engine.Pause()
	if err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := engine.Status(); status.Committed != 0 || !status.Paused {
		t.Fatalf("paused engine made progress: %+v", status)
	}
	engine.Resume()
	if err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := engine.Status(); status.Committed != status.Total {
		t.Fatalf("resumed engine progress: %+v", status)
	}
}

func TestCheckpointResumeInvalidationAndFreeze(t *testing.T) {
	ds, source, phases, truth, target := testInputs(t)
	full, _, err := FullRetarget(source, phases, truth, target, ds.Params)
	if err != nil {
		t.Fatal(err)
	}
	storeDir := t.TempDir()
	store := NewSnapshotStore(storeDir)
	first, err := NewEngine(source, phases, truth, target, ds.Params, EngineOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.RunCycles(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if status := first.Status(); status.Committed != 1 {
		t.Fatalf("committed=%d want=1", status.Committed)
	}
	if _, err := os.Stat(filepath.Join(storeDir, SnapshotNameForCycle(0))); err != nil {
		t.Fatalf("cycle 0 checkpoint missing: %v", err)
	}
	second, err := NewEngine(source, phases, truth, target, ds.Params, EngineOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if loaded := second.SnapshotReport().LoadedCycles; len(loaded) != 1 || loaded[0] != 0 {
		t.Fatalf("loaded cycles=%v want=[0]", loaded)
	}
	if err := second.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := second.Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(full, resumed); err != nil {
		t.Fatalf("resumed result differs: %v", err)
	}

	path := filepath.Join(storeDir, SnapshotNameForCycle(0))
	var env SnapshotEnvelope
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	env.Payload.Frames[0].Root.X += 1
	tampered, _ := json.MarshalIndent(env, "", "  ")
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := NewEngine(source, phases, truth, target, ds.Params, EngineOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	report := third.SnapshotReport()
	if !report.Invalidated {
		t.Fatal("tampered checkpoint was not invalidated")
	}
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, ok := ParseSnapshotCycle(entry.Name()); ok {
			t.Fatalf("snapshot %s survived invalidation", entry.Name())
		}
	}
	if err := third.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	recomputed, err := third.Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(full, recomputed); err != nil {
		t.Fatalf("full recomputation after invalidation differs: %v", err)
	}

	versionStore := t.TempDir()
	versionEngine, err := NewEngine(source, phases, truth, target, ds.Params,
		EngineOptions{Store: NewSnapshotStore(versionStore)})
	if err != nil {
		t.Fatal(err)
	}
	if err := versionEngine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed := ds.Params
	changed.ParameterVersion = "different-parameter-version"
	versionCheck, err := NewEngine(source, phases, truth, target, changed,
		EngineOptions{Store: NewSnapshotStore(versionStore)})
	if err != nil {
		t.Fatal(err)
	}
	if !versionCheck.SnapshotReport().Invalidated {
		t.Fatal("parameter version mismatch did not invalidate all snapshots")
	}
	if status := versionCheck.Status(); status.Committed != 0 {
		t.Fatalf("version mismatch left %d committed cycles", status.Committed)
	}
}

func TestThreePathsAndDivergence(t *testing.T) {
	ds, source, phases, truth, target := testInputs(t)
	report, err := VerifyThreePaths(context.Background(), source, phases, truth, target,
		ds.Params, t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Equal || report.ResumeLoaded != 1 {
		t.Fatalf("unexpected verification report: %+v", report)
	}
	full, mapping, err := FullRetarget(source, phases, truth, target, ds.Params)
	if err != nil {
		t.Fatal(err)
	}
	extraEvent := cloneResult(t, full)
	extraEvent.Events = append(extraEvent.Events, FootEvent{
		Frame: 5, Cycle: 0, Foot: "left", Type: EventLand,
	})
	err = ValidateEventTimeline(extraEvent, truth, mapping, ds.Params)
	app := requireCode(t, err, CodeDiverge)
	if app.Frame != 5 || app.Field != "event.land" || app.Foot != "left" {
		t.Fatalf("unexpected event timeline divergence: %+v", app)
	}
	left := cloneResult(t, full)
	right := cloneResult(t, full)
	right.Frames[5].Root.X += 1
	err = CompareResults(left, right)
	app = requireCode(t, err, CodeDiverge)
	if app.Frame != 5 || app.Field != "root.x" {
		t.Fatalf("divergence location frame=%d field=%q", app.Frame, app.Field)
	}

	right = cloneResult(t, full)
	right.Events = right.Events[1:]
	err = CompareResults(left, right)
	app = requireCode(t, err, CodeDiverge)
	if app.Field != "events.count" || app.Frame != full.Events[0].Frame {
		t.Fatalf("event divergence frame=%d field=%q", app.Frame, app.Field)
	}
}

func TestFailureIsolationRetryAndCommittedFreeze(t *testing.T) {
	ds, source, phases, truth, target := testInputs(t)
	cycle := source.Cycles[1]
	for footIndex := range source.Feet {
		for frame := cycle.StartFrame; frame <= cycle.EndFrame; frame++ {
			phase := phaseForFoot(phases.Frames[frame], source.Feet[footIndex].Foot, footIndex)
			if phase == PhaseSwing {
				source.Feet[footIndex].Frames[frame].Position.Y = 0.55
			}
		}
	}
	params := ds.Params
	params.SwingLiftScale = 1.2
	storeDir := t.TempDir()
	engine, err := NewEngine(source, phases, truth, target, params, EngineOptions{Store: NewSnapshotStore(storeDir)})
	if err != nil {
		t.Fatal(err)
	}
	runErr := engine.Run(context.Background())
	app := requireCode(t, runErr, CodeHeight)
	if app.Cycle != 1 {
		t.Fatalf("height failure cycle=%d want=1", app.Cycle)
	}
	status := engine.Status()
	if status.States[0] != CycleCommitted || status.States[1] != CycleFailed || status.States[2] != CycleCommitted {
		t.Fatalf("failure polluted other cycles: %+v", status.States)
	}
	if _, err := engine.Result(); err == nil || CodeOf(err) != CodeState {
		t.Fatalf("incomplete result should be E_STATE, got %v", err)
	}
	before := map[int]string{}
	for _, cycleIndex := range []int{0, 2} {
		data, err := os.ReadFile(filepath.Join(storeDir, SnapshotNameForCycle(cycleIndex)))
		if err != nil {
			t.Fatal(err)
		}
		before[cycleIndex] = string(data)
	}

	params.SwingLiftScale = 1
	if err := engine.UpdateParams(params); err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("retry after parameter correction: %v", err)
	}
	for _, cycleIndex := range []int{0, 2} {
		data, err := os.ReadFile(filepath.Join(storeDir, SnapshotNameForCycle(cycleIndex)))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != before[cycleIndex] {
			t.Fatalf("committed checkpoint for cycle %d changed after parameter update", cycleIndex)
		}
	}
	result, err := engine.Result()
	if err != nil {
		t.Fatal(err)
	}
	if result.FrameCount != 138 {
		t.Fatalf("retry result frame count=%d", result.FrameCount)
	}
	expected, _, err := FullRetarget(source, phases, truth, target, params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected.Cycles[1], result.Cycles[1]) {
		t.Fatal("retried uncommitted cycle differs from full recompute with updated parameters")
	}
}

func TestCorrectionRecordAndConcurrency(t *testing.T) {
	ds, source, phases, truth, _ := testInputs(t)
	target, err := SelectTarget(ds, "target-human-02")
	if err != nil {
		t.Fatal(err)
	}
	phases.Frames[5].Global = PhaseStance
	full, _, err := FullRetarget(source, phases, truth, target, ds.Params)
	if err != nil {
		t.Fatalf("full retarget with local phase conflict: %v", err)
	}
	if len(full.Corrections) == 0 {
		t.Fatal("expected a correction record")
	}
	correction := full.Corrections[0]
	if correction.MappedPhase != PhaseStance || correction.DetectedPhase != PhaseDoubleSupport ||
		correction.CorrectedPhase != PhaseDoubleSupport {
		t.Fatalf("unexpected correction: %+v", correction)
	}
	report, err := VerifyThreePaths(context.Background(), source, phases, truth, target,
		ds.Params, t.TempDir(), 1)
	if err != nil {
		t.Fatalf("three-path verification with correction record: %v", err)
	}
	if !report.Equal {
		t.Fatalf("three paths differ with correction record: %+v", report)
	}

	serialParams := ds.Params
	serialParams.MaxParallelism = 1
	parallelParams := ds.Params
	parallelParams.MaxParallelism = 4
	serial, err := NewEngine(source, phases, truth, target, serialParams, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := NewEngine(source, phases, truth, target, parallelParams, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := serial.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := parallel.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	serialResult, _ := serial.Result()
	parallelResult, _ := parallel.Result()
	if err := CompareResults(serialResult, parallelResult); err != nil {
		t.Fatalf("parallel result differs from serial: %v", err)
	}

	orderedDir := t.TempDir()
	ordered, err := NewEngine(source, phases, truth, target, parallelParams,
		EngineOptions{Store: NewSnapshotStore(orderedDir)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ordered.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(orderedDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) != len(source.Cycles) {
		t.Fatalf("checkpoint file count=%d want=%d", len(names), len(source.Cycles))
	}
	for cycle := range source.Cycles {
		if names[cycle] != SnapshotNameForCycle(cycle) {
			t.Fatalf("checkpoint order at %d: got %s want %s", cycle, names[cycle], SnapshotNameForCycle(cycle))
		}
		env, err := readSnapshotEnvelope(filepath.Join(orderedDir, names[cycle]))
		if err != nil {
			t.Fatal(err)
		}
		if env.Cycle != cycle {
			t.Fatalf("checkpoint %s has cycle %d", names[cycle], env.Cycle)
		}
	}

	bufferParams := ds.Params
	bufferParams.CycleBufferLimit = 8
	_, err = NewEngine(source, phases, truth, target, bufferParams, EngineOptions{})
	requireCode(t, err, CodeBuffer)
}

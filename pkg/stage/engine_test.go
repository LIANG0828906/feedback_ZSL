package stage_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sort"
	"testing"

	"stageworkbench/pkg/generator"
	"stageworkbench/pkg/stage"
)

func tiny(t *testing.T, seed int64) *stage.Project {
	t.Helper()
	project, err := generator.Generate(generator.Options{Preset: "tiny", Seed: seed})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return project
}

func cloneProject(t *testing.T, project *stage.Project) *stage.Project {
	t.Helper()
	raw, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var out stage.Project
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func requireCode(t *testing.T, err error, code stage.ErrorCode) *stage.DomainError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	domain, ok := stage.AsDomainError(err)
	if !ok {
		t.Fatalf("expected domain error %s, got %T: %v", code, err, err)
	}
	if domain.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, domain.Code, domain)
	}
	return domain
}

func TestGeneratedTruthMatchesExactEvaluator(t *testing.T) {
	project := tiny(t, 11)
	if err := stage.ValidateProject(project, stage.DefaultValidateOptions()); err != nil {
		t.Fatalf("validate generated project: %v", err)
	}
	params := stage.DefaultParams().WithDefaults(project)
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}
	if err := stage.VerifyTruth(project, evaluator); err != nil {
		t.Fatalf("truth mismatch: %v", err)
	}
	if len(project.Truth.Samples) < 8 {
		t.Fatalf("expected useful sample truth set, got %d", len(project.Truth.Samples))
	}
	if project.Truth.TotalDurationMS != project.DurationMS {
		t.Fatalf("truth total duration mismatch")
	}
}

func TestDeterministicOutputIsIndependentOfArrayOrder(t *testing.T) {
	project := tiny(t, 12)
	params := stage.DefaultParams().WithDefaults(project)
	baseline, err := stage.NewEvaluator(project, params)
	if err != nil {
		t.Fatal(err)
	}
	reordered := cloneProject(t, project)
	for i, j := 0, len(reordered.Lights)-1; i < j; i, j = i+1, j-1 {
		reordered.Lights[i], reordered.Lights[j] = reordered.Lights[j], reordered.Lights[i]
	}
	for i, j := 0, len(reordered.Groups)-1; i < j; i, j = i+1, j-1 {
		reordered.Groups[i], reordered.Groups[j] = reordered.Groups[j], reordered.Groups[i]
	}
	for start := 0; start < len(reordered.Cues); {
		end := start + 1
		for end < len(reordered.Cues) && reordered.Cues[end].TimeMS == reordered.Cues[start].TimeMS {
			end++
		}
		if end-start > 1 {
			for i, j := start, end-1; i < j; i, j = i+1, j-1 {
				reordered.Cues[i], reordered.Cues[j] = reordered.Cues[j], reordered.Cues[i]
			}
		}
		start = end
	}
	other, err := stage.NewEvaluator(reordered, params)
	if err != nil {
		t.Fatal(err)
	}
	if stage.ContentHash(project) != stage.ContentHash(reordered) {
		t.Fatal("canonical content hash changed after order-only transformation")
	}
	for frame := 0; frame < stage.FrameCount(project.DurationMS, project.FPS); frame += 7 {
		left := baseline.Frame(frame)
		right := other.Frame(frame)
		if err := stage.CompareFrameSequences([]stage.FrameOutput{left}, []stage.FrameOutput{right}); err != nil {
			t.Fatalf("frame %d changed after order permutation: %v", frame, err)
		}
	}
}

func TestValidationErrorCodes(t *testing.T) {
	base := tiny(t, 13)

	t.Run("schema", func(t *testing.T) {
		project := cloneProject(t, base)
		project.Lights[0].ID = ""
		requireCode(t, stage.ValidateProject(project, stage.DefaultValidateOptions()), stage.CodeSchema)
	})
	t.Run("reference", func(t *testing.T) {
		project := cloneProject(t, base)
		project.Cues[0].LightID = "missing-light"
		project.Cues[0].GroupID = ""
		requireCode(t, stage.ValidateProject(project, stage.DefaultValidateOptions()), stage.CodeReference)
	})
	t.Run("cycle", func(t *testing.T) {
		project := cloneProject(t, base)
		project.Groups[0].ParentID = project.Groups[1].ID
		project.Groups[1].ParentID = project.Groups[0].ID
		requireCode(t, stage.ValidateProject(project, stage.DefaultValidateOptions()), stage.CodeCycle)
	})
	t.Run("parameter range", func(t *testing.T) {
		project := cloneProject(t, base)
		bad := 2.0
		project.Cues[0].Values.Intensity = &bad
		project.Cues[0].LightID = project.Lights[0].ID
		project.Cues[0].GroupID = ""
		requireCode(t, stage.ValidateProject(project, stage.DefaultValidateOptions()), stage.CodeParameter)
	})
	t.Run("interpolation parameter", func(t *testing.T) {
		project := cloneProject(t, base)
		project.Cues[0].CurvePower = 8
		domain := requireCode(t, stage.ValidateProject(project, stage.DefaultValidateOptions()), stage.CodeParameter)
		if domain.Field != "curve_power" {
			t.Fatalf("expected curve_power field, got %q", domain.Field)
		}
	})
	t.Run("truth duration", func(t *testing.T) {
		project := cloneProject(t, base)
		project.Truth.TotalDurationMS++
		domain := requireCode(t, stage.ValidateProject(project, stage.DefaultValidateOptions()), stage.CodeSchema)
		if domain.Field != "total_duration_ms" {
			t.Fatalf("expected total_duration_ms locator, got %+v", domain)
		}
	})
	t.Run("monotonic timestamps", func(t *testing.T) {
		project := cloneProject(t, base)
		if len(project.Cues) < 2 {
			t.Fatal("test project needs at least two cues")
		}
		project.Cues[0].TimeMS = project.Cues[1].TimeMS + 1
		requireCode(t, stage.ValidateProject(project, stage.ValidateOptions{CheckTruth: false}), stage.CodeSchema)
	})
	t.Run("conflict", func(t *testing.T) {
		project := cloneProject(t, base)
		target := project.Lights[0].ID
		valueA, valueB := 0.1, 0.9
		project.Cues = append(project.Cues,
			stage.Cue{
				ID: "CONFLICT-A", TimeMS: 4321, LightID: target, Kind: stage.CueSet, Priority: 700,
				Easing: stage.EaseStep, CurvePower: 1, Values: stage.CueValues{Intensity: &valueA},
			},
			stage.Cue{
				ID: "CONFLICT-B", TimeMS: 4321, LightID: target, Kind: stage.CueSet, Priority: 700,
				Easing: stage.EaseStep, CurvePower: 1, Values: stage.CueValues{Intensity: &valueB},
			},
		)
		sort.SliceStable(project.Cues, func(i, j int) bool { return project.Cues[i].TimeMS < project.Cues[j].TimeMS })
		requireCode(t, stage.ValidateProject(project, stage.ValidateOptions{CheckTruth: false, CheckConflict: true}), stage.CodeConflict)
	})
}

func TestPrecedenceAndArbitrationDetails(t *testing.T) {
	ranges := stage.DefaultParamRanges()
	base := stage.LightValues{
		RGB: stage.RGB{R: 10, G: 20, B: 30}, Intensity: 0.2, Pan: 0, Tilt: 0,
		Focus: 50, ColorTemp: 5600, StrobeHz: 0,
	}
	g1, g2 := 0.45, 0.65
	single := 0.9
	project := &stage.Project{
		SchemaVersion: stage.SchemaVersion, ID: "precedence", FPS: 30, DurationMS: 1000, WindowMS: 500,
		Lights: []stage.Light{{ID: "L1", Name: "L1", Kind: stage.LightPar, Base: base, Ranges: ranges}},
		Groups: []stage.Group{
			{ID: "G1", Members: []string{"L1"}, Priority: 10},
			{ID: "G2", Members: []string{"L1"}, Priority: 20},
		},
		Cues: []stage.Cue{
			{ID: "C1", TimeMS: 0, GroupID: "G1", Kind: stage.CueGroup, Easing: stage.EaseStep, CurvePower: 1, Values: stage.CueValues{Intensity: &g1}},
			{ID: "C2", TimeMS: 0, GroupID: "G2", Kind: stage.CueGroup, Easing: stage.EaseStep, CurvePower: 1, Values: stage.CueValues{Intensity: &g2}},
			{ID: "C3", TimeMS: 0, LightID: "L1", Kind: stage.CueSet, Easing: stage.EaseStep, CurvePower: 1, Priority: 100, Values: stage.CueValues{Intensity: &single}},
		},
	}
	evaluator, err := stage.NewEvaluator(project, stage.Params{WindowMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	frame := evaluator.Frame(0)
	if got := frame.Lights[0].Intensity; math.Abs(got-single) > 1e-6 {
		t.Fatalf("single light should override groups, got intensity %v", got)
	}
	decisions := evaluator.Decisions()
	if len(decisions) != 1 {
		t.Fatalf("expected one arbitration decision, got %d", len(decisions))
	}
	if decisions[0].Rule != "single_overrides_group" {
		t.Fatalf("unexpected rule: %s", decisions[0].Rule)
	}
	if decisions[0].Winner != "cue:C3" {
		t.Fatalf("unexpected winner %s", decisions[0].Winner)
	}
}

func TestWindowStateMachineAndFrozenCommit(t *testing.T) {
	project := tiny(t, 14)
	params := stage.DefaultParams().WithDefaults(project)
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		t.Fatal(err)
	}
	specs := stage.BuildWindowSpecs(project, params)
	machine := stage.NewWindowMachine(specs)
	if err := machine.Begin(0); err != nil {
		t.Fatal(err)
	}
	domain := requireCode(t, machine.Begin(0), stage.CodeState)
	if domain.Object != "window:0" {
		t.Fatalf("expected window locator, got %q", domain.Object)
	}
	if state, _ := machine.State(0); state != stage.WindowScheduling {
		t.Fatalf("illegal transition changed state to %s", state)
	}
	if err := machine.Fail(0, errors.New("test fault")); err != nil {
		t.Fatal(err)
	}
	if err := machine.Begin(0); err != nil {
		t.Fatalf("retry failed window: %v", err)
	}
	window, err := stage.ComputeWindow(project, evaluator, specs[0], stage.ModeDecision{Mode: stage.ModeExact})
	if err != nil {
		t.Fatal(err)
	}
	broken := window
	broken.CueOffset++
	requireCode(t, machine.Commit(broken), stage.CodeState)
	if state, _ := machine.State(0); state != stage.WindowScheduling {
		t.Fatalf("incomplete commit changed state to %s", state)
	}
	if err := machine.Commit(window); err != nil {
		t.Fatalf("commit: %v", err)
	}
	requireCode(t, machine.Commit(window), stage.CodeState)
	if state, _ := machine.State(0); state != stage.WindowCommitted {
		t.Fatalf("expected committed, got %s", state)
	}
	first, _ := machine.Result(0)
	first.CueOffset = -1
	second, _ := machine.Result(0)
	if second.CueOffset != window.CueOffset {
		t.Fatal("committed result was not frozen")
	}
}

func TestParallelAndSerialOutputsMatch(t *testing.T) {
	project := tiny(t, 15)
	dir := t.TempDir()
	run := func(workers int, name string) stage.RunResult {
		params := stage.DefaultParams()
		params.MaxWorkers = workers
		params.PerWorkerBufferBytes = 2 << 20
		runner, err := stage.NewRunner(project, params)
		if err != nil {
			t.Fatal(err)
		}
		result, err := runner.Run(context.Background(), stage.RunOptions{
			Mode: stage.RunRealtime, CheckpointPath: filepath.Join(dir, name),
		})
		if err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		return result
	}
	serial := run(1, "serial.db")
	parallel := run(4, "parallel.db")
	if err := stage.ComparePathSnapshots(serial.Snapshot(), parallel.Snapshot()); err != nil {
		t.Fatalf("parallel/serial mismatch: %v", err)
	}
}

func TestEditedProjectParallelAndSerialMatch(t *testing.T) {
	project := tiny(t, 151)
	project.Cues[8].Priority++
	project.Cues[19].TimeMS += 1
	sort.SliceStable(project.Cues, func(i, j int) bool {
		if project.Cues[i].TimeMS != project.Cues[j].TimeMS {
			return project.Cues[i].TimeMS < project.Cues[j].TimeMS
		}
		return project.Cues[i].ID < project.Cues[j].ID
	})
	if err := stage.ValidateProject(project, stage.ValidateOptions{CheckTruth: false, CheckConflict: true}); err != nil {
		t.Fatalf("edited project is invalid: %v", err)
	}
	dir := t.TempDir()
	run := func(workers int, name string) stage.RunResult {
		params := stage.DefaultParams()
		params.MaxWorkers = workers
		params.PerWorkerBufferBytes = 2 << 20
		runner, err := stage.NewRunner(project, params)
		if err != nil {
			t.Fatal(err)
		}
		result, err := runner.Run(context.Background(), stage.RunOptions{
			Mode: stage.RunRealtime, CheckpointPath: filepath.Join(dir, name), SkipTruthCheck: true,
		})
		if err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		return result
	}
	serial := run(1, "edited-serial.db")
	parallel := run(4, "edited-parallel.db")
	if err := stage.ComparePathSnapshots(serial.Snapshot(), parallel.Snapshot()); err != nil {
		t.Fatalf("edited parallel/serial mismatch: %v", err)
	}
}

func TestThreePathsConsistencyExactAndMerged(t *testing.T) {
	project := tiny(t, 16)
	for _, forced := range []stage.Mode{stage.ModeExact, stage.ModeMerged} {
		t.Run(string(forced), func(t *testing.T) {
			params := stage.DefaultParams()
			params.ForceMode = forced
			params.MaxWorkers = 3
			params.PerWorkerBufferBytes = 2 << 20
			report, err := stage.VerifyThreePaths(context.Background(), project, params, t.TempDir())
			if err != nil {
				t.Fatalf("verify failed: %v", err)
			}
			if !report.Consistent {
				t.Fatal("expected consistent report")
			}
			if report.FramesCompared <= 0 || report.WindowsCompared <= 0 {
				t.Fatalf("expected compared frames/windows, got %d/%d", report.FramesCompared, report.WindowsCompared)
			}
			if report.Mode != forced {
				t.Fatalf("expected mode %s, got %s", forced, report.Mode)
			}
		})
	}
}

func TestBudgetBlockingIsRecorded(t *testing.T) {
	project := tiny(t, 17)
	params := stage.DefaultParams()
	params.MaxWorkers = 2
	params.PerWorkerBufferBytes = 40_000
	params.MemoryBudgetBytes = 50_000
	runner, err := stage.NewRunner(project, params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunRealtime, CheckpointPath: filepath.Join(t.TempDir(), "budget.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := 0
	for _, record := range result.BudgetLog {
		if record.Event == "blocked" && record.Reason != "" && record.Required > 0 {
			blocked++
		}
	}
	if blocked == 0 {
		t.Fatalf("expected budget blocking records, got %+v", result.BudgetLog)
	}
	if result.Progress.Committed != result.Progress.Total {
		t.Fatalf("blocked queue did not eventually complete: %+v", result.Progress)
	}
}

func TestPerWorkerBufferRejectsOversizedWindow(t *testing.T) {
	project := tiny(t, 171)
	params := stage.DefaultParams()
	params.MaxWorkers = 1
	params.PerWorkerBufferBytes = 4_096
	params.MemoryBudgetBytes = 4_096
	runner, err := stage.NewRunner(project, params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunRealtime, CheckpointPath: filepath.Join(t.TempDir(), "small-buffer.db"),
	})
	domain := requireCode(t, err, stage.CodeState)
	if domain.Field != "per_worker_buffer_bytes" {
		t.Fatalf("expected per-worker buffer locator, got %+v", domain)
	}
	if len(result.Statuses) == 0 || result.Statuses[0].State != stage.WindowFailed {
		t.Fatalf("expected failed window status, got %+v", result.Statuses)
	}
}

func TestCheckpointInvalidationAndContiguousCommit(t *testing.T) {
	project := tiny(t, 18)
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	params := stage.DefaultParams().WithDefaults(project)
	store, err := stage.OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner, err := stage.NewRunner(project, params)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		t.Fatal(err)
	}
	specs := stage.BuildWindowSpecs(project, params)
	window1, err := stage.ComputeWindow(project, evaluator, specs[1], stage.ModeDecision{Mode: stage.ModeExact})
	if err != nil {
		t.Fatal(err)
	}
	window1.State = stage.WindowCommitted
	requireCode(t, store.SaveWindow(project.ID, window1, len(specs)), stage.CodeState)

	result, err := runner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunRealtime, CheckpointPath: dbPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Progress.Committed != result.Progress.Total {
		t.Fatalf("unexpected progress after failed out-of-order insert: %+v", result.Progress)
	}
	load, err := store.Load(project.ID, "wrong-hash", evaluator.ParamVersion(), stage.AlgorithmVersion, len(specs))
	if err != nil {
		t.Fatal(err)
	}
	if !load.Invalidated || len(load.Windows) != 0 {
		t.Fatalf("expected entire checkpoint invalidation, got %+v", load)
	}
	if _, err := runner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunRealtime, CheckpointPath: dbPath,
	}); err != nil {
		t.Fatal(err)
	}
	paramLoad, err := store.Load(project.ID, evaluator.ContentHash(), "different-param-version", stage.AlgorithmVersion, len(specs))
	if err != nil {
		t.Fatal(err)
	}
	if !paramLoad.Invalidated || len(paramLoad.Windows) != 0 {
		t.Fatalf("expected parameter-version invalidation, got %+v", paramLoad)
	}
}

func TestWindowFailureRetryAndRecovery(t *testing.T) {
	project := tiny(t, 19)
	dbPath := filepath.Join(t.TempDir(), "fault.db")
	params := stage.DefaultParams().WithDefaults(project)
	params.MaxWorkers = 2
	runner, err := stage.NewRunner(project, params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunRealtime, CheckpointPath: dbPath,
		FaultInjection: true, FaultWindow: 3, FaultMessage: "single window fault",
	})
	requireCode(t, err, stage.CodeState)
	if result.Progress.Committed != 3 {
		t.Fatalf("expected 3 committed windows before fault, got %+v", result.Progress)
	}
	if len(result.Statuses) <= 3 || result.Statuses[3].State != stage.WindowFailed {
		t.Fatalf("expected failed window 3, got %+v", result.Statuses)
	}
	resumeRunner, err := stage.NewRunner(project, params)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := resumeRunner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunResume, CheckpointPath: dbPath,
	})
	if err != nil {
		t.Fatalf("resume after fault: %v", err)
	}
	if resumed.Progress.Committed != resumed.Progress.Total {
		t.Fatalf("resume did not finish: %+v", resumed.Progress)
	}
	fullRunner, err := stage.NewRunner(project, params)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullRunner.Run(context.Background(), stage.RunOptions{
		Mode: stage.RunFull, CheckpointPath: filepath.Join(t.TempDir(), "full.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.ComparePathSnapshots(resumed.Snapshot(), full.Snapshot()); err != nil {
		t.Fatalf("recovered output differs from full recompute: %v", err)
	}
}

func TestDivergenceLocator(t *testing.T) {
	project := tiny(t, 20)
	evaluator, err := stage.NewEvaluator(project, stage.DefaultParams().WithDefaults(project))
	if err != nil {
		t.Fatal(err)
	}
	frame := evaluator.Frame(10)
	changed := frame
	changed.Lights = append([]stage.LightOutput(nil), frame.Lights...)
	changed.Lights[2].RGB.R += 7
	err = stage.CompareFrameSequences([]stage.FrameOutput{frame}, []stage.FrameOutput{changed})
	domain := requireCode(t, err, stage.CodeDiverge)
	if len(domain.Details) == 0 {
		t.Fatal("missing divergence details")
	}
	detail := domain.Details[0]
	if detail.Frame != 10 || detail.LightID != frame.Lights[2].LightID || detail.Field != "rgb.r" {
		t.Fatalf("bad divergence locator: %+v", detail)
	}
}

func TestScaleErrorIncludesTimeAndLight(t *testing.T) {
	project := tiny(t, 21)
	params := stage.DefaultParams().WithDefaults(project)
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		t.Fatal(err)
	}
	project.Truth.Samples[0].Lights[0].RGB.R += 80
	err = stage.VerifyTruth(project, evaluator)
	domain := requireCode(t, err, stage.CodeScale)
	if len(domain.Details) == 0 || domain.Details[0].TimeMS != project.Truth.Samples[0].TimeMS ||
		domain.Details[0].LightID == "" || domain.Details[0].Field == "" {
		t.Fatalf("scale error lacks time/light/field locator: %+v", domain)
	}
}

func TestModeSelectionUsesScaleAndPreviewBudget(t *testing.T) {
	project := tiny(t, 22)
	params := stage.DefaultParams()
	params.MaxExactCues = 10
	decision := stage.SelectMode(project, params)
	if decision.Mode != stage.ModeMerged {
		t.Fatalf("expected merged mode, got %+v", decision)
	}
	params = stage.DefaultParams()
	decision = stage.SelectMode(project, params)
	if decision.Mode != stage.ModeExact {
		t.Fatalf("expected exact mode for tiny project, got %+v", decision)
	}
}

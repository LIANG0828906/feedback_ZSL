package acceptance_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"terrain-occlusion/internal/checkpoint"
	"terrain-occlusion/internal/cull"
	"terrain-occlusion/internal/diagnostic"
	"terrain-occlusion/internal/estimate"
	"terrain-occlusion/internal/geo"
	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/runner"
	"terrain-occlusion/internal/samplegen"
	"terrain-occlusion/internal/statemachine"
)

func runReport(t *testing.T) *model.DiagnosticReport {
	t.Helper()
	p, err := samplegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	opt := diagnostic.Options{
		RealtimeDB: filepath.Join(dir, "realtime.db"),
		ResumeDB:   filepath.Join(dir, "resume.db"),
		ReplayDB:   filepath.Join(dir, "replay.db"),
		BreakAt:    3,
	}
	report := diagnostic.RunThreePaths(p, opt)
	if !report.OK {
		t.Fatalf("diagnostic report not ok: %v", report.VerificationErrors)
	}
	return report
}

func TestThreePathsVerdictsStreamingAndCheckpoint(t *testing.T) {
	report := runReport(t)
	if !report.Comparison.Identical {
		t.Fatalf("paths differ: %+v", report.Comparison)
	}
	if report.TotalSamples != len(report.Realtime.Results) || report.CommittedSamples != report.TotalSamples {
		t.Fatalf("bad progress committed=%d total=%d results=%d", report.CommittedSamples, report.TotalSamples, len(report.Realtime.Results))
	}

	counts := map[model.Verdict]int{}
	revisit, loads, unloads, queued := 0, 0, 0, 0
	for _, s := range report.Realtime.Results {
		for _, v := range s.Verdicts {
			counts[v.Final]++
			if v.LowerBound > v.DecisionMargin+1e-9 || v.UpperBound < v.DecisionMargin-1e-9 {
				t.Fatalf("sample %d chunk %d margin outside bounds", s.SampleIndex, v.ChunkID)
			}
		}
		if s.RevisitHit {
			revisit++
		}
		loads += len(s.LoadEvents)
		unloads += len(s.UnloadEvents)
		queued += len(s.QueuedIDs)
		if err := estimate.CheckSufficiency(s.ErrorReport, 0); err != nil {
			t.Fatal(err)
		}
	}
	if counts[model.VerdictVisible] == 0 || counts[model.VerdictOccluded] == 0 || counts[model.VerdictUncertain] == 0 {
		t.Fatalf("need all three verdict classes, got %+v", counts)
	}
	if revisit == 0 {
		t.Fatal("expected deterministic camera fallback cache hits")
	}
	if loads == 0 || unloads == 0 || queued == 0 {
		t.Fatalf("expected load/unload/queue evidence: loads=%d unloads=%d queued=%d", loads, unloads, queued)
	}

	store, err := checkpoint.Open(report.Realtime.CheckpointPath)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.LoadVerified(checkpoint.ProjectHash(&report.Project))
	_ = store.Close()
	if err != nil {
		t.Fatalf("verified checkpoint path: %v", err)
	}
	if len(records) != len(report.Realtime.Results) {
		t.Fatalf("checkpoint records=%d results=%d", len(records), len(report.Realtime.Results))
	}
}

func TestProjectionTruthMatchesCameraMath(t *testing.T) {
	p, err := samplegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range p.Truth.Projection {
		cam := geo.NewCamera(p.CameraTrack[pt.SampleIndex], p.Parameters.ViewportWidth, p.Parameters.ViewportHeight)
		var world model.Vec3
		for _, c := range p.Chunks {
			if c.ID != pt.ChunkID {
				continue
			}
			world = c.SamplePoints[pt.PointIndex]
		}
		x, y, z, _, _, ok := cam.Project(world)
		if !ok {
			t.Fatalf("truth projection no longer projects: %+v", pt)
		}
		if abs(x-pt.ScreenX) > 1e-6 || abs(y-pt.ScreenY) > 1e-6 || abs(z-pt.ViewDepth) > 1e-6 {
			t.Fatalf("projection mismatch truth=(%v,%v,%v) got=(%v,%v,%v)", pt.ScreenX, pt.ScreenY, pt.ViewDepth, x, y, z)
		}
	}
}

func TestEVisListsChunkAndBothValues(t *testing.T) {
	p, _ := samplegen.Generate()
	shadow := geo.RenderShadowMap(*p)
	got := cull.EvaluateChunk(p, p.CameraTrack[0], shadow, p.Chunks[0])
	wrong := model.VerdictVisible
	if got.Final == model.VerdictVisible {
		wrong = model.VerdictOccluded
	}
	p.Truth.Samples[0].Chunks[0].Verdict = wrong

	r := runner.New(p, nil, true)
	_, err := r.Advance()
	var te *model.Error
	if !errors.As(err, &te) || te.Code != model.EVis {
		t.Fatalf("want E_VIS, got %v", err)
	}
	if te.Details["chunk_id"] != 1 || te.Details["truth"] != string(wrong) || te.Details["observed"] != string(got.Final) {
		t.Fatalf("E_VIS details must list chunk and both values: %+v", te.Details)
	}
}

func TestELoadReportsChunkAndDelay(t *testing.T) {
	p, _ := samplegen.Generate()
	p.Budgets.LoadSlots = 1
	p.Budgets.MaxResidencyDelay = 0
	r := runner.New(p, nil, false)
	if _, err := r.Advance(); err != nil {
		t.Fatalf("first sample should queue without deadline violation: %v", err)
	}
	_, err := r.Advance()
	var te *model.Error
	if !errors.As(err, &te) || te.Code != model.ELoad {
		t.Fatalf("want E_LOAD, got %v", err)
	}
	if te.Details["chunk_id"] == nil || te.Details["delayed_samples"] == nil {
		t.Fatalf("E_LOAD must include chunk and delayed samples: %+v", te.Details)
	}
}

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	m := statemachine.New(2)
	err := m.Transition(0, statemachine.Committed)
	var te *model.Error
	if !errors.As(err, &te) || te.Code != model.EState {
		t.Fatalf("want E_STATE, got %v", err)
	}
	if m.State(0) != statemachine.Pending {
		t.Fatalf("illegal transition changed state to %s", m.State(0))
	}
	if committed, total := m.Progress(); committed != 0 || total != 2 {
		t.Fatalf("progress=(%d,%d)", committed, total)
	}
}

func TestEEstWhenObservedDifferenceExceedsBound(t *testing.T) {
	p, _ := samplegen.Generate()
	report := estimate.Report(p.Parameters, 8, 0)
	if err := estimate.CheckSufficiency(report, report.EstimatedSetDifference+1); err == nil {
		t.Fatal("expected E_EST")
	} else {
		var te *model.Error
		if !errors.As(err, &te) || te.Code != model.EEst {
			t.Fatalf("want E_EST, got %v", err)
		}
		if te.Details["sample_index"] != 8 {
			t.Fatalf("E_EST must identify sample: %+v", te.Details)
		}
	}
}

func TestCommittedSamplesFreezeParameterVersion(t *testing.T) {
	p, _ := samplegen.Generate()
	r := runner.New(p, nil, false)
	if _, err := r.Advance(); err != nil {
		t.Fatal(err)
	}
	oldResolution := r.Params.ShadowResolution
	oldVersion := runner.ParameterVersion(r.Params)
	r.Params.ShadowResolution = 256
	if _, err := r.Advance(); err != nil {
		t.Fatal(err)
	}
	if r.Results[0].ShadowResolution != oldResolution || r.Results[0].ShadowVersion != oldVersion {
		t.Fatal("committed sample was mutated by a later parameter change")
	}
	if r.Results[1].ShadowResolution != 256 || r.Results[1].ShadowVersion == oldVersion {
		t.Fatal("new sample did not use changed parameter version")
	}
}

func TestShadowResolutionRepeatableAtEachDeclaredValue(t *testing.T) {
	p, _ := samplegen.Generate()
	evaluate := func(resolution int) []model.ChunkVerdict {
		p.Parameters.ShadowResolution = resolution
		shadow := geo.RenderShadowMap(*p)
		out := make([]model.ChunkVerdict, 0, len(p.Chunks))
		for _, chunk := range p.Chunks {
			out = append(out, cull.EvaluateChunk(p, p.CameraTrack[0], shadow, chunk))
		}
		return out
	}
	for _, resolution := range []int{64, 128, 256} {
		first := evaluate(resolution)
		second := evaluate(resolution)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("resolution %d produced non-reproducible verdicts", resolution)
		}
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

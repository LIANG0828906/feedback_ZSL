package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustProject(t *testing.T) *Project {
	t.Helper()
	p, err := GenerateProject(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExactSamplingDeterministicAndTruth(t *testing.T) {
	p := mustProject(t)
	one, err := RunProject(context.Background(), p, RunOptions{Workers: 1, Sampling: ExactSampling()})
	if err != nil {
		t.Fatal(err)
	}
	many, err := RunProject(context.Background(), p, RunOptions{Workers: 4, Sampling: ExactSampling()})
	if err != nil {
		t.Fatal(err)
	}
	if ok, mismatch := CompareRunResults(one, many); !ok {
		t.Fatalf("worker count changed result: %+v", mismatch)
	}
	if len(one.Windows) != 5 {
		t.Fatalf("windows = %d", len(one.Windows))
	}
	uncertain := 0
	points := 0
	for _, w := range one.Windows {
		uncertain += w.UncertainCount
		for _, f := range w.Fixtures {
			points += len(f.Points)
			for _, pt := range f.Points {
				if pt.Uncertain && !(pt.LowerBound < pt.Intensity && pt.Intensity < pt.UpperBound) {
					t.Fatalf("bad uncertain bounds: %+v", pt)
				}
			}
		}
	}
	if points == 0 || uncertain == 0 {
		t.Fatalf("points=%d uncertain=%d", points, uncertain)
	}
	overlap := false
	for _, w := range one.Windows {
		for _, a := range w.Aggregates {
			if len(a.Contributors) >= 2 {
				overlap = true
				break
			}
		}
	}
	if !overlap {
		t.Fatal("expected at least one accumulated overlapping cell")
	}
	bundle, err := GenerateSampleBundle(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	var truth TruthManifest
	if err := jsonUnmarshal(bundle.Files["truth.json"], &truth); err != nil {
		t.Fatal(err)
	}
	if err := CompareTruth(one, truth); err != nil {
		t.Fatalf("truth comparison: %v", err)
	}
}

func TestPoseAndSamplingErrors(t *testing.T) {
	p := mustProject(t)
	zero := Vec3{}
	p.Fixtures[0].BaseAim = &zero
	_, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling()})
	if !errors.Is(err, ErrParam) {
		t.Fatalf("zero aim: expected E_PARA, got %v", err)
	}
	if AsError(err).FixtureID != "F1" {
		t.Fatalf("missing fixture id: %v", err)
	}

	p = mustProject(t)
	p.Fixtures[1].OuterHalfMicroDeg = p.Fixtures[1].InnerHalfMicroDeg - 1
	_, err = RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling()})
	if !errors.Is(err, ErrParam) || AsError(err).FixtureID != "F2" {
		t.Fatalf("inverted cone: %v", err)
	}

	p = mustProject(t)
	cfg := ExactSampling()
	cfg.Density = 0
	_, err = RunProject(context.Background(), p, RunOptions{Sampling: cfg})
	if !errors.Is(err, ErrValue) || AsError(err).Field != "sampling.density" {
		t.Fatalf("density validation: %v", err)
	}
	cfg = ExactSampling()
	cfg.DistancesMM = []int64{0}
	_, err = RunProject(context.Background(), p, RunOptions{Sampling: cfg})
	if !errors.Is(err, ErrValue) {
		t.Fatalf("zero distance validation: %v", err)
	}
	if AsError(err).FixtureID == "" {
		t.Fatal("sampling validation did not carry fixture context")
	}
}

func TestStateMachinesKeepStateAndProgressOnIllegalMove(t *testing.T) {
	w := NewWindowStateMachine()
	if err := w.Transition(WindowCommitted); !errors.Is(err, ErrState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if w.State() != WindowPending || w.Progress() != 0 {
		t.Fatalf("illegal move changed state: %s/%d", w.State(), w.Progress())
	}
	if err := w.Transition(WindowPoseEvaluating); err != nil {
		t.Fatal(err)
	}
	if err := w.Transition(WindowConeSampling); err != nil {
		t.Fatal(err)
	}
	if err := w.Transition(WindowCommitted); err != nil {
		t.Fatal(err)
	}
	if w.Progress() != 3 {
		t.Fatalf("progress = %d", w.Progress())
	}
	f := NewFixtureStateMachine("F1")
	if err := f.Transition(FixtureSampled); !errors.Is(err, ErrState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if f.State() != FixturePending || f.Progress() != 0 {
		t.Fatal("fixture illegal move changed state")
	}
	if err := f.Transition(FixtureEvaluated); err != nil {
		t.Fatal(err)
	}
	if err := f.Transition(FixtureSampled); err != nil {
		t.Fatal(err)
	}
	if err := f.Transition(FixtureCommitted); err != nil {
		t.Fatal(err)
	}
	if f.Progress() != 3 {
		t.Fatalf("fixture progress = %d", f.Progress())
	}
}

func TestCheckpointResumeAndTamperRejection(t *testing.T) {
	p := mustProject(t)
	dir := t.TempDir()
	store := NewCheckpointStore(dir)
	partial, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling(), Store: store, Limit: 2, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(partial.CommitSequence, []int{1, 2}) {
		t.Fatalf("partial sequence: %v", partial.CommitSequence)
	}
	resumed, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling(), Store: store, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.Reused, []int{1, 2}) || !reflect.DeepEqual(resumed.Committed, []int{3, 4, 5}) {
		t.Fatalf("resume split reused=%v committed=%v", resumed.Reused, resumed.Committed)
	}
	full, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling(), Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if ok, mismatch := CompareRunResults(full, resumed); !ok {
		t.Fatalf("resume mismatch: %+v", mismatch)
	}
	files, err := filepath.Glob(filepath.Join(dir, "window-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(files[0], data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAll(p.ParameterVersion, AlgorithmVersion); !errors.Is(err, ErrVersion) {
		t.Fatalf("tamper: expected E_VER, got %v", err)
	}

	otherDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherDir, "notes.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCheckpointStore(otherDir).LoadAll(p.ParameterVersion, AlgorithmVersion); !errors.Is(err, ErrVersion) {
		t.Fatalf("unexpected JSON: expected E_VER, got %v", err)
	}
}

func TestInvalidationIsScopedAndCuesOnlyAffectIntersectingWindows(t *testing.T) {
	p := mustProject(t)
	next, err := cloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range next.Cues {
		if next.Cues[i].ID == "cue-color-f3" {
			next.Cues[i].FromRGB = [3]int64{0, 10_000, 0}
		}
	}
	report, err := CompareProjects(p, next)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, 0, len(report.Invalidated))
	for _, inv := range report.Invalidated {
		got = append(got, inv.WindowNumber)
	}
	want := []int{3, 4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("color cue invalidated %v, want %v", got, want)
	}
	if !reflect.DeepEqual(report.Reused, []int{1, 2}) {
		t.Fatalf("reused = %v", report.Reused)
	}

	dir := t.TempDir()
	store := NewCheckpointStore(dir)
	if _, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling(), Store: store}); err != nil {
		t.Fatal(err)
	}
	run, err := RunProject(context.Background(), next, RunOptions{Sampling: ExactSampling(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(run.Reused, []int{1, 2}) || !reflect.DeepEqual(run.Committed, []int{3, 4, 5}) {
		t.Fatalf("checkpoint reuse after edit: reused=%v committed=%v", run.Reused, run.Committed)
	}
	withPrevious, err := RunProject(context.Background(), next, RunOptions{
		Sampling: ExactSampling(), Store: store, Previous: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withPrevious.Invalidated) != 3 || withPrevious.Invalidated[0].WindowNumber != 3 {
		t.Fatalf("previous-project invalidation list: %+v", withPrevious.Invalidated)
	}
	if len(withPrevious.Invalidated[0].Reasons) == 0 || len(withPrevious.Invalidated[0].FixtureIDs) == 0 {
		t.Fatalf("invalidation lacks reasons/fixture: %+v", withPrevious.Invalidated[0])
	}
}

func TestPreviewAndThreePaths(t *testing.T) {
	p := mustProject(t)
	preview, err := RunPreview(context.Background(), p, 2, 5000)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling(), Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	cmp, err := ComparePreviewWithExact(preview.Result, exact, Tolerance{})
	if err != nil {
		t.Fatalf("preview comparison: %v (%+v)", err, cmp)
	}
	if !cmp.OK || cmp.Compared == 0 {
		t.Fatalf("bad preview comparison: %+v", cmp)
	}
	data, err := json.Marshal(exact)
	if err != nil {
		t.Fatal(err)
	}
	var tampered RunResult
	if err := json.Unmarshal(data, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Windows[0].Fixtures[0].Points[0].Intensity++
	if _, err := ComparePreviewWithExact(preview.Result, tampered, Tolerance{}); !errors.Is(err, ErrDiverge) {
		t.Fatalf("tampered exact path: expected E_DIVERGE, got %v", err)
	} else if AsError(err).FixtureID == "" || len(AsError(err).Coordinate) != 3 {
		t.Fatalf("diverge error lacks location: %+v", AsError(err))
	}
	report, err := VerifyThreePaths(context.Background(), p, t.TempDir(), PathVerifyOptions{
		Sampling: ExactSampling(), LiveWorkers: 1, FullWorkers: 4, ResumeStopAfter: 2,
	})
	if err != nil {
		t.Fatalf("three paths: %v", err)
	}
	if !report.OK || len(report.Paths) != 3 {
		t.Fatalf("bad verify report: %+v", report)
	}
	for _, path := range report.Paths {
		if !reflect.DeepEqual(path.CommitSequence, []int{1, 2, 3, 4, 5}) {
			t.Fatalf("%s commit sequence %v", path.Name, path.CommitSequence)
		}
	}
}

func TestDependencyGraphScopesCueToIntersectingWindows(t *testing.T) {
	p := mustProject(t)
	graph, err := BuildDependencyGraph(p, ExactSampling())
	if err != nil {
		t.Fatal(err)
	}
	downstream := graph.Downstream("cue:cue-color-f3")
	joined := strings.Join(downstream, "\n")
	for _, window := range []int{3, 4, 5} {
		if !strings.Contains(joined, fmt.Sprintf("window:%d:summary", window)) {
			t.Fatalf("downstream missing window %d: %v", window, downstream)
		}
	}
	if strings.Contains(joined, "window:1:summary") || strings.Contains(joined, "window:2:summary") {
		t.Fatalf("color cue leaked to windows 1/2: %v", downstream)
	}
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

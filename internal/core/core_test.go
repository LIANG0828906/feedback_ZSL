package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestGeneratorTruthAndValidation(t *testing.T) {
	dir := t.TempDir() + "/samples"
	if _, err := GenerateSampleSet(GeneratorOptions{OutDir: dir, Seed: DefaultSeed}); err != nil {
		t.Fatal(err)
	}
	set, e := LoadSampleSet(dir)
	if e != nil {
		t.Fatalf("LoadSampleSet: %v", e)
	}
	if len(set.Clips) != 3 || len(set.Targets) != 2 {
		t.Fatalf("expected 3 clips and 2 targets, got %d/%d", len(set.Clips), len(set.Targets))
	}
	for id, clip := range set.Clips {
		truth := set.Truth[id]
		report := VerifyTruth(clip, truth)
		if !report.OK {
			t.Fatalf("truth mismatch for %s: %v", id, report.Mismatches)
		}
	}
}

func TestSchemaMissingField(t *testing.T) {
	clip := generateClip(sourceSkeleton(), "schema_case", 7, 12, 12, 0.02, 0.3, 0)
	data, _ := json.Marshal(clip)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "id")
	missing, _ := json.Marshal(raw)
	_, err := DecodeAndValidateClip(missing)
	if err == nil || !IsCode(err, ECodeSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	var ce *Error
	if !AsError(err, &ce) || ce.Field != "id" {
		t.Fatalf("expected missing id field, got %+v", ce)
	}

	var nested map[string]any
	if err := json.Unmarshal(data, &nested); err != nil {
		t.Fatal(err)
	}
	skeleton := nested["skeleton"].(map[string]any)
	joints := skeleton["joints"].([]any)
	delete(joints[0].(map[string]any), "parent")
	nestedData, _ := json.Marshal(nested)
	_, err = DecodeAndValidateClip(nestedData)
	if err == nil || !IsCode(err, ECodeSchema) {
		t.Fatalf("expected E_SCHEMA for missing nested parent, got %v", err)
	}
	if err.Field != "parent" {
		t.Fatalf("expected parent field error, got %+v", err)
	}
}

func TestCycleErrorCode(t *testing.T) {
	clip := generateClip(sourceSkeleton(), "cycle_case", 7, 12, 12, 0.02, 0.3, 0)
	for i := range clip.Skeleton.Joints {
		if clip.Skeleton.Joints[i].Name == "hips" {
			clip.Skeleton.Joints[i].Parent = "spine"
		}
	}
	err := ValidateClip(clip)
	if err == nil || !IsCode(err, ECodeCycle) {
		t.Fatalf("expected E_CYCLE, got %v", err)
	}
}

func TestQuaternionCountErrorNamesFrameAndJoint(t *testing.T) {
	clip := generateClip(sourceSkeleton(), "quat_case", 7, 12, 12, 0.02, 0.3, 0)
	frame := 7
	wantJoint := clip.Skeleton.Joints[len(clip.Skeleton.Joints)-1].Name
	clip.Frames[frame].Rotations = clip.Frames[frame].Rotations[:len(clip.Skeleton.Joints)-1]
	err := ValidateClip(clip)
	if err == nil || !IsCode(err, ECodeQuat) {
		t.Fatalf("expected E_QUAT, got %v", err)
	}
	var qe *Error
	AsError(err, &qe)
	if qe.Frame != frame || qe.Joint != wantJoint {
		t.Fatalf("expected frame=%d joint=%s, got frame=%d joint=%s", frame, wantJoint, qe.Frame, qe.Joint)
	}
}

func TestMappingTraceFanoutAndEMap(t *testing.T) {
	source := sourceSkeleton()
	hero := heroTallSkeleton()
	m, e := BuildMapping(source, hero, DefaultParameters())
	if e != nil {
		t.Fatal(e)
	}
	entry, ok := m.QuerySource("spine")
	if !ok || entry.Strategy != "fanout" || len(entry.Targets) != 2 {
		t.Fatalf("expected spine fanout to two targets, got %+v ok=%v", entry, ok)
	}
	if _, ok := m.QueryTarget("spine2"); !ok {
		t.Fatal("spine2 reverse mapping is not queryable")
	}
	foundToe := false
	for _, name := range m.UnmappedSource {
		if name == "toe_l" {
			foundToe = true
		}
	}
	if !foundToe {
		t.Fatalf("toe_l must be traceable as unmapped: %v", m.UnmappedSource)
	}

	badTarget := Skeleton{ID: "bad", Joints: []Joint{{Name: "unrelated_root", Offset: Vec3{0, 1, 0}}}}
	_, err := BuildMapping(source, badTarget, DefaultParameters())
	if err == nil || !IsCode(err, ECodeMap) {
		t.Fatalf("expected E_MAP, got %v", err)
	}
	var me *Error
	if AsError(err, &me) {
		names, _ := me.Details.([]string)
		if len(names) == 0 || names[0] != "hips" {
			t.Fatalf("expected impossible hips in details, got %#v", me.Details)
		}
	}
}

func TestStateMachineIllegalTransitionPreservesState(t *testing.T) {
	sm := NewStateMachine(4)
	if err := sm.Transition(2, StateCommitted); err == nil || !IsCode(err, ECodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if got := sm.State(2); got != StatePending {
		t.Fatalf("illegal transition changed state to %s", got)
	}
	if err := sm.Transition(2, StateSolving); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(2, StateCommitted); err != nil {
		t.Fatal(err)
	}
}

func TestSolverReturnsESolveWithJointAndResidual(t *testing.T) {
	zeroLimit := &AngleLimit{MinRadians: 0, MaxRadians: 0, Axis: Vec3{1, 0, 0}}
	sk := Skeleton{ID: "impossible_reach", Joints: []Joint{
		{Name: "root", Offset: Vec3{0, 1, 0}},
		{Name: "a", Parent: "root", Offset: Vec3{0, -1, 0}, Limit: zeroLimit},
		{Name: "b", Parent: "a", Offset: Vec3{0, -1, 0}, Limit: zeroLimit, Reach: &ReachLimit{Anchor: "root", MaxDistance: 0.1}},
	}}
	initial := []Quat{{1, 0, 0, 0}, AxisAngle(Vec3{1, 0, 0}, 0.8), AxisAngle(Vec3{1, 0, 0}, 0.8)}
	params := DefaultParameters()
	params.MaxIterations = 3
	params.AngleTolerance = 1e-5
	params.ReachTolerance = 1e-5
	_, err := SolveFrame(9, sk, initial, params)
	if err == nil || !IsCode(err, ECodeSolve) {
		t.Fatalf("expected E_SOLVE, got %v", err)
	}
	var se *Error
	AsError(err, &se)
	if se.Frame != 9 || se.Joint == "" {
		t.Fatalf("E_SOLVE must include frame and joint, got %+v", se)
	}
}

func TestCompareLocatesJointField(t *testing.T) {
	sk := &Skeleton{Joints: []Joint{{Name: "j0"}, {Name: "j1"}}}
	a := []FrameResult{{Frame: 0, Rotations: []Quat{{1, 0, 0, 0}, {1, 0, 0, 0}}, Root: Vec3{1, 2, 3}}}
	b := []FrameResult{{Frame: 0, Rotations: []Quat{{1, 0, 0, 0}, {1, 0.1, 0, 0}}, Root: Vec3{1, 2, 3}}}
	rep := CompareResults("a", "b", sk, a, b)
	if rep.OK || rep.FirstMismatch == nil {
		t.Fatal("expected mismatch")
	}
	if rep.FirstMismatch.Joint != "j1" || rep.FirstMismatch.Field != "rotation.x" {
		t.Fatalf("unexpected first mismatch: %+v", rep.FirstMismatch)
	}
	if err := DiffAsError(rep); !IsCode(err, ECodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
}

func TestBoundedPoolBlocksWhenBuffersFullWithoutDrop(t *testing.T) {
	pool := NewBoundedPool[int](1, 1)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	r1 := pool.Submit(0, func() (int, *Error) {
		started <- struct{}{}
		<-release
		return 10, nil
	})
	<-started
	r2 := pool.Submit(1, func() (int, *Error) { return 20, nil })
	blockedReturned := make(chan struct{})
	go func() {
		r3 := pool.Submit(2, func() (int, *Error) { return 30, nil })
		<-r3
		close(blockedReturned)
	}()
	select {
	case <-blockedReturned:
		t.Fatal("third submit should block while worker and buffer are full")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if got := (<-r1).Value; got != 10 {
		t.Fatalf("r1=%d", got)
	}
	if got := (<-r2).Value; got != 20 {
		t.Fatalf("r2=%d", got)
	}
	select {
	case <-blockedReturned:
	case <-time.After(time.Second):
		t.Fatal("blocked submit did not drain after worker release")
	}
	pool.Close()
	pool.Wait()
}

func TestAsErrorHandlesWrappedErrors(t *testing.T) {
	base := NewError(ECodeMap, "x")
	wrapped := fmt.Errorf("wrap: %w", base)
	if !IsCode(wrapped, ECodeMap) {
		t.Fatalf("expected wrapped E_MAP, got %v", wrapped)
	}
	var me *Error
	if !AsError(wrapped, &me) || me.Code != ECodeMap {
		t.Fatalf("AsError failed: %v", wrapped)
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("errors.Is should identify core.Error pointer")
	}
}

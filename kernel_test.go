package hckernel

import (
	"context"
	"path/filepath"
	"testing"
)

func sampleProjectEvents(t *testing.T) (*Project, []Event) {
	t.Helper()
	g := NewGenerator(DefaultSeed)
	return g.BuildProject(), g.BuildEvents()
}

func TestSchemaVersionsLoadEquivalent(t *testing.T) {
	g := NewGenerator(DefaultSeed)
	p := g.BuildProject()
	docs := map[string][]byte{}
	var err error
	docs[SchemaV1], err = MarshalV1(p)
	if err != nil {
		t.Fatal(err)
	}
	docs[SchemaV2], err = MarshalV2(p)
	if err != nil {
		t.Fatal(err)
	}
	docs[SchemaV3], err = MarshalV3(p)
	if err != nil {
		t.Fatal(err)
	}

	events := g.BuildEvents()
	var fp string
	var ids []string
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		loaded, lerr := LoadDocument(docs[version])
		if lerr != nil {
			t.Fatalf("%s load: %v", version, lerr)
		}
		thisFP, ferr := loaded.Project.Fingerprint()
		if ferr != nil {
			t.Fatal(ferr)
		}
		if fp == "" {
			fp = thisFP
			ids = SortedInstanceIDs(loaded.Project.Instances)
		} else if thisFP != fp {
			t.Fatalf("%s fingerprint mismatch:\n%s\n%s", version, fp, thisFP)
		}
		if got := SortedInstanceIDs(loaded.Project.Instances); len(got) != len(ids) {
			t.Fatalf("%s ids=%v want %v", version, got, ids)
		}
		res, rerr := NewEngine(loaded.Project, events)
		if rerr != nil {
			t.Fatal(rerr)
		}
		gotResult, rerr := res.Run()
		if rerr != nil {
			t.Fatalf("%s run: %v", version, rerr)
		}
		baseEngine, _ := NewEngine(p, events)
		baseResult, _ := baseEngine.Run()
		if !ResultsEqualSemantic(gotResult, baseResult) {
			t.Fatalf("%s produced different rounds", version)
		}
	}
}

func TestUnknownSchemaCode(t *testing.T) {
	_, err := LoadDocument([]byte(`{"schema":"hcs/v999"}`))
	if !IsCode(err, ErrUnknownSchemaVersion) {
		t.Fatalf("code=%v err=%v", CodeOf(err), err)
	}
}

func TestGeneratedSchedulingBranches(t *testing.T) {
	p, events := sampleProjectEvents(t)
	e, err := NewEngine(p, events)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rounds) != 14 {
		t.Fatalf("rounds=%d", len(res.Rounds))
	}

	var droppedBe06 bool
	var sawQueue bool
	for _, round := range res.Rounds {
		for _, d := range round.Dropped {
			if d.InstanceID == "be-06" && d.Code == ErrDeferTimeout && round.Round == 6 {
				droppedBe06 = true
			}
		}
		if len(round.Queue) > 0 {
			sawQueue = true
		}
	}
	if !droppedBe06 {
		t.Fatalf("expected deterministic be-06 max-defer drop in round 6")
	}
	if !sawQueue {
		t.Fatalf("expected deferred queue members")
	}

	wantTransitions := map[string]bool{
		"be-03:initial:healthy":     false,
		"be-01:healthy:unhealthy":   false,
		"be-01:unhealthy:half_open": false,
		"be-01:half_open:healthy":   false,
		"be-05:unhealthy:half_open": false,
		"be-05:half_open:unhealthy": false,
		"be-02:healthy:unhealthy":   false,
	}
	for _, obs := range res.HealthTrace {
		key := obs.InstanceID + ":" + string(obs.FromState) + ":" + string(obs.ToState)
		if _, ok := wantTransitions[key]; ok && obs.StateChanged {
			wantTransitions[key] = true
		}
	}
	for key, found := range wantTransitions {
		if !found {
			t.Fatalf("missing transition %s", key)
		}
	}
}

func TestIncrementalEditsMatchFull(t *testing.T) {
	g := NewGenerator(DefaultSeed)
	cache, err := BuildCache(g.BuildProject(), g.BuildEvents())
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range g.BuildEdits() {
		inc, err := cache.ApplyEdit(edit)
		if err != nil {
			t.Fatalf("%s: %v", edit.ID, err)
		}
		if !inc.MatchesFull {
			t.Fatalf("%s incremental result does not equal full replay", edit.ID)
		}
		if inc.CacheHits == 0 {
			t.Fatalf("%s expected cached prefix hits", edit.ID)
		}
		if len(inc.RecomputedRounds) == 0 || inc.Closure.FromRound < 1 {
			t.Fatalf("%s missing closure", edit.ID)
		}
		for _, r := range inc.Result.Rounds {
			if r.Provenance == nil || r.Provenance.InvalidationSource == "" {
				t.Fatalf("%s round %d missing provenance", edit.ID, r.Round)
			}
		}
	}
}

func TestWeightScaleInvariant(t *testing.T) {
	permille := map[string]int64{"a": 500, "b": 300, "c": 200}
	basisPoints := map[string]int64{"a": 5000, "b": 3000, "c": 2000}
	ppm := map[string]int64{"a": 500000, "b": 300000, "c": 200000}
	a1, err := AllocateWeightedSlots(permille, 5)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := AllocateWeightedSlots(basisPoints, 5)
	a3, _ := AllocateWeightedSlots(ppm, 5)
	if !mapsEqual(a1, a2) || !mapsEqual(a1, a3) {
		t.Fatalf("weight scales differ: %v %v %v", a1, a2, a3)
	}
	if _, err := AllocateWeightedSlots(map[string]int64{"a": 1}, -1); !IsCode(err, ErrBudgetZero) {
		t.Fatalf("negative slots code=%v", CodeOf(err))
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestParallelBackpressure(t *testing.T) {
	ctx := context.Background()
	executor, err := NewParallelExecutor(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	probes := []Probe{{ID: "p1", InstanceID: "i1"}, {ID: "p2", InstanceID: "i2"}, {ID: "p3", InstanceID: "i3"}}
	_, err = executor.ExecuteBatch(ctx, 1, probes, 4, nil)
	if !IsCode(err, ErrBackpressureBufferFull) {
		t.Fatalf("buffer code=%v err=%v", CodeOf(err), err)
	}
	_, err = executor.ExecuteBatch(ctx, 1, probes[:2], 9, nil)
	if !IsCode(err, ErrBackpressureWorker) {
		t.Fatalf("worker budget code=%v", CodeOf(err))
	}

	small, _ := NewParallelExecutor(1, 4)
	lease, err := small.TryReserve(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := small.TryReserve(1); !IsCode(err, ErrBackpressureWorkersBusy) {
		t.Fatalf("busy code=%v", CodeOf(err))
	}
	lease[0].Release()

	out, err := executor.ExecuteBatch(ctx, 7, probes[:2], 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Outputs) != 2 || out.Duration < 0 {
		t.Fatalf("bad batch result %+v", out)
	}
}

func TestCheckpointResumeAndExceptionCodes(t *testing.T) {
	p, events := sampleProjectEvents(t)
	full, err := NewEngine(p, events)
	if err != nil {
		t.Fatal(err)
	}
	fullResult, err := full.Run()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.log")
	crashEngine, err := NewEngine(p, events)
	if err != nil {
		t.Fatal(err)
	}
	var crashed bool
	for round := 1; round <= 4; round++ {
		now := int64(round-1) * p.StepMs
		if err := crashEngine.Advance(now, round); err != nil {
			t.Fatal(err)
		}
		cp := crashEngine.Snapshot(round)
		err := CommitCheckpoint(path, cp, CommitOptions{CrashBeforeCursor: round == 3})
		if round == 3 {
			if !IsCode(err, ErrInjectedCrash) {
				t.Fatalf("crash code=%v", CodeOf(err))
			}
			crashed = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !crashed {
		t.Fatal("crash injection did not run")
	}

	cp, err := LoadCheckpoint(path, LoadCheckpointOptions{ExpectedFingerprint: full.fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || cp.CommittedRound != 2 {
		t.Fatalf("committed=%v want round 2", cp)
	}
	resume, err := RestoreEngine(p, events, cp)
	if err != nil {
		t.Fatal(err)
	}
	for round := cp.CommittedRound + 1; round <= p.Rounds; round++ {
		now := int64(round-1) * p.StepMs
		if err := resume.Advance(now, round); err != nil {
			t.Fatal(err)
		}
	}
	resumedResult := resume.Result()
	for i := cp.CommittedRound; i < len(fullResult.Rounds); i++ {
		a := resumedResult.Rounds[i-cp.CommittedRound]
		b := fullResult.Rounds[i]
		if !roundEqual(a, b) {
			t.Fatalf("resumed round %d differs", b.Round)
		}
	}

	truncPath := filepath.Join(dir, "truncated.log")
	if err := WriteTruncatedCheckpoint(truncPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCheckpoint(truncPath, LoadCheckpointOptions{}); !IsCode(err, ErrCheckpointTruncated) {
		t.Fatalf("truncated code=%v err=%v", CodeOf(err), err)
	}

	mismatchPath := filepath.Join(dir, "mismatch.log")
	goodCP := full.Snapshot(p.Rounds)
	if err := CommitCheckpoint(mismatchPath, goodCP, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCheckpoint(mismatchPath, LoadCheckpointOptions{ExpectedFingerprint: "sha256:different"}); !IsCode(err, ErrCheckpointVersionMatch) {
		t.Fatalf("version mismatch code=%v", CodeOf(err))
	}
}

func TestInputExceptionCodes(t *testing.T) {
	cases := []struct {
		name string
		p    Project
		code ErrorCode
	}{
		{"zero interval", Project{ProjectID: "x", StepMs: 100, GlobalConcurrency: 1, Workers: 1, Racks: []Rack{{ID: "r", Concurrency: 1}}, Instances: []Instance{{ID: "i", RackID: "r", IntervalMs: 0, TimeoutMs: 1, SuccessThreshold: 1, FailureThreshold: 1, WeightPPM: 1}}}, ErrIntervalZero},
		{"negative timeout", Project{ProjectID: "x", StepMs: 100, GlobalConcurrency: 1, Workers: 1, Racks: []Rack{{ID: "r", Concurrency: 1}}, Instances: []Instance{{ID: "i", RackID: "r", IntervalMs: 100, TimeoutMs: -1, SuccessThreshold: 1, FailureThreshold: 1, WeightPPM: 1}}}, ErrNegativeTimeout},
		{"threshold", Project{ProjectID: "x", StepMs: 100, GlobalConcurrency: 1, Workers: 1, Racks: []Rack{{ID: "r", Concurrency: 1}}, Instances: []Instance{{ID: "i", RackID: "r", IntervalMs: 100, TimeoutMs: 1, SuccessThreshold: 0, FailureThreshold: 1, WeightPPM: 1}}}, ErrThresholdOutOfRange},
		{"missing rack", Project{ProjectID: "x", StepMs: 100, GlobalConcurrency: 1, Workers: 1, Racks: []Rack{{ID: "r", Concurrency: 1}}, Instances: []Instance{{ID: "i", RackID: "ghost", IntervalMs: 100, TimeoutMs: 1, SuccessThreshold: 1, FailureThreshold: 1, WeightPPM: 1}}}, ErrRackReferenceMissing},
	}
	for _, tc := range cases {
		if err := ValidateProject(&tc.p); !IsCode(err, tc.code) {
			t.Fatalf("%s code=%v err=%v", tc.name, CodeOf(err), err)
		}
	}
}

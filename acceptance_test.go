package cachebench

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testBundle(t *testing.T) *Bundle {
	t.Helper()
	b, err := GenerateBundle(DefaultSeed)
	if err != nil {
		t.Fatalf("GenerateBundle: %v", err)
	}
	return b
}

func TestThreeSchemaVersionsLoadEquivalent(t *testing.T) {
	b := testBundle(t)
	loaded := map[string]*Project{}
	for version, doc := range b.Docs {
		p, err := Load(doc)
		if err != nil {
			t.Fatalf("Load %s: %v", version, err)
		}
		loaded[version] = p
	}

	f1 := loaded[SchemaV1].CanonicalFingerprint()
	f2 := loaded[SchemaV2].CanonicalFingerprint()
	f3 := loaded[SchemaV3].CanonicalFingerprint()
	if !reflect.DeepEqual(f1, f2) || !reflect.DeepEqual(f1, f3) {
		t.Fatalf("schema fingerprints differ:\n%#v\n%#v\n%#v", f1, f2, f3)
	}

	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		sim, err := Simulate(*loaded[version])
		if err != nil {
			t.Fatalf("Simulate loaded %s: %v", version, err)
		}
		if !ResultsEqual(sim, b.Truth) {
			t.Fatalf("simulation from %s differs from generator truth", version)
		}
	}

	v1 := loaded[SchemaV1]
	v2 := loaded[SchemaV2]
	if got := v1.Nodes[0].WeightPermyriad; got != 4000 {
		t.Fatalf("v1 permille conversion = %d, want 4000", got)
	}
	if got := v2.Nodes[0].WeightPermyriad; got != 4000 {
		t.Fatalf("v2 permyriad weight = %d, want 4000", got)
	}
	if !v2.Observations[2].OverloadMarkerPresent || !v2.Observations[2].Overloaded {
		t.Fatalf("v2 optional overload marker was not preserved: %#v", v2.Observations[2])
	}
	if v1.Observations[2].OverloadMarkerPresent {
		t.Fatalf("v1 document has no overload field but loader reported it present")
	}
}

func TestDeterministicGeneratorAndCapacityOverflow(t *testing.T) {
	b1 := testBundle(t)
	b2 := testBundle(t)
	if !ResultsEqual(b1.Truth, b2.Truth) {
		t.Fatal("fixed-seed generator truth is not deterministic")
	}

	var overflow *Overflow
	for ti := range b1.Truth.Ticks {
		for oi := range b1.Truth.Ticks[ti].Overflow {
			ov := b1.Truth.Ticks[ti].Overflow[oi]
			if ov.TickMS == 10 && ov.NodeID == "cache-north-1" {
				overflow = &ov
			}
		}
	}
	if overflow == nil {
		t.Fatal("expected deterministic capacity overflow on cache-north-1 at tick 10")
	}
	if overflow.ConstraintID != "CONN-LIMIT-cache-north-1" {
		t.Fatalf("constraint id = %q", overflow.ConstraintID)
	}
	if overflow.ReasonCode != CodeCapacityOverflow {
		t.Fatalf("overflow code = %s, want %s", overflow.ReasonCode, CodeCapacityOverflow)
	}

	var accepted, throughput int64
	for _, tick := range b1.Truth.Ticks {
		throughput += tick.TotalThroughput
		for _, n := range tick.Nodes {
			accepted += int64(n.Accepted)
		}
	}
	if accepted != throughput {
		t.Fatalf("accepted=%d but completed throughput=%d", accepted, throughput)
	}
	last := b1.Truth.Ticks[len(b1.Truth.Ticks)-1]
	for _, n := range last.Nodes {
		if n.Inflight != 0 {
			t.Fatalf("node %s not drained: inflight=%d", n.NodeID, n.Inflight)
		}
	}
}

func TestWeightZeroNeverReceivesRequests(t *testing.T) {
	b := testBundle(t)
	p := CloneProject(b.Project)
	for i := range p.Nodes {
		if p.Nodes[i].ID == "cache-south-2" {
			p.Nodes[i].WeightPermyriad = 0
		}
	}
	sim, err := Simulate(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range sim.Ticks {
		for _, d := range tick.Decisions {
			if d.NodeID == "cache-south-2" {
				t.Fatalf("zero-weight node received request %s", d.RequestID)
			}
		}
	}
}

func TestPriorityAndDeclarationOrderArbitration(t *testing.T) {
	nodes := []Node{
		{ID: "later-order", Priority: 1, DeclarationOrder: 5, WeightPermyriad: 1},
		{ID: "priority-wins", Priority: 0, DeclarationOrder: 8, WeightPermyriad: 1},
	}
	got, basis := Arbitrate(nodes)
	if got.ID != "priority-wins" || basis != "ARB-PRI-priority-wins" {
		t.Fatalf("priority arbitration = %s/%s", got.ID, basis)
	}

	nodes = []Node{
		{ID: "second", Priority: 2, DeclarationOrder: 4, WeightPermyriad: 1},
		{ID: "first", Priority: 2, DeclarationOrder: 1, WeightPermyriad: 1},
	}
	got, basis = Arbitrate(nodes)
	if got.ID != "first" || basis != "ARB-ORD-first" {
		t.Fatalf("order arbitration = %s/%s", got.ID, basis)
	}

	req := Request{ID: "route-req"}
	d := Route(req, nodes, RoutingPolicy{Mode: "priority"})
	if d.NodeID != "first" || d.BasisID != "ARB-ORD-first" {
		t.Fatalf("priority route = %#v", d)
	}
}

func TestBreakerOpensAndRecoversOnLogicalMS(t *testing.T) {
	b := testBundle(t)
	var opens, recovers int
	var openTick, recoverTick int64 = -1, -1
	for _, tick := range b.Truth.Ticks {
		for _, ev := range tick.BreakerEvents {
			if ev.Opened {
				opens++
				openTick = ev.TickMS
				if ev.ReasonCode != "REASON_INFLIGHT_HIGH" && ev.ReasonCode != "REASON_ERROR_RATE" {
					t.Fatalf("unexpected breaker reason %q", ev.ReasonCode)
				}
				if ev.BasisID == "" {
					t.Fatal("breaker event missing basis id")
				}
			}
			if ev.Recovered {
				recovers++
				recoverTick = ev.TickMS
				if recoverTick < openTick {
					t.Fatal("recovery precedes open")
				}
			}
		}
	}
	if opens == 0 {
		t.Fatal("expected at least one deterministic breaker open event")
	}
	if recovers == 0 {
		t.Fatal("expected at least one logical-time recovery event")
	}
}

func TestCriticalFixedPointConversions(t *testing.T) {
	b := testBundle(t)
	reports := RunCriticalConversions(b.Project)
	if len(reports) != len(b.Project.CriticalCases) {
		t.Fatalf("reports=%d cases=%d", len(reports), len(b.Project.CriticalCases))
	}
	for _, r := range reports {
		if !r.WithinTolerance {
			t.Fatalf("critical case %s not within bound: got=%d expected=%d saturated=%v code=%s", r.ID, r.GotValue, r.ExpectedValue, r.Saturated, r.Code)
		}
	}

	permille := PermilleToPermyriad(400, 16)
	if permille.Value != 4000 || permille.Saturated {
		t.Fatalf("400 permille conversion = %#v", permille)
	}
	sat := PermilleToPermyriad(13, 8)
	if !sat.Saturated || sat.Value != 127 || sat.Err == nil || sat.Err.Code != CodeFixedOverflow {
		t.Fatalf("expected 8-bit saturation with E_FIXED_POINT_OVERFLOW, got %#v", sat)
	}
	ticks := TicksToMS(7, 5, 16)
	if ticks.Value != 35 {
		t.Fatalf("ticks to ms = %d, want 35", ticks.Value)
	}

	errors, requests := int64(9), int64(31)
	ratePPM, _ := ErrorRatePPM(errors, requests)
	limitPPM := int64(200000)
	openBefore := ratePPM > limitPPM
	converted := ratePPM
	for i := 0; i < 1000; i++ {
		myriad := ConvertScale(converted, PPMScale, ScalePermyriad, 64)
		permille := ConvertScale(myriad.Value, ScalePermyriad, ScalePermille, 64)
		back := ConvertScale(permille.Value, ScalePermille, PPMScale, 64)
		converted = back.Value
	}
	if openBefore != (converted > limitPPM) {
		t.Fatalf("cumulative conversion changed breaker decision: rate=%d converted=%d", ratePPM, converted)
	}
}

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	b := testBundle(t)
	edits := []Edit{
		{ID: "cap", Kind: EditMaxInflight, NodeID: "cache-north-1", AtMS: 20, IntValue: 3},
		{ID: "weight", Kind: EditNodeWeight, NodeID: "cache-south-2", AtMS: 15, IntValue: 0},
		{ID: "threshold", Kind: EditBreakerThreshold, NodeID: "cache-north-2", AtMS: 12, IntValue: 1},
		{ID: "add", Kind: EditAddRequest, AtMS: 22, Request: &Request{ID: "req-incremental", AtMS: 22, Candidates: []string{"cache-south-1"}}},
		{ID: "delete", Kind: EditDeleteRequest, RequestID: "req-0007"},
	}
	for _, edit := range edits {
		eng, err := NewIncrementalEngine(b.Project)
		if err != nil {
			t.Fatal(err)
		}
		report, err := eng.ApplyEdit(edit)
		if err != nil {
			t.Fatalf("ApplyEdit %s: %v", edit.ID, err)
		}
		full, equal, err := eng.VerifyAgainstFull()
		if err != nil {
			t.Fatal(err)
		}
		if !equal {
			t.Fatalf("incremental result for %s differs from full recomputation", edit.ID)
		}
		if full == nil {
			t.Fatal("missing full verification result")
		}
		if edit.AtMS > 0 {
			if len(report.ReusedTicks) != int(edit.AtMS) {
				t.Fatalf("%s reused ticks=%d want %d", edit.ID, len(report.ReusedTicks), edit.AtMS)
			}
			pv := report.Provenance["tick:5"]
			if !pv.Reused || pv.SourceEditID != edit.ID {
				t.Fatalf("tick:5 provenance = %#v", pv)
			}
			recomputeKey := "tick:" + itoa64(edit.AtMS)
			pv = report.Provenance[recomputeKey]
			if !pv.Recomputed || pv.SourceEditID != edit.ID {
				t.Fatalf("%s provenance = %#v", recomputeKey, pv)
			}
			if len(report.Closure) == 0 {
				t.Fatal("empty propagation closure")
			}
		}
	}
}

func TestParallelResultsStableAcrossBudgets(t *testing.T) {
	b := testBundle(t)
	for _, workers := range []int{1, 2, 4} {
		for _, buffer := range []int{1, 16} {
			rep, err := ParallelSimulate(b.Project, Budget{Workers: workers, BufferPerWorker: buffer})
			if err != nil {
				t.Fatalf("ParallelSimulate w=%d buf=%d: %v", workers, buffer, err)
			}
			if !ResultsEqual(rep.Result, b.Truth) {
				t.Fatalf("parallel mismatch workers=%d buffer=%d", workers, buffer)
			}
			if len(rep.NodeComputeNS) != len(b.Project.Nodes) {
				t.Fatalf("diagnostics nodes=%d want %d", len(rep.NodeComputeNS), len(b.Project.Nodes))
			}
		}
	}

	d, err := NewDispatcher(Budget{Workers: 1, BufferPerWorker: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AcquireWorker("n1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AcquireWorker("n2"); CodeOf(err) != CodeBackpressureThread {
		t.Fatalf("thread backpressure code=%v", err)
	}
	if err := d.AcquireBuffer("r1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AcquireBuffer("r2"); CodeOf(err) != CodeBackpressureBuffer {
		t.Fatalf("buffer backpressure code=%v", err)
	}
}

func TestSQLiteCheckpointRoundTripAndFaults(t *testing.T) {
	b := testBundle(t)
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	store, err := OpenSQLiteCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	last := b.Truth.Ticks[len(b.Truth.Ticks)-1]
	data := CheckpointData{ProjectSchemaVersion: SchemaV3, CommittedTick: last.TickMS, Nodes: last.Nodes, Windows: b.Truth.Windows}
	if err := store.SaveCommitted(data); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	magic, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(magic[:15]) != "SQLite format 3" {
		t.Fatalf("file is not sqlite: %q", magic[:15])
	}

	store, err = OpenSQLiteCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCommitted()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedTick != data.CommittedTick || len(loaded.Nodes) != len(data.Nodes) || len(loaded.Windows) != len(data.Windows) {
		t.Fatalf("loaded checkpoint = tick %d nodes %d windows %d", loaded.CommittedTick, len(loaded.Nodes), len(loaded.Windows))
	}
	if !reflect.DeepEqual(loaded.Nodes[0], data.Nodes[0]) {
		t.Fatalf("node roundtrip mismatch:\n%#v\n%#v", loaded.Nodes[0], data.Nodes[0])
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := TruncateCheckpointForInjection(path); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLiteCheckpoint(path)
	if CodeOf(err) != CodeCheckpointTruncated {
		t.Fatalf("truncated code=%s err=%v", CodeOf(err), err)
	}
}

func TestCheckpointVersionMismatchRebuilds(t *testing.T) {
	b := testBundle(t)
	path := filepath.Join(t.TempDir(), "version.sqlite")
	store, err := OpenSQLiteCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCommitted(CheckpointData{Version: "checkpoint.v0", CommittedTick: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenSQLiteCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.LoadCommitted()
	if CodeOf(err) != CodeCheckpointVersion {
		t.Fatalf("version code=%s err=%v", CodeOf(err), err)
	}
	_ = store.Close()

	recovery, err := RecoverOrRebuild(b.Project, path)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Rebuilt || recovery.Reason != CodeCheckpointVersion {
		t.Fatalf("recovery=%#v", recovery)
	}
	if !ResultsEqual(recovery.Result, b.Truth) {
		t.Fatal("rebuilt result differs from truth")
	}
}

func TestCheckpointUncommittedTickNotVisible(t *testing.T) {
	b := testBundle(t)
	path := filepath.Join(t.TempDir(), "crash.sqlite")
	store, err := OpenSQLiteCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	firstTick := b.Truth.Ticks[10]
	if err := store.SaveCommitted(CheckpointData{CommittedTick: firstTick.TickMS, Nodes: firstTick.Nodes}); err != nil {
		t.Fatal(err)
	}

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM node_state`); err != nil {
		t.Fatal(err)
	}
	crashTick := b.Truth.Ticks[11]
	for _, n := range crashTick.Nodes {
		if _, err := tx.Exec(`INSERT INTO node_state(tick,node_id,inflight,circuit_open,high_since,open_since,recovery_at,accepted,completed,errors_completed,latency_total_ms,overflow) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.TickMS, n.NodeID, n.Inflight, boolToInt(n.CircuitOpen), n.HighSinceMS, n.OpenSinceMS, n.RecoveryAtMS,
			n.Accepted, n.Completed, n.ErrorsCompleted, n.LatencyTotalMS, n.Overflow); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate process kill after node states but before committed_tick and
	// before transaction commit. Closing discards the uncommitted driver tx.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.LoadCommitted()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedTick != firstTick.TickMS {
		t.Fatalf("visible tick=%d, want last committed %d", loaded.CommittedTick, firstTick.TickMS)
	}
	if len(loaded.Nodes) != len(firstTick.Nodes) {
		t.Fatalf("node rows=%d want %d", len(loaded.Nodes), len(firstTick.Nodes))
	}
}

func TestStableExceptionCodesAndSourceUnmodified(t *testing.T) {
	before := reflect.ValueOf(bundleForFaults(t).Project)
	cases := []struct {
		inj  Injection
		code ErrorCode
	}{
		{InjectNegativeCapacity, CodeNegativeCapacity},
		{InjectWeightOutOfRange, CodeWeightScale},
		{InjectBreakerOrder, CodeBreakerOrder},
		{InjectMissingNode, CodeNodeNotFound},
	}
	for _, tc := range cases {
		b := bundleForFaults(t)
		_, err := InjectProjectFault(b.Project, tc.inj)
		if CodeOf(err) != tc.code {
			t.Fatalf("%s code=%s err=%v", tc.inj, CodeOf(err), err)
		}
		after := reflect.ValueOf(b.Project)
		if !reflect.DeepEqual(before.Interface(), after.Interface()) {
			t.Fatalf("source project modified by injection %s", tc.inj)
		}
	}

	if err := ValidateBudget(Budget{Workers: 0, BufferPerWorker: 8}); CodeOf(err) != CodeWorkerBudget {
		t.Fatalf("worker budget code=%v", err)
	}
	if err := ValidateBudget(Budget{Workers: 2, BufferPerWorker: 0}); CodeOf(err) != CodeBufferBudget {
		t.Fatalf("buffer budget code=%v", err)
	}
	if _, err := Load([]byte(`{"schema_version":"nope"}`)); CodeOf(err) != CodeSchemaVersion {
		t.Fatalf("schema code=%v", err)
	}
}

func bundleForFaults(t *testing.T) *Bundle {
	return testBundle(t)
}

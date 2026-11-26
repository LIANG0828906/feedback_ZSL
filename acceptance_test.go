package acceptance

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/checkpoint"
	"quota-cluster/internal/engine"
	"quota-cluster/internal/fixed"
	"quota-cluster/internal/incremental"
	"quota-cluster/internal/model"
	"quota-cluster/internal/projectio"
	"quota-cluster/internal/sample"
)

func generatedBundle(t *testing.T) *sample.Bundle {
	t.Helper()
	b, err := sample.Generate(42)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return b
}

func loadThree(t *testing.T, b *sample.Bundle) []*model.Project {
	t.Helper()
	docs := [][]byte{b.V1, b.V2, b.V3}
	out := make([]*model.Project, 0, 3)
	for i, doc := range docs {
		p, err := projectio.Load(doc)
		if err != nil {
			t.Fatalf("load v%d: %v", i+1, err)
		}
		p.SourceSchema = ""
		out = append(out, p)
	}
	want := *b.Project
	want.SourceSchema = ""
	for i, p := range out {
		if !reflect.DeepEqual(p, &want) {
			t.Fatalf("schema v%d canonical model mismatch", i+1)
		}
	}
	return out
}

func TestThreeSchemaLoadEquivalence(t *testing.T) {
	b := generatedBundle(t)
	projects := loadThree(t, b)
	for i, p := range projects {
		report, err := engine.Run(p, engine.Options{Workers: 4, BatchSize: 2})
		if err != nil {
			t.Fatalf("run schema %d: %v", i+1, err)
		}
		if !reflect.DeepEqual(report.Ticks, b.Truth.Ticks) {
			t.Fatalf("schema %d tick truth mismatch", i+1)
		}
		if !reflect.DeepEqual(report.Decisions, b.Truth.Decisions) {
			t.Fatalf("schema %d decision truth mismatch", i+1)
		}
	}
}

func TestGeneratorDeterministicByteForByte(t *testing.T) {
	a := generatedBundle(t)
	b := generatedBundle(t)
	if string(a.V1) != string(b.V1) || string(a.V2) != string(b.V2) || string(a.V3) != string(b.V3) {
		t.Fatal("schema documents are not byte-for-byte deterministic")
	}
	if string(a.TruthJSON) != string(b.TruthJSON) || string(a.CriticalJSON) != string(b.CriticalJSON) {
		t.Fatal("truth/critical artifacts are not byte-for-byte deterministic")
	}
}

func TestParallelAndBatchConclusionsStable(t *testing.T) {
	b := generatedBundle(t)
	cases := []engine.Options{
		{Workers: 1, BatchSize: 1},
		{Workers: 2, BatchSize: 1},
		{Workers: 4, BatchSize: 2},
		{Workers: 8, BatchSize: 3},
		{Workers: 16, BatchSize: 16},
	}
	var first *model.Report
	for _, opts := range cases {
		r, err := engine.Run(b.Project, opts)
		if err != nil {
			t.Fatalf("run %+v: %v", opts, err)
		}
		if first == nil {
			first = r
			continue
		}
		if !reflect.DeepEqual(first.Ticks, r.Ticks) || !reflect.DeepEqual(first.Decisions, r.Decisions) {
			t.Fatalf("results changed for options %+v", opts)
		}
	}
}

func TestLedgerInvariantsFairnessAndBackpressureCode(t *testing.T) {
	b := generatedBundle(t)
	for _, tick := range b.Truth.Ticks {
		for _, ts := range tick.TenantStates {
			ceiling := ts.AvailableQuotaBytes + ts.BurstLimitBytes
			if ts.UsedBytes > ceiling {
				t.Fatalf("tick %d tenant %s used %d > ceiling %d", tick.Tick, ts.TenantID, ts.UsedBytes, ceiling)
			}
			wantGuarantee := max64Test(0, ts.GuaranteeBytes-ts.UsedBytes)
			if ts.GuaranteeRemainingBytes != wantGuarantee {
				t.Fatalf("guarantee math mismatch for %s", ts.TenantID)
			}
		}
		for _, ns := range tick.NodeStates {
			if ns.UsedBytes > ns.CapacityBytes {
				t.Fatalf("tick %d node %s over capacity", tick.Tick, ns.NodeID)
			}
		}
	}

	index := map[string]int{}
	var thetaReject *model.Decision
	for i, d := range b.Truth.Decisions {
		if _, exists := index[d.RequestID]; !exists {
			index[d.RequestID] = i
		}
		if d.RequestID == "req-theta-3" && d.Verdict == model.VerdictReject {
			dd := d
			thetaReject = &dd
		}
	}
	for _, id := range []string{"req-beta", "req-alpha", "req-gamma", "req-delta"} {
		if index[id] >= index["req-epsilon"] {
			t.Fatalf("guarantee request %s was not ordered before pure-burst epsilon", id)
		}
	}
	if index["req-theta-1"] >= index["req-theta-2"] || index["req-theta-2"] >= index["req-theta-3"] {
		t.Fatal("same-tenant tie order is not request sequence order")
	}
	if thetaReject == nil || thetaReject.ReasonCode != bizerror.CodeQueueLimit || thetaReject.Constraint != engine.ConstraintWaitQueueLimit {
		t.Fatalf("expected stable queue-limit business code, got %+v", thetaReject)
	}
	if thetaReject.RetryTick <= thetaReject.Tick {
		t.Fatal("rejected queue-limit request lacks a future retry tick")
	}

	foundEncroacher := false
	for _, d := range b.Truth.Decisions {
		if d.EncroacherTenantID != "" && d.Verdict == model.VerdictQueue {
			foundEncroacher = true
		}
	}
	if !foundEncroacher {
		t.Fatal("expected at least one queue decision with encroacher source tenant")
	}
}

func TestBoundedExecutorBackpressure(t *testing.T) {
	exec := engine.NewBoundedExecutor(1, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := exec.TrySubmit(func() { close(started); <-release }); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := exec.TrySubmit(func() {}); err != nil {
		t.Fatalf("buffered task should fit: %v", err)
	}
	err := exec.TrySubmit(func() {})
	if be, ok := bizerror.As(err); !ok || be.Code != bizerror.CodeBackpressureWorkers {
		t.Fatalf("expected worker backpressure code, got %v", err)
	}
	close(release)
	exec.CloseAndWait()
}

func TestGateLatencyBudgetExceeded(t *testing.T) {
	b := generatedBundle(t)
	for i := range b.Project.Events {
		if b.Project.Events[i].RequestID == "req-eta-background" {
			b.Project.Events[i].LatencyBudgetMS = 1
		}
	}
	r, err := engine.Run(b.Project, engine.Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range r.Decisions {
		if d.RequestID == "req-eta-background" {
			if d.Verdict != model.VerdictReject || d.ReasonCode != bizerror.CodeGateBudget || d.Constraint != engine.ConstraintGateLatency {
				t.Fatalf("expected latency budget rejection, got %+v", d)
			}
			return
		}
	}
	t.Fatal("latency budget decision not found")
}

func TestFixedPointConversionsAndCriticalCases(t *testing.T) {
	if got := fixed.ScalePermilleToBasisPoints(250); got.Value != 2500 || !got.Exact {
		t.Fatalf("permille conversion: %+v", got)
	}
	if got := fixed.KiBToBytes(1024); got.Value != 1024*1024 {
		t.Fatalf("KiB conversion: %+v", got)
	}
	if got := fixed.TicksToMillis(7, 10); got.Value != 70 {
		t.Fatalf("tick conversion: %+v", got)
	}
	if got := fixed.ScalePermilleToBasisPoints(1 << 62); !got.Overflow || !got.Saturated || got.Code != bizerror.CodeScaleOverflow {
		t.Fatalf("expected overflow/saturation, got %+v", got)
	}
	b := generatedBundle(t)
	for _, c := range b.CriticalCases {
		if c.Conversion.Overflow {
			if c.Status != "overflow_saturated_rejected_by_loader" {
				t.Fatalf("overflow case %s has wrong status", c.Name)
			}
			continue
		}
		if c.ExceedsTolerance {
			t.Fatalf("non-overflow critical case %s exceeds tolerance", c.Name)
		}
	}
}

func TestIncrementalSuffixEquivalence(t *testing.T) {
	b := generatedBundle(t)
	svc, err := incremental.NewService(b.Project, engine.Options{Workers: 3, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	edits := []model.Edit{
		{Kind: model.EditNodeCapacity, TargetID: "node-a", AtTick: 2, Value: b.Project.Nodes[0].CapacityBytes + sample.MiB*256},
		{Kind: model.EditTenantQuota, TargetID: "tenant-alpha", AtTick: 1, Value: sample.MiB * 3000},
		{Kind: model.EditTenantGuarantee, TargetID: "tenant-beta", AtTick: 2, Value: sample.MiB * 250},
		{Kind: model.EditWeightEvent, EventSequence: 4, Value: 180},
	}
	for _, edit := range edits {
		result, err := svc.Apply(edit)
		if err != nil {
			t.Fatalf("apply %+v: %v", edit, err)
		}
		if !result.FullEqual {
			t.Fatalf("edit %+v incremental result differs from full recompute", edit)
		}
		start := result.StartTick
		if result.ReusedTicks != int(start) || result.RecomputedTicks != int(b.Project.LogicalTicks-start+1) {
			t.Fatalf("unexpected reuse counts: %+v", result)
		}
		for _, p := range result.Provenance {
			if p.Status == "reused" && !strings.HasPrefix(p.Source, "cache:") {
				t.Fatalf("reused tick lacks cache source: %+v", p)
			}
			if p.Status == "recomputed" && !strings.HasPrefix(p.Source, "invalidation:") {
				t.Fatalf("recomputed tick lacks invalidation source: %+v", p)
			}
		}
	}
}

func TestSQLiteCheckpointRoundTripAndVersionGate(t *testing.T) {
	b := generatedBundle(t)
	run, err := engine.RunWithStates(b.Project, engine.Options{Workers: 4, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	finalTick := b.Project.LogicalTicks
	last := run.Report.Ticks[len(run.Report.Ticks)-1]
	cp := &checkpoint.Checkpoint{
		FormatVersion: model.FormatVersion, ProjectID: b.Project.ProjectID,
		ProjectHash: "seed-42-hash", CommittedTick: finalTick,
		TenantStates: sortedTenantStates(last.TenantStates),
		NodeStates:   sortedNodeStates(last.NodeStates),
		Queue:        last.Queue, Decisions: run.Report.Decisions,
		EngineState: run.States[finalTick],
	}
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	if err := checkpoint.SaveCheckpoint(path, cp, checkpoint.SaveOptions{ProjectHash: "seed-42-hash", State: run.States[finalTick]}); err != nil {
		t.Fatal(err)
	}
	loaded, err := checkpoint.LoadCheckpoint(path, "seed-42-hash")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedTick != finalTick || len(loaded.TenantStates) != len(cp.TenantStates) || len(loaded.NodeStates) != len(cp.NodeStates) {
		t.Fatalf("checkpoint summary mismatch: tick=%d tenants=%d nodes=%d", loaded.CommittedTick, len(loaded.TenantStates), len(loaded.NodeStates))
	}
	if !reflect.DeepEqual(loaded.TenantStates, cp.TenantStates) {
		t.Fatal("tenant checkpoint payload mismatch")
	}
	if !reflect.DeepEqual(loaded.EngineState, cp.EngineState) {
		t.Fatal("engine state checkpoint payload mismatch")
	}

	badVersion := *cp
	badVersion.FormatVersion = "checkpoint-v0"
	badPath := filepath.Join(t.TempDir(), "bad-version.sqlite")
	if err := checkpoint.SaveCheckpoint(badPath, &badVersion, checkpoint.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.LoadCheckpoint(badPath, "")
	if be, ok := bizerror.As(err); !ok || be.Code != bizerror.CodeCheckpointVersion {
		t.Fatalf("expected checkpoint version mismatch, got %v", err)
	}
	_, err = checkpoint.LoadCheckpoint(path, "different-hash")
	if be, ok := bizerror.As(err); !ok || be.Code != bizerror.CodeCheckpointVersion {
		t.Fatalf("expected project hash version gate, got %v", err)
	}
}

func sortedTenantStates(in map[string]model.TenantSnapshot) []model.TenantSnapshot {
	out := make([]model.TenantSnapshot, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out
}

func sortedNodeStates(in map[string]model.NodeSnapshot) []model.NodeSnapshot {
	out := make([]model.NodeSnapshot, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func max64Test(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

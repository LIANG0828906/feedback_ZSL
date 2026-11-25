package thermalcache

import (
	"testing"
)

func TestFixedSeedEngineCore(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	fingerprintBefore := p.Fingerprint()
	res := runProject(t, p)
	if p.Fingerprint() != fingerprintBefore {
		t.Fatal("engine execution modified the source project")
	}

	if len(res.Timeline) != 7 || res.Timeline[0] != 0 || res.Timeline[6] != 600 {
		t.Fatalf("unexpected timeline: %v", res.Timeline)
	}
	if len(res.Ticks) != 7 {
		t.Fatalf("expected 7 tick results, got %d", len(res.Ticks))
	}
	if !res.Finished || len(res.Batches) == 0 {
		t.Fatalf("run did not finish with batches: finished=%v batches=%d", res.Finished, len(res.Batches))
	}

	tick100 := tickAt(t, res, 100)
	o05 := objectAt(t, tick100, "o05")
	if o05.Tier != TierCold || o05.AnswerTier != TierCold || !o05.Migrating || o05.TargetTier != TierHot {
		t.Fatalf("o05 migration gate state wrong at 100: %+v", o05)
	}
	if o05.PendingWrites != 1 || o05.SourceVersion != 1 || o05.TargetVersion != 0 || o05.TargetVisible {
		t.Fatalf("o05 write-ahead gate state wrong at 100: %+v", o05)
	}
	if o05.GateDenials != 1 {
		t.Fatalf("o05 expected one target gate denial, got %d", o05.GateDenials)
	}
	var sawTargetDenial bool
	for _, gate := range tick100.Gate {
		if gate.ObjectID == "o05" && gate.Kind == OpRead && gate.RequestedTier == TierHot {
			sawTargetDenial = gate.Code == CodeGateTargetNotVisible && !gate.Allowed && gate.AnsweredTier == TierCold
		}
	}
	if !sawTargetDenial {
		t.Fatal("target read during migration was not rejected with E_GATE_TARGET_NOT_VISIBLE")
	}

	tick200 := tickAt(t, res, 200)
	o05 = objectAt(t, tick200, "o05")
	if o05.Tier != TierHot || o05.AnswerTier != TierHot || o05.Migrating || !o05.TargetVisible {
		t.Fatalf("o05 did not switch answer layer after commit: %+v", o05)
	}
	if o05.SourceVersion != 1 || o05.TargetVersion != 1 || o05.PendingWrites != 0 {
		t.Fatalf("o05 pending write was not applied at commit: %+v", o05)
	}

	o09 := objectAt(t, tickAt(t, res, 400), "o09")
	if !o09.Expired || o09.ReasonID != "R_TTL_EXPIRED" {
		t.Fatalf("o09 TTL expiry decision missing: %+v", o09)
	}

	var forced *BatchResult
	for i := range res.Batches {
		if res.Batches[i].Forced {
			forced = &res.Batches[i]
		}
	}
	if forced == nil || forced.StartedAtMs != 500 || forced.Target != TierHot ||
		len(forced.ObjectIDs) != 1 || forced.ObjectIDs[0] != "o10" {
		t.Fatalf("forced migration batch missing or wrong: %+v", forced)
	}
	o10 := objectAt(t, tickAt(t, res, 500), "o10")
	if !o10.Migrating || o10.AnswerTier != TierCold || o10.PendingWrites != 1 || o10.GateDenials != 1 {
		t.Fatalf("o10 forced migration gate state wrong: %+v", o10)
	}
	o10 = objectAt(t, tickAt(t, res, 600), "o10")
	if o10.Tier != TierHot || o10.Migrating || o10.TargetVersion != 1 || !o10.TargetVisible {
		t.Fatalf("o10 did not commit forced migration: %+v", o10)
	}

	for _, b := range res.Batches {
		if len(b.ObjectIDs) > p.Limits.MaxBatchObjects {
			t.Fatalf("batch %s exceeds object limit", b.ID)
		}
		var bytes int64
		for _, id := range b.ObjectIDs {
			obj := p.objectByID(id)
			if obj == nil {
				t.Fatalf("batch %s has unknown object %s", b.ID, id)
			}
			bytes += obj.SizeBytes
		}
		if bytes > p.Limits.MaxBatchBytes {
			t.Fatalf("batch %s exceeds byte limit", b.ID)
		}
	}

	batch3, ok := findBatch(res, "batch-0003")
	if !ok {
		t.Fatal("expected batch-0003")
	}
	if len(batch3.Deferred) == 0 || batch3.Deferred[0].ObjectID != "o07" ||
		batch3.Deferred[0].Code != ConstraintBatchBytes {
		t.Fatalf("expected deterministic o07 C_BATCH_BYTES deferral: %+v", batch3.Deferred)
	}

	for _, tick := range res.Ticks {
		if tick.Occupancy.HotUsed > tick.Occupancy.HotCapacity || tick.Occupancy.ColdUsed > tick.Occupancy.ColdCapacity {
			t.Fatalf("actual occupancy exceeded capacity at %d: %+v", tick.AtMs, tick.Occupancy)
		}
	}
}

func TestSchemaAndWorkerInvariance(t *testing.T) {
	f := fixture42(t)
	var baseline string
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		p := loadVersion(t, f, version)
		for _, workers := range []int{1, 2, 4} {
			cp := p.Clone()
			cp.Limits.WorkerBudget = workers
			got := normalizedResultJSON(t, runProject(t, cp))
			if baseline == "" {
				baseline = got
			} else if got != baseline {
				t.Fatalf("result differs for schema=%s workers=%d", version, workers)
			}
		}
	}
}

func TestScoreBoundaryAndSaturation(t *testing.T) {
	cfg := ScoreConfig{
		Bits: 32, Scale: 1, CountWeight: 1,
		PromoteThreshold: 3000, DemoteThreshold: 1000,
		Boundary: PromoteAtBoundary,
	}
	atBoundary := ComputeScore(cfg, 3000, 0, 0, 5000, 100)
	if atBoundary.Score != 3000 || atBoundary.Overflowed {
		t.Fatalf("boundary score mismatch: %+v", atBoundary)
	}
	d := Decide(cfg, TierCold, atBoundary, nil)
	if d.Direction != DirectionPromote || d.Reason != ReasonPromoteBoundary || !d.Boundary {
		t.Fatalf("cold object at promote boundary must promote: %+v", d)
	}
	hotAtDemote := ComputeScore(cfg, 1000, 0, 0, 5000, 100)
	d = Decide(cfg, TierHot, hotAtDemote, nil)
	if d.Direction != DirectionDemote || d.Reason != ReasonDemoteBoundary {
		t.Fatalf("hot object at demote boundary must demote in promote-inclusive mode: %+v", d)
	}
	cfg.Boundary = DemoteAtBoundary
	d = Decide(cfg, TierCold, atBoundary, nil)
	if d.Direction != DirectionHold {
		t.Fatalf("promote-exclusive boundary must hold: %+v", d)
	}
	cfg.Boundary = PromoteAtBoundary
	overflow := ComputeScore(cfg, int64(^uint64(0)>>1), 0, 0, 0, 100)
	if !overflow.Saturated || !overflow.Overflowed || overflow.Score != signedMax(32) {
		t.Fatalf("score overflow was not saturated/reported: %+v", overflow)
	}
}

func TestMigrationInterruptRollsBackSource(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunOne(); err != nil {
		t.Fatal(err)
	}
	before := eng.Occupancy()
	batch, err := eng.StartMigration("o05", TierHot)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || batch.ID == "" {
		t.Fatal("manual migration did not create a batch")
	}
	if _, err := eng.Gate("o05", OpWrite, ""); err != nil {
		t.Fatal(err)
	}
	st, _ := eng.State("o05")
	if !st.Migrating || st.Tier != TierCold || st.PendingWrites != 1 {
		t.Fatalf("manual migration not staged correctly: %+v", st)
	}
	if err := eng.InterruptBatch(batch.ID); err != nil {
		t.Fatal(err)
	}
	after := eng.Occupancy()
	st, _ = eng.State("o05")
	if st.Migrating || st.Tier != TierCold || st.PendingWrites != 0 || st.SourceVersion != 1 {
		t.Fatalf("interrupt did not roll back migration while retaining source version: %+v", st)
	}
	if after.HotUsed != before.HotUsed || after.ColdUsed != before.ColdUsed ||
		after.HotReserved != 0 || after.ColdReserved != 0 {
		t.Fatalf("interrupt changed actual occupancy: before=%+v after=%+v", before, after)
	}
}

func TestPartialCommitAndResumeInvariant(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	p.Limits.MaxBatchBytes = 8192
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.RunOne(); err != nil {
			t.Fatal(err)
		}
	}
	var activeID string
	for _, id := range eng.batchOrder {
		b := eng.batches[id]
		if (b.Status == "staged" || b.Status == "partial") && len(b.ObjectIDs) == 2 {
			activeID = id
			break
		}
	}
	if activeID == "" {
		t.Fatalf("expected a two-object staged batch at 200")
	}
	b := eng.batches[activeID]
	first := b.ObjectIDs[0]
	second := b.ObjectIDs[1]
	if err := eng.CommitBatchStep(activeID, 1); err != nil {
		t.Fatal(err)
	}
	st1, _ := eng.State(first)
	st2, _ := eng.State(second)
	if st1.Tier != TierHot || st1.Migrating {
		t.Fatalf("partially committed object not in target: %+v", st1)
	}
	if st2.Tier != TierCold || !st2.Migrating {
		t.Fatalf("uncommitted object should still answer source: %+v", st2)
	}
	if err := eng.InterruptBatch(activeID); err != nil {
		t.Fatal(err)
	}
	st2, _ = eng.State(second)
	if st2.Tier != TierCold || st2.Migrating {
		t.Fatalf("interrupt after partial commit lost source object: %+v", st2)
	}
	st1, _ = eng.State(first)
	if st1.Tier != TierHot || st1.Migrating {
		t.Fatalf("partial commit point was rolled back: %+v", st1)
	}
	// Every object is charged to exactly one layer.
	occ := eng.Occupancy()
	var sum int64
	for _, id := range eng.objectOrder {
		sum += eng.objectByID(id).SizeBytes
	}
	if occ.HotUsed+occ.ColdUsed != sum {
		t.Fatalf("object bytes double-counted or lost: hot=%d cold=%d sum=%d", occ.HotUsed, occ.ColdUsed, sum)
	}
}

func TestBufferBackpressureIsObservable(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	p.Limits.PerLayerBuffer = 1
	res := runProject(t, p)
	tick := tickAt(t, res, 200)
	if len(tick.Backpressure) == 0 {
		t.Fatalf("expected migration backpressure at 200: %+v", tick)
	}
	found := false
	for _, bp := range tick.Backpressure {
		if bp.Code == CodeMigrationBackpressure && bp.Candidates > bp.Limit {
			found = true
		}
	}
	if !found {
		t.Fatalf("no E_MIGRATION_BACKPRESSURE observation: %+v", tick.Backpressure)
	}
	for _, obj := range tick.Objects {
		if obj.ObjectID == "o07" && obj.DeferredCode != ConstraintBuffer {
			t.Fatalf("backpressured object not reported as deferred: %+v", obj)
		}
	}
}

func TestCapacityAndObjectConstraintDeferrals(t *testing.T) {
	f := fixture42(t)

	t.Run("capacity", func(t *testing.T) {
		p := loadVersion(t, f, SchemaV1)
		for i := range p.Nodes {
			if p.Nodes[i].Tier == TierHot {
				p.Nodes[i].CapacityBytes = 5120
			}
		}
		eng, err := NewEngine(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.RunOne(); err != nil {
			t.Fatal(err)
		}
		tick, err := eng.RunOne()
		if err != nil {
			t.Fatal(err)
		}
		o05 := objectAt(t, *tick, "o05")
		if o05.DeferredCode != ConstraintCapacityHot {
			t.Fatalf("expected %s, got %+v", ConstraintCapacityHot, o05)
		}
		if len(tick.BatchesStarted) != 0 {
			t.Fatalf("capacity-constrained promotion should not start a batch: %v", tick.BatchesStarted)
		}
	})

	t.Run("objects", func(t *testing.T) {
		p := loadVersion(t, f, SchemaV1)
		p.Limits.MaxBatchObjects = 1
		p.Limits.MaxBatchBytes = 8192
		res := runProject(t, p)
		batch, ok := findBatch(res, "batch-0003")
		if !ok {
			t.Fatal("batch-0003 missing")
		}
		if len(batch.ObjectIDs) != 1 || len(batch.Deferred) == 0 ||
			batch.Deferred[0].ObjectID != "o07" || batch.Deferred[0].Code != ConstraintBatchObjects {
			t.Fatalf("object-limit deferral missing: %+v", batch)
		}
	})
}

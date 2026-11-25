package thermalcache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteCheckpointRoundTripAndRecovery(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	p.Limits.CommitDelayMs = 0
	full := runProject(t, p)

	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.RunOne(); err != nil {
			t.Fatal(err)
		}
	}
	prefix := append([]TickResult(nil), eng.Result().Ticks...)
	if err := eng.Checkpoint(path); err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) < 16 || !bytes.Equal(head[:16], []byte("SQLite format 3\x00")) {
		t.Fatal("checkpoint is not a SQLite database")
	}

	db, err := openSQLiteDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT section FROM checkpoint_write_log ORDER BY seq`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if len(rows) < 4 {
		t.Fatalf("checkpoint write log is incomplete: %v", rows)
	}
	var sections []string
	for _, row := range rows {
		sections = append(sections, rowString(row, 0, ""))
	}
	if sections[len(sections)-1] != "meta" {
		t.Fatalf("global tick metadata was not written last: %v", sections)
	}
	if sections[0] != "objects" {
		t.Fatalf("object/layer state was not written first: %v", sections)
	}

	cp, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if cp.SchemaVersion != SchemaV1 || cp.NextTimeline != 3 || len(cp.Objects) != len(p.Objects) {
		t.Fatalf("checkpoint metadata/content mismatch: %+v", cp)
	}
	recovered, err := NewEngineFromCheckpoint(p, path)
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := recovered.Run()
	if err != nil {
		t.Fatal(err)
	}
	combined := *suffix
	combined.Ticks = append(append([]TickResult(nil), prefix...), suffix.Ticks...)
	if got, want := normalizedResultJSON(t, &combined), normalizedResultJSON(t, full); got != want {
		t.Fatalf("recovered continuation differs from uninterrupted run")
	}
	for _, tick := range combined.Ticks {
		if tick.Occupancy.HotUsed > tick.Occupancy.HotCapacity || tick.Occupancy.ColdUsed > tick.Occupancy.ColdCapacity {
			t.Fatalf("recovered occupancy exceeded capacity at %d", tick.AtMs)
		}
	}
}

func TestCheckpointTruncatedAndVersionMismatch(t *testing.T) {
	f := fixture42(t)
	p1 := loadVersion(t, f, SchemaV1)
	p2 := loadVersion(t, f, SchemaV2)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.sqlite")
	eng, _ := NewEngine(p1)
	if _, err := eng.RunOne(); err != nil {
		t.Fatal(err)
	}
	if err := eng.Checkpoint(good); err != nil {
		t.Fatal(err)
	}
	// A version mismatch must be rejected even though the normalized content
	// fingerprint of v1 and v2 is identical.
	eng2, _ := NewEngine(p2)
	assertCode(t, eng2.RestoreCheckpoint(good), CodeCheckpointVersion)

	truncated := filepath.Join(dir, "truncated.sqlite")
	data, _ := os.ReadFile(good)
	if err := os.WriteFile(truncated, data[:64], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCheckpoint(truncated)
	assertCode(t, err, CodeCheckpointTruncated)

	changed := p1.Clone()
	changed.Objects[0].Priority++
	eng3, _ := NewEngine(changed)
	assertCode(t, eng3.RestoreCheckpoint(good), CodeCheckpointVersion)
}

func TestPartialBatchCheckpointResumesAtLastCommit(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	p.Limits.MaxBatchBytes = 8192
	p.OpEvents = append(p.OpEvents, OpEvent{AtMs: 200, ObjectID: "o07", Kind: OpWrite, RequestedTier: ""})
	path := filepath.Join(t.TempDir(), "partial.sqlite")
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.RunOne(); err != nil {
			t.Fatal(err)
		}
	}
	var batchID string
	for _, id := range eng.batchOrder {
		b := eng.batches[id]
		if (b.Status == "staged" || b.Status == "partial") && len(b.ObjectIDs) == 2 {
			batchID = id
			break
		}
	}
	if batchID == "" {
		t.Fatal("two-object batch was not staged")
	}
	b := eng.batches[batchID]
	committedObject := b.ObjectIDs[0]
	pendingObject := b.ObjectIDs[1]
	if err := eng.CommitBatchStep(batchID, 1); err != nil {
		t.Fatal(err)
	}
	before := eng.Occupancy()
	if err := eng.Checkpoint(path); err != nil {
		t.Fatal(err)
	}
	restored, err := NewEngineFromCheckpoint(p, path)
	if err != nil {
		t.Fatal(err)
	}
	stCommitted, _ := restored.State(committedObject)
	stPending, _ := restored.State(pendingObject)
	if stCommitted.Tier != TierHot || stCommitted.Migrating {
		t.Fatalf("committed part not restored at target: %+v", stCommitted)
	}
	if stPending.Tier != TierCold || !stPending.Migrating {
		t.Fatalf("remaining part did not resume from source: %+v", stPending)
	}
	if stPending.PendingWrites != 1 || stPending.SourceVersion != 1 || stPending.TargetVersion != 0 {
		t.Fatalf("pending write gate state was not persisted: %+v", stPending)
	}
	after := restored.Occupancy()
	if after.HotUsed != before.HotUsed || after.ColdUsed != before.ColdUsed ||
		after.HotReserved != before.HotReserved {
		t.Fatalf("partial checkpoint occupancy changed: before=%+v after=%+v", before, after)
	}
	if _, err := restored.Run(); err != nil {
		t.Fatal(err)
	}
	stPending, _ = restored.State(pendingObject)
	if stPending.Tier != TierHot || stPending.Migrating {
		t.Fatalf("remaining part did not commit after recovery: %+v", stPending)
	}
	if stPending.TargetVersion != 1 || stPending.PendingWrites != 0 {
		t.Fatalf("pending write was not applied after resumed commit: %+v", stPending)
	}
	final := restored.Occupancy()
	var total int64
	for _, o := range p.Objects {
		total += o.SizeBytes
	}
	if final.HotUsed+final.ColdUsed != total {
		t.Fatalf("recovered object capacity double-counted or lost: %+v total=%d", final, total)
	}
}

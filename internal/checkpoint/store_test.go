package checkpoint

import (
	"context"
	"path/filepath"
	"testing"

	"atelier/internal/engine"
	"atelier/internal/model"
	"atelier/internal/sample"
)

func TestCheckpointCommitValidationAndForcedRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "checkpoints.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sc := sample.Generate(sample.DefaultSeed)
	e, err := engine.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	e.SetCheckpointSink(store)
	snap, err := e.FullRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Checkpoint == nil || snap.Checkpoint.CommittedZones != len(sc.Region.Zones) {
		t.Fatalf("checkpoint status missing: %+v", snap.Checkpoint)
	}
	req := VerifyRequest{
		ExpectedScenarioHash:  e.ScenarioHash(),
		ExpectedAlgorithm:     model.AlgorithmVersion,
		ExpectedParamsVersion: e.ExpectedParamsVersions(),
		ExpectedZoneOrder:     e.ZoneOrder(),
	}
	recovered, err := store.LoadLatest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Valid || recovered.RecoverySource != SourceCompleted || len(recovered.Zones) != len(sc.Region.Zones) {
		t.Fatalf("completed checkpoint not recovered: %+v", recovered)
	}
	for _, zone := range recovered.Zones {
		expected := stateFromSnapshot(snap, zone.ZoneID)
		if expected.ContentHash != zone.ContentHash || expected.HeatSummary != zone.HeatSummary {
			t.Fatalf("recovered zone %s differs from committed snapshot", zone.ZoneID)
		}
	}
	_ = store.Close()

	// Simulate forced termination: start a session and commit only a prefix,
	// then close without marking the session complete/cancelled.
	forcedStore, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := forcedStore.BeginSession(ctx, e.ScenarioHash(), e.ZoneOrder())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := forcedStore.SaveZone(ctx, sessionID, snap.ZoneStates[i]); err != nil {
			t.Fatal(err)
		}
	}
	_ = forcedStore.Close()

	restarted, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	recovered, err = restarted.LoadLatest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Valid || recovered.RecoverySource != SourceForcedTermination || len(recovered.Zones) != 2 {
		t.Fatalf("forced recovery mismatch: %+v", recovered)
	}

	recoveredEngine, err := engine.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	recoveredEngine.SetCheckpointSink(restarted)
	resumed, err := recoveredEngine.ResumeFromCheckpoint(ctx, recovered.Zones, engine.RecoveryReport{
		Source: SourceForcedTermination, SnapshotStatus: StatusRunning,
		ResumedZones:       []string{recovered.Zones[0].ZoneID, recovered.Zones[1].ZoneID},
		RecoveredFromStore: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Recovery == nil || len(resumed.ZoneStates) != len(sc.Region.Zones) {
		t.Fatalf("resume did not complete workspace: %+v", resumed.Recovery)
	}
	result, _, err := recoveredEngine.CompareWithFull(ctx)
	if err != nil || !result.Equal {
		t.Fatalf("resumed workspace differs from full: result=%+v err=%v", result, err)
	}
}

func TestActiveCancelSourceAndVersionMismatchDiscardsAll(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sc := sample.Generate(sample.DefaultSeed)
	e, err := engine.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := store.BeginSession(ctx, e.ScenarioHash(), e.ZoneOrder())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := e.FullRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveZone(ctx, sessionID, snap.ZoneStates[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishSession(ctx, sessionID, StatusCancelled); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	req := VerifyRequest{
		ExpectedScenarioHash:  e.ScenarioHash(),
		ExpectedAlgorithm:     model.AlgorithmVersion,
		ExpectedParamsVersion: e.ExpectedParamsVersions(),
		ExpectedZoneOrder:     e.ZoneOrder(),
	}
	recovered, err := store.LoadLatest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Valid || recovered.RecoverySource != SourceActiveCancel || len(recovered.Zones) != 1 {
		t.Fatalf("active-cancel source mismatch: %+v", recovered)
	}
	_ = store.Close()

	// Corrupt a parameter version. Any mismatch must discard the complete snapshot.
	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.db.ExecContext(ctx, `UPDATE zone_checkpoints SET params_version = 'tampered' WHERE session_id = ?`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err = store.LoadLatest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Valid || !recovered.Discarded || recovered.DiscardReason == "" {
		t.Fatalf("version mismatch was not fully discarded: %+v", recovered)
	}
	rows, err := store.CheckpointRows(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("discarded snapshot left %d mixed zone rows", len(rows))
	}
}

func TestContentHashMismatchDiscardsAllZones(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sc := sample.Generate(sample.DefaultSeed)
	e, err := engine.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	e.SetCheckpointSink(store)
	snap, err := e.FullRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := snap.Checkpoint.SessionID
	_, err = store.db.ExecContext(ctx, `UPDATE zone_checkpoints SET payload = ? WHERE session_id = ? AND ordinal = 1`,
		[]byte(`{"tampered":true}`), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	req := VerifyRequest{
		ExpectedScenarioHash:  e.ScenarioHash(),
		ExpectedAlgorithm:     model.AlgorithmVersion,
		ExpectedParamsVersion: e.ExpectedParamsVersions(),
		ExpectedZoneOrder:     e.ZoneOrder(),
	}
	recovered, err := store.LoadLatest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Valid || !recovered.Discarded || len(recovered.Zones) != 0 {
		t.Fatalf("content hash mismatch did not discard all: %+v", recovered)
	}
	if rows, _ := store.CheckpointRows(ctx, sessionID); len(rows) != 0 {
		t.Fatalf("partial rows remain after content hash mismatch")
	}
}

func TestOutOfOrderZoneCommitIsRejected(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sc := sample.Generate(sample.DefaultSeed)
	e, err := engine.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := e.FullRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := store.BeginSession(ctx, e.ScenarioHash(), e.ZoneOrder())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveZone(ctx, sessionID, snap.ZoneStates[1]); err == nil {
		t.Fatal("expected out-of-order zone commit to fail")
	}
	rows, err := store.CheckpointRows(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("out-of-order commit left %d rows", len(rows))
	}
	if err := store.SaveZone(ctx, sessionID, snap.ZoneStates[0]); err != nil {
		t.Fatalf("ordered commit after rejected attempt failed: %v", err)
	}
}

func stateFromSnapshot(snap engine.Snapshot, zoneID string) model.ZoneState {
	for _, state := range snap.ZoneStates {
		if state.ZoneID == zoneID {
			return state
		}
	}
	return model.ZoneState{}
}

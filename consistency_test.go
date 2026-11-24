package roomworkbench_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"roomworkbench/internal/store"
	"roomworkbench/internal/wb"
)

func TestGeneratedSampleJSONLoads(t *testing.T) {
	raw, err := json.Marshal(wb.GenerateSample(wb.DefaultSeed))
	if err != nil {
		t.Fatal(err)
	}
	loaded, apiErr := wb.LoadSample(raw)
	if apiErr != nil {
		t.Fatalf("LoadSample generated JSON: %v", apiErr)
	}
	if loaded.Seed != wb.DefaultSeed || len(loaded.Rooms) != 4 || len(loaded.Truth.Paths) != 12 {
		t.Fatalf("unexpected loaded sample: seed=%d rooms=%d truth=%d",
			loaded.Seed, len(loaded.Rooms), len(loaded.Truth.Paths))
	}
}

func TestDeterministicSampleTruthAndState(t *testing.T) {
	ctx := context.Background()
	sample := wb.GenerateSample(wb.DefaultSeed)
	if apiErr := wb.ValidateSample(sample); apiErr != nil {
		t.Fatalf("sample invalid: %v", apiErr)
	}

	engine, apiErr := wb.NewEngine(sample, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	report, apiErr := engine.RunFull(ctx, true)
	if apiErr != nil {
		t.Fatalf("full recompute failed: %v", apiErr)
	}
	if report == nil {
		t.Fatal("missing report")
	}
	if apiErr := engine.VerifyInitialTruth(); apiErr != nil {
		t.Fatalf("truth mismatch: %v", apiErr)
	}
	view := engine.View()
	if view.Progress.Committed != 4 || view.Progress.Total != 4 {
		t.Fatalf("progress = %+v, want 4/4", view.Progress)
	}
	if got := strings.Join(view.CommitSequence, ","); got != "R1,R2,R3,R4" {
		t.Fatalf("commit sequence = %s", got)
	}
	if view.Scheduler.MaxParallel != 2 || view.Scheduler.BufferSampleCap != 16 {
		t.Fatalf("bad scheduler bounds: %+v", view.Scheduler)
	}
}

func TestSchemaGeometryConnectionAndStateErrors(t *testing.T) {
	sample := wb.GenerateSample(wb.DefaultSeed)

	missing := []byte(`{"seed":1}`)
	if _, err := wb.LoadSample(missing); err == nil || err.Code != wb.CodeSchema {
		t.Fatalf("missing schema err = %v", err)
	}

	cloned := wb.CloneSample(sample)
	cloned.Sources[0].X = cloned.Bounds.X2 + 1
	err := wb.ValidateSample(cloned)
	if err == nil || err.Code != wb.CodeSchema || err.Object != "S1" {
		t.Fatalf("source out of bounds err = %v", err)
	}

	cloned = wb.CloneSample(sample)
	cloned.Listeners[0].Y = cloned.Bounds.Y2 + 1
	err = wb.ValidateSample(cloned)
	if err == nil || err.Code != wb.CodeSchema || err.Object != "L1" {
		t.Fatalf("listener out of bounds err = %v", err)
	}

	cloned = wb.CloneSample(sample)
	for i := range cloned.Openings {
		if cloned.Openings[i].ID == "D1" {
			cloned.Openings[i].WallID = "W_B1"
		}
	}
	err = wb.ValidateSample(cloned)
	if err == nil || err.Code != wb.CodeGeom || err.Object != "D1" {
		t.Fatalf("non-adjacent door err = %v", err)
	}

	cloned = wb.CloneSample(sample)
	for i := range cloned.Rooms {
		if cloned.Rooms[i].ID == "R2" {
			cloned.Rooms[i].Rect.X1 = 10
		}
	}
	err = wb.ValidateSample(cloned)
	if err == nil || err.Code != wb.CodeGeom || !strings.Contains(err.Object, "R2") {
		t.Fatalf("overlap err = %v", err)
	}

	cloned = wb.CloneSample(sample)
	for i := range cloned.Openings {
		if cloned.Openings[i].ID == "D1" {
			cloned.Openings = append(cloned.Openings[:i], cloned.Openings[i+1:]...)
			break
		}
	}
	graph := wb.BuildGraph(cloned)
	_, connErr := wb.SolvePairPath(cloned, graph, "S1", "L2")
	if connErr == nil || connErr.Code != wb.CodeConn ||
		!strings.Contains(connErr.Object, "S1") || !strings.Contains(connErr.Object, "L2") {
		t.Fatalf("connection err = %v", connErr)
	}

	engine, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	stateErr := engine.TransitionRoom("R1", wb.StatusCommitted)
	if stateErr == nil || stateErr.Code != wb.CodeState {
		t.Fatalf("illegal transition err = %v", stateErr)
	}
	if got := engine.View().States[0].Status; got != wb.StatusPending {
		t.Fatalf("state changed after illegal transition: %s", got)
	}

	_, paraErr := engine.UpdatePrecision(context.Background(), wb.PrecisionSpec{Scale: wb.FixedScale, SampleCount: 1})
	if paraErr == nil || paraErr.Code != wb.CodePara || paraErr.Field != "precision.sampleCount" {
		t.Fatalf("precision err = %v", paraErr)
	}
}

func TestPreciseInvalidationAndIncrementalDiff(t *testing.T) {
	ctx := context.Background()
	engine, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, apiErr = engine.RunFull(ctx, true); apiErr != nil {
		t.Fatal(apiErr)
	}

	newParams := engine.View().Sample.Rooms[0].Params
	newParams.GainDB += 500
	report, apiErr := engine.UpdateRoomParams(ctx, wb.ParamUpdateRequest{RoomID: "R1", Params: newParams})
	if apiErr != nil {
		t.Fatalf("param update: %v", apiErr)
	}
	if !itemHasStatus(report.Items, "listener", "L1", "invalidated-and-resynthesized") {
		t.Fatalf("L1 was not precisely invalidated: %+v", report.Items)
	}
	if !itemReused(report.Items, "listener", "L2") || !itemReused(report.Items, "room", "R2") {
		t.Fatalf("unaffected dependency was not reused: %+v", report.Items)
	}
	diff, apiErr := engine.CompareWithFull(ctx)
	if apiErr != nil {
		t.Fatalf("param diff: %v report=%+v", apiErr, diff)
	}

	report, apiErr = engine.AddDoor(ctx, wb.AddDoorRequest{WallID: "W_V1", ID: "D_EXTRA", Center: 10, Width: 2})
	if apiErr != nil {
		t.Fatalf("add door: %v", apiErr)
	}
	if !itemReused(report.Items, "room", "R3") {
		t.Fatalf("R3 should reuse cache after door on R1/R2: %+v", report.Items)
	}
	if diff, apiErr = engine.CompareWithFull(ctx); apiErr != nil {
		t.Fatalf("add door diff: %v report=%+v", apiErr, diff)
	}

	if _, apiErr = engine.DeleteDoor(ctx, "D_EXTRA"); apiErr != nil {
		t.Fatalf("delete door: %v", apiErr)
	}
	if diff, apiErr = engine.CompareWithFull(ctx); apiErr != nil {
		t.Fatalf("delete door diff: %v report=%+v", apiErr, diff)
	}

	if _, apiErr = engine.DragBoundary(ctx, wb.BoundaryRequest{WallID: "W_V1", Value: 22}); apiErr != nil {
		t.Fatalf("drag boundary: %v", apiErr)
	}
	if diff, apiErr = engine.CompareWithFull(ctx); apiErr != nil {
		t.Fatalf("boundary diff: %v report=%+v", apiErr, diff)
	}
}

func TestSQLiteCheckpointRecoveryAndTamper(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data", "workbench.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), st)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, apiErr = engine.RunFull(ctx, false); apiErr != nil {
		t.Fatal(apiErr)
	}
	firstHash := engine.View().LastDiff
	_ = firstHash
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine2, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), st2)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if apiErr = engine2.Recover(ctx); apiErr != nil {
		t.Fatal(apiErr)
	}
	view := engine2.View()
	if view.Recovery.Recovered != 4 || view.Recovery.Discarded {
		t.Fatalf("recovery = %+v", view.Recovery)
	}
	if view.Progress.Committed != 4 {
		t.Fatalf("recovered progress = %+v", view.Progress)
	}
	if apiErr = engine2.VerifyInitialTruth(); apiErr != nil {
		t.Fatalf("recovered truth: %v", apiErr)
	}
	if _, apiErr = engine2.Resume(ctx); apiErr != nil {
		t.Fatalf("resume after complete recovery: %v", apiErr)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE room_checkpoints SET param_version=99 WHERE room_id='R1'`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	st3, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	engine3, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), st3)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if apiErr = engine3.Recover(ctx); apiErr != nil {
		t.Fatal(apiErr)
	}
	if !engine3.View().Recovery.Discarded {
		t.Fatalf("version tamper was not discarded: %+v", engine3.View().Recovery)
	}
	bundle, err := st3.LoadCheckpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Items) != 0 {
		t.Fatalf("tampered snapshot was not atomically voided: %d rows", len(bundle.Items))
	}
	if _, apiErr = engine3.Resume(ctx); apiErr != nil {
		t.Fatalf("full recompute after discard: %v", apiErr)
	}
}

func TestSQLitePartialCheckpointResume(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data", "partial.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), st)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, apiErr = engine.RunFull(ctx, false); apiErr != nil {
		t.Fatal(apiErr)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM room_checkpoints WHERE room_id='R4'`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	engine2, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), st2)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if apiErr = engine2.Recover(ctx); apiErr != nil {
		t.Fatal(apiErr)
	}
	if got := engine2.View().Recovery.Recovered; got != 3 {
		t.Fatalf("recovered %d, want 3", got)
	}
	if _, apiErr = engine2.Resume(ctx); apiErr != nil {
		t.Fatalf("partial resume: %v", apiErr)
	}
	if engine2.View().Progress.Committed != 4 {
		t.Fatalf("progress after resume: %+v", engine2.View().Progress)
	}
	if diff, apiErr := engine2.CompareWithFull(ctx); apiErr != nil {
		t.Fatalf("resumed checkpoint differs from full: %v %+v", apiErr, diff)
	}
}

func TestRoomFailureIsolationAndBufferBlocking(t *testing.T) {
	ctx := context.Background()
	engine, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, apiErr = engine.RunFull(ctx, true); apiErr != nil {
		t.Fatal(apiErr)
	}
	before := engine.View().Progress.Committed
	report, err := engine.InjectRoomFailure(ctx, "R2")
	if err == nil || err.Code != wb.CodeState || err.Object != "R2" {
		t.Fatalf("failure injection err = %v", err)
	}
	if report == nil {
		t.Fatal("missing failure report")
	}
	view := engine.View()
	if state := findState(view.States, "R2"); state.Status != wb.StatusFailed {
		t.Fatalf("R2 status = %s", state.Status)
	}
	if view.Progress.Committed != before-1 {
		t.Fatalf("committed after failure = %d, want %d", view.Progress.Committed, before-1)
	}
	if !itemReused(report.Items, "room", "R1") || !itemReused(report.Items, "listener", "L1") {
		t.Fatalf("other room should be untouched/reused: %+v", report.Items)
	}
	if _, apiErr = engine.Resume(ctx); apiErr != nil {
		t.Fatalf("retry after transient injection: %v", apiErr)
	}
	if engine.View().Progress.Committed != before {
		t.Fatalf("retry did not restore progress: %+v", engine.View().Progress)
	}

	scheduler := wb.NewScheduler(wb.PrecisionSpec{MaxParallel: 2, BufferSampleCap: 4})
	_, release1 := scheduler.AcquireSampleBuffer("test-1")
	_, release2 := scheduler.AcquireSampleBuffer("test-2")
	done := make(chan struct{})
	go func() {
		_, release3 := scheduler.AcquireSampleBuffer("test-blocked")
		release3()
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	release1()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked sample buffer was not released")
	}
	release2()
	diag := scheduler.Diagnostics()
	found := false
	for _, event := range diag.QueuedEvents {
		if event.Reason == "sample-buffer-exhausted" && event.TaskID == "test-blocked" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing buffer exhaustion queue event: %+v", diag.QueuedEvents)
	}
}

func itemReused(items []wb.InvalidationItem, kind, id string) bool {
	for _, item := range items {
		if item.Kind == kind && item.ID == id {
			return item.Reused
		}
	}
	return false
}

func itemHasStatus(items []wb.InvalidationItem, kind, id, status string) bool {
	for _, item := range items {
		if item.Kind == kind && item.ID == id {
			return item.Status == status
		}
	}
	return false
}

func findState(states []wb.RoomState, id string) wb.RoomState {
	for _, state := range states {
		if state.RoomID == id {
			return state
		}
	}
	return wb.RoomState{}
}

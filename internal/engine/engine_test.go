package engine

import (
	"context"
	"path/filepath"
	"testing"

	"roomreverb/internal/checkpoint"
	"roomreverb/internal/errs"
	"roomreverb/internal/generator"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
)

func fixture(t *testing.T) generator.Bundle {
	t.Helper()
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func config() Config {
	return Config{Solvers: solver.Config{MaxParallel: 3, PathBufferLimit: 64, GraphCacheBudget: 2}}
}

func TestCompareLocatesFirstDiff(t *testing.T) {
	b := fixture(t)
	full, err := RunFull(context.Background(), b.Level, config())
	if err != nil {
		t.Fatal(err)
	}
	changed := *full
	changed.Rooms = map[string]solver.RoomResult{}
	for id, room := range full.Rooms {
		changed.Rooms[id] = room.Clone()
	}
	room := changed.Rooms["r1"]
	p := room.Pairs[0]
	p.DecayFixed++
	room.Pairs[0] = p
	changed.Rooms["r1"] = room
	err = Compare(full, &changed)
	if !errs.IsCode(err, errs.EDiff) {
		t.Fatalf("want E_DIFF got %v", err)
	}
	typed := err.(*errs.Error)
	if typed.Room != "r1" || typed.Listener == "" || typed.Source == "" || typed.Field != "decay_fixed" {
		t.Fatalf("diff location incomplete: %+v", typed)
	}
}

func TestCheckpointInterruptAndResumeMatchesFull(t *testing.T) {
	b := fixture(t)
	path := filepath.Join(t.TempDir(), "resume.sqlite")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config()
	cfg.RunID = "resume-run"
	cfg.InterruptAfter = 3
	partial, err := RunWithCheckpoint(context.Background(), b.Level, store, cfg)
	if !errs.IsCode(err, errs.ECanceled) {
		t.Fatalf("want simulated cancellation got %v", err)
	}
	if partial.Progress.Committed != 3 {
		t.Fatalf("want 3 committed got %d", partial.Progress.Committed)
	}
	n, _ := store.CountSnapshots(context.Background(), cfg.RunID)
	if n != 3 {
		t.Fatalf("want 3 persisted snapshots got %d", n)
	}
	_ = store.Close()

	cfg.InterruptAfter = 0
	full, err := RunFull(context.Background(), b.Level, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.InterruptAfter = 0
	resumed, err := RunWithCheckpoint(context.Background(), b.Level, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := Compare(full, resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Progress.Committed != len(b.Level.Rooms) {
		t.Fatalf("resume incomplete: %+v", resumed.Progress)
	}
}

func TestPreviewRedundantPortalReusesUnaffectedRooms(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	s, err := NewPreview(ctx, b.Level, nil, config())
	if err != nil {
		t.Fatal(err)
	}
	old, _ := b.Level.Opening("o45")
	edit := model.Edit{Kind: model.EditAddPortal, Portal: &model.Opening{
		ID: "o99", WallID: "w45", Center: old.Center, Width: old.Width, Active: true,
	}}
	report, err := s.Apply(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]InvalidationRecord{}
	for _, rec := range report.Invalidations {
		records[rec.RoomID] = rec
	}
	for _, id := range []string{"r0", "r1", "r2", "r3"} {
		if !records[id].Reused || records[id].Invalidated {
			t.Fatalf("room %s should be reused: %+v", id, records[id])
		}
	}
	for _, id := range []string{"r4", "r5"} {
		if !records[id].Invalidated {
			t.Fatalf("room %s should be invalidated: %+v", id, records[id])
		}
	}
}

func TestPreviewWritesPersistentCacheForAllRooms(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config()
	cfg.UseCache = true
	s, err := NewPreview(ctx, b.Level, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := b.Level.Rooms[1].Params
	p.T60MS = 640
	if _, err := s.Apply(ctx, model.Edit{Kind: model.EditSetRoomParams, RoomID: "r1", Params: &p}); err != nil {
		t.Fatal(err)
	}
	current := s.Level()
	for _, id := range SortedRoomIDs(current) {
		_, ok, err := store.LoadCache(ctx, id, checkpoint.RoomInputHash(current, id), s.paramsVersion)
		if err != nil || !ok {
			t.Fatalf("room %s cache missing: ok=%v err=%v", id, ok, err)
		}
	}
}

func TestDisconnectedRunReturnsEConnAndFailsRoom(t *testing.T) {
	b := fixture(t)
	level := b.Level.Clone()
	for i := range level.Openings {
		level.Openings[i].Active = false
	}
	run, err := RunFull(context.Background(), level, config())
	if !errs.IsCode(err, errs.EConn) {
		t.Fatalf("want E_CONN got %v", err)
	}
	typed := err.(*errs.Error)
	if typed.Source == "" || typed.Listener == "" {
		t.Fatalf("E_CONN missing source/listener: %+v", typed)
	}
	if run.Progress.Committed != 0 {
		t.Fatalf("no room should commit when a source is unreachable, got %+v", run.Progress)
	}
	failedFound := false
	for _, st := range run.States {
		if st.State == "failed" {
			failedFound = true
		}
	}
	if !failedFound {
		t.Fatal("disconnected room did not enter failed state")
	}
}

func TestPreviewRetryFailedRoomAfterConnectivityRepair(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	s, err := NewPreview(ctx, b.Level, nil, config())
	if err != nil {
		t.Fatal(err)
	}
	for _, portal := range []string{"o03", "o34"} {
		if _, err := s.Apply(ctx, model.Edit{Kind: model.EditRemovePortal, PortalID: portal}); err != nil {
			// The second removal isolates r3 and is expected to report E_CONN.
			if !errs.IsCode(err, errs.EConn) {
				t.Fatalf("remove %s: %v", portal, err)
			}
		}
	}
	state, _ := s.tracker.Get("r3")
	if state.State != "failed" {
		t.Fatalf("r3 should be failed after disconnection, got %s", state.State)
	}
	if err := s.Retry(ctx, "r3"); !errs.IsCode(err, errs.EConn) {
		t.Fatalf("retry while disconnected should return E_CONN, got %v", err)
	}
	state, _ = s.tracker.Get("r3")
	if state.State != "failed" {
		t.Fatalf("failed retry changed state to %s", state.State)
	}
	portal := model.Opening{ID: "o03b", WallID: "w03", Center: model.Point{X: 1450, Y: 2000}, Width: 300, Active: true}
	if _, err := s.Apply(ctx, model.Edit{Kind: model.EditAddPortal, Portal: &portal}); err != nil {
		t.Fatal(err)
	}
	state, _ = s.tracker.Get("r3")
	if state.State != "committed" {
		t.Fatalf("r3 should commit after repair, got %s", state.State)
	}
}

package lightprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDeterministicSceneAndTrajectory(t *testing.T) {
	a, err := GenerateScene(DefaultSeed, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateScene(DefaultSeed, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("same seed produced different scene/trajectory")
	}
	if a.Queries[10].Position != b.Queries[10].Position {
		t.Fatal("camera trajectory is not reproducible")
	}
}

func TestAccumulatedUpperTightensWithFixedScale(t *testing.T) {
	scales := []int64{100, 1_000, 10_000, 100_000, 1_000_000}
	previous := 1e9
	for _, scale := range scales {
		params := DefaultParams()
		params.FixedScale = scale
		params.WeightScale = scale
		params.BlendScale = scale
		params.SHMinFixed = -4 * scale
		params.SHMaxFixed = 4 * scale
		scene, err := GenerateScene(DefaultSeed, params)
		if err != nil {
			t.Fatal(err)
		}
		pr, err := RunPipeline(scene, RunOptions{Pool: PoolConfig{Workers: 2, BufferLimit: 4, QueueBudget: 4}})
		if err != nil {
			t.Fatal(err)
		}
		_, upper, violations := CheckErrorBounds(pr)
		if len(violations) != 0 {
			t.Fatalf("scale %d upper-bound violations: %v", scale, violations)
		}
		if upper > previous+1e-15 {
			t.Fatalf("upper did not tighten: scale=%d upper=%.9f previous=%.9f", scale, upper, previous)
		}
		previous = upper
	}
}

func TestCancelLeavesOnlyCommittedRoom(t *testing.T) {
	scene := testScene(t)
	path := filepath.Join(t.TempDir(), "cancel.lpjson")
	pr, err := RunPipelineContext(context.Background(), scene, RunOptions{
		CheckpointPath: path, CancelAfterCommits: 1,
		Pool: PoolConfig{Workers: 1, BufferLimit: 1, QueueBudget: 1},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(pr.CommitOrder) != 1 || pr.CommitOrder[0] != "room-a" {
		t.Fatalf("cancel boundary committed order %v", pr.CommitOrder)
	}
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Snapshot().Order) != 1 {
		t.Fatalf("cancellation must not leave interleaved room writes")
	}
	resume, err := RunPipeline(scene, RunOptions{CheckpointPath: path, Pool: PoolConfig{Workers: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.RoomErrors) != 0 || len(resume.RestoredRooms) != 1 {
		t.Fatalf("resume after cancel failed: errors=%v restored=%v", resume.RoomErrors, resume.RestoredRooms)
	}
}

func TestTamperedCheckpointForcesWholeRoomRebuild(t *testing.T) {
	scene := testScene(t)
	path := filepath.Join(t.TempDir(), "tampered.lpjson")
	first, err := RunPipeline(scene, RunOptions{CheckpointPath: path, Pool: PoolConfig{Workers: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.CommitOrder) != 3 {
		t.Fatalf("first run did not commit all rooms: %v", first.CommitOrder)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap CheckpointSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	rec := snap.Rooms["room-a"]
	rec.ContentHash += "-tampered"
	snap.Rooms["room-a"] = rec
	data, _ = json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := RunPipeline(scene, RunOptions{CheckpointPath: path, Pool: PoolConfig{Workers: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.StaleRooms) != 1 || second.StaleRooms[0].RoomID != "room-a" || second.StaleRooms[0].Reason != "content-hash-mismatch" {
		t.Fatalf("expected room-a hash stale, got %+v", second.StaleRooms)
	}
	if len(second.RestoredRooms) != 2 {
		t.Fatalf("expected b/c restored, got %v", second.RestoredRooms)
	}
	if len(second.QueryResults) == 0 || len(second.RoomErrors) != 0 {
		t.Fatalf("tampered room should rebuild and pipeline complete: errors=%v queries=%d", second.RoomErrors, len(second.QueryResults))
	}
	if diff := DiffPipelines(first, second); diff != nil {
		t.Fatalf("rebuilt checkpoint result differs: %v", diff)
	}
}

func TestWorkbenchHTTPHandlers(t *testing.T) {
	wb, err := NewWorkbench(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := WorkbenchHandler(wb)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !containsString([]string{rec.Body.String()}, rec.Body.String()) {
		t.Fatalf("index status=%d", rec.Code)
	}
	if body := rec.Body.String(); len(body) < 100 || body[:15] != "<!doctype html>" {
		t.Fatalf("unexpected index body prefix: %q", body[:min(40, len(body))])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state status=%d", rec.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["acceptance"] == nil || decoded["invalidations"] == nil {
		t.Fatalf("state missing workbench sections")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

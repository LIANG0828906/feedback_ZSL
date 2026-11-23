package spatialpick

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSnapshotRestoresExactState(t *testing.T) {
	scene, _, engine := mustSample(t)
	for i := 0; i < 2; i++ {
		if _, err := engine.ApplyPivotBatch(scene.EditSequence[i]); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "state.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	restored, status, err := OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Loaded || status.Rebuilt {
		t.Fatalf("status = %+v", status)
	}
	if restored.Revision() != engine.Revision() || restored.IndexVersion() != engine.IndexVersion() {
		t.Fatalf("restored versions: revision=%d/%d index=%d/%d", restored.Revision(), engine.Revision(), restored.IndexVersion(), engine.IndexVersion())
	}
	if !reflect.DeepEqual(restored.AppliedBatches(), engine.AppliedBatches()) {
		t.Fatalf("batches = %v/%v", restored.AppliedBatches(), engine.AppliedBatches())
	}
	if restored.ContentHash() != engine.ContentHash() {
		t.Fatal("content hash mismatch after restore")
	}
	left, right := engine.Bundle(), restored.Bundle()
	if err := CompareBundles(&left, &right); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptSnapshotInvalidatesWholeSnapshot(t *testing.T) {
	scene, _, engine := mustSample(t)
	path := filepath.Join(t.TempDir(), "state.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	env["checksum"] = strings.Repeat("0", 64)
	data, _ = json.Marshal(env)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	restored, status, err := OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Loaded || !status.Rebuilt || !strings.Contains(status.Reason, "checksum") {
		t.Fatalf("status = %+v", status)
	}
	if restored.Revision() != 0 {
		t.Fatalf("fallback revision = %d", restored.Revision())
	}
}

func TestSnapshotVersionAndContentHashMismatchRebuilds(t *testing.T) {
	scene, _, engine := mustSample(t)
	path := filepath.Join(t.TempDir(), "state.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	otherScene := *scene
	otherScene.SceneVersion = "different-structure-version"
	hash, err := ComputeSceneHash(&otherScene)
	if err != nil {
		t.Fatal(err)
	}
	otherScene.ContentHash = hash
	otherEngine, err := NewEngine(&otherScene)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherEngine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	_, status, err := OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Loaded || !status.Rebuilt {
		t.Fatalf("version mismatch status = %+v", status)
	}

	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env snapshotEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	var payload snapshotPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.ContentHash = strings.Repeat("f", 64)
	payloadBytes, _ := json.Marshal(payload)
	sum := sha256.Sum256(payloadBytes)
	env.Checksum = hex.EncodeToString(sum[:])
	env.Payload = payloadBytes
	data, _ = json.MarshalIndent(env, "", "  ")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_, status, err = OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Loaded || !status.Rebuilt || !strings.Contains(status.Reason, "content hash") {
		t.Fatalf("content mismatch status = %+v", status)
	}
}

func TestSnapshotCancelAndConcurrentWriters(t *testing.T) {
	scene, _, engine := mustSample(t)
	path := filepath.Join(t.TempDir(), "state.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.SaveSnapshot(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save error = %v", err)
	}
	if _, status, err := OpenEngineWithSnapshot(context.Background(), scene, path); err != nil || !status.Loaded {
		t.Fatalf("canceled write damaged snapshot: status=%+v err=%v", status, err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".snapshot-*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temporary snapshot files remain: %v", matches)
	}

	const writers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- engine.SaveSnapshot(context.Background(), path)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, status, err := OpenEngineWithSnapshot(context.Background(), scene, path); err != nil || !status.Loaded {
		t.Fatalf("concurrent writes left unreadable snapshot: status=%+v err=%v", status, err)
	}
}

func TestMissingSnapshotFallsBackToFullBuild(t *testing.T) {
	scene, _, _ := mustSample(t)
	path := filepath.Join(t.TempDir(), "absent.snapshot")
	engine, status, err := OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Rebuilt || status.Reason != "snapshot_missing" || engine.Revision() != 0 {
		t.Fatalf("status = %+v revision=%d", status, engine.Revision())
	}
}

func TestSnapshotPreservesFailedEntryForRetry(t *testing.T) {
	scene, _, engine := mustSample(t)
	if _, err := engine.RebuildSubtree(context.Background(), []string{"n012"}, RebuildOptions{
		Fault: func(id string) error {
			if id == "n012" {
				return context.DeadlineExceeded
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "failed.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	restored, status, err := OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil || !status.Loaded {
		t.Fatalf("restore status=%+v err=%v", status, err)
	}
	if _, err := restored.GetDerived("n012", 0); !IsCode(err, CodeStale) {
		t.Fatalf("failed entry not preserved: %v", err)
	}
	if _, err := restored.RebuildSubtree(context.Background(), []string{"n012"}, RebuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.GetDerived("n012", 0); err != nil {
		t.Fatalf("retry after restore failed: %v", err)
	}
}

func TestSnapshotEnvelopeUsesCompleteSingleFile(t *testing.T) {
	_, _, engine := mustSample(t)
	path := filepath.Join(t.TempDir(), "state.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env snapshotEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if _, err := base64.StdEncoding.DecodeString(base64.StdEncoding.EncodeToString(env.Payload)); err != nil {
		t.Fatal(err)
	}
	if len(env.Payload) == 0 || env.Checksum == "" {
		t.Fatal("snapshot envelope lacks payload/checksum")
	}
}

func TestSnapshotRestoresRuntimeGeometryEdit(t *testing.T) {
	scene, _, engine := mustSample(t)
	g := &Geometry{ID: "g-snapshot-added", Kind: GeometrySphere, Center: V3(0, 0, 0), Radius: 0.2}
	if _, err := engine.ApplyGeometryMutations([]GeometryMutation{{NodeID: "n001", Geometry: g}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "geometry.snapshot")
	if err := engine.SaveSnapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	restored, status, err := OpenEngineWithSnapshot(context.Background(), scene, path)
	if err != nil || !status.Loaded {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	d, err := restored.GetDerived("n001", 0)
	if err != nil || d.GeometryID != g.ID {
		t.Fatalf("geometry not restored: d=%+v err=%v", d, err)
	}
	if _, ok := restored.Index().FindEntry("n001"); !ok {
		t.Fatal("restored index missing runtime geometry")
	}
}

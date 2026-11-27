package grid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVerificationWorkflowThreePathsAndInjections(t *testing.T) {
	dir := t.TempDir()
	report, err := RunVerification(dir)
	if err != nil {
		t.Fatalf("RunVerification: %v", err)
	}
	if !report.OK || !report.ThreePaths.RestartEqualsFull || !report.ThreePaths.IncrementalEqualsFull {
		t.Fatalf("consistency failed: %#v", report.ThreePaths)
	}
	if report.TileCount != 8 || report.RestartRecovered != 3 {
		t.Fatalf("tile/recovery counts = %d/%d", report.TileCount, report.RestartRecovered)
	}
	if report.IncrementalReused == 0 || report.IncrementalRecomputed == 0 {
		t.Fatalf("incremental reuse=%d recompute=%d", report.IncrementalReused, report.IncrementalRecomputed)
	}
	for name, code := range map[string]ErrorCode{
		"decode": CodeDecode, "duplicate": CodeDecode, "order": CodeOrder,
		"norm": CodeNorm, "block": CodeBlock, "bounce": CodeBounce,
	} {
		ge := report.Injections[name]
		if ge == nil || ge.Code != code {
			t.Fatalf("injection %s = %#v, want %s", name, ge, code)
		}
	}
	for _, file := range []string{"schema_v1.json", "schema_v2.json", "schema_v3.json", "summary.json", "restart.json", "tiles.db"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Fatalf("expected artifact %s: %v", file, err)
		}
	}
}

func TestCoverageCeilingReturnsECOVER(t *testing.T) {
	scene, err := ProjectScene(Generate(DefaultSeed), DefaultCamera())
	if err != nil {
		t.Fatal(err)
	}
	err = scene.ValidateCoverage(1)
	ge, ok := AsError(err, CodeCover)
	if !ok || ge.Tile == nil || ge.Measured == "" {
		t.Fatalf("coverage error = %#v", err)
	}
}

func TestSnapshotRollbackAndStaleHashInvalidation(t *testing.T) {
	ctx := t.TempDir()
	dbPath := filepath.Join(ctx, "tiles.db")
	level := Generate(DefaultSeed)
	cam := DefaultCamera()
	versions := DefaultSemanticVersions()
	scene, err := ProjectScene(level, cam)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenSnapshot(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTile(scene.Tiles[0], cam, versions, scene.HitStats); err != nil {
		t.Fatal(err)
	}

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statsJSON, _ := json.Marshal(scene.HitStats)
	versionsJSON, _ := json.Marshal(versions)
	second := scene.Tiles[1]
	_, err = tx.Exec(upsertTileSQL,
		int64(second.ID.Col), int64(second.ID.Row), int64(second.Seq), second.ContentHash,
		second.ColorDigest, second.DepthDigest, second.Color, DepthBytes(second.Depth),
		int64(versions.Structure), int64(cam.CameraVersion), AlgorithmVersion, string(versionsJSON),
		int64(second.CoveragePermille), int64(second.PolyCoverage), string(statsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := OpenSnapshot(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("committed rows after rollback = %d, want 1", len(rows))
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var dbFile map[string]any
	if err := json.Unmarshal(raw, &dbFile); err != nil {
		t.Fatal(err)
	}
	tables := dbFile["tables"].(map[string]any)
	tilesTable := tables["tiles"].(map[string]any)
	row := tilesTable["0,0"].(map[string]any)
	row["content_hash"].(map[string]any)["s"] = "stale-hash"
	newRaw, _ := json.Marshal(dbFile)
	if err := os.WriteFile(dbPath, newRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	store3, err := OpenSnapshot(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()
	cached, err := store3.LoadTile(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fresh := scene.Tiles[0]
	ok, field := cached.Recoverable(fresh, versions, cam.CameraVersion)
	if ok || field != "contentHash" {
		t.Fatalf("recoverable stale tile = %v field=%s", ok, field)
	}
	rerun, err := ProjectScene(level, cam)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunWithCache(store3, rerun, cam, versions, nil); err != nil {
		t.Fatal(err)
	}
	if diff := CompareScenes(scene, rerun); diff != nil {
		t.Fatalf("recomputed stale path differs: %#v", diff)
	}
}

func TestCacheKeyIncludesRequiredComponents(t *testing.T) {
	v := DefaultSemanticVersions()
	k1 := CacheKeyForTile("hash-a", v, 1)
	v.Structure++
	k2 := CacheKeyForTile("hash-a", v, 1)
	if k1 == k2 {
		t.Fatal("structure version must change cache key")
	}
	v = DefaultSemanticVersions()
	k3 := CacheKeyForTile("hash-b", v, 1)
	if k1 == k3 {
		t.Fatal("content hash must change cache key")
	}
	k4 := CacheKeyForTile("hash-a", v, 2)
	if k1 == k4 {
		t.Fatal("camera version must change cache key")
	}
}

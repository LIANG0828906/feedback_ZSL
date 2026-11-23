package checkpoint_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/stroke"
)

func testMeta() checkpoint.Meta {
	return checkpoint.Meta{
		SchemaVersion:    checkpoint.SchemaVersion,
		GlyphID:          "test-glyph",
		GlyphHash:        "glyph-hash",
		ParamVersion:     "param-v1",
		AlgorithmVersion: "algo-v1",
		SegmentCount:     2,
	}
}

func testRecord(id, ordinal int) checkpoint.Record {
	poly := []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
	return checkpoint.Record{
		SegmentID:    id,
		Ordinal:      ordinal,
		ContourIndex: 0,
		CurveIndex:   id - 1,
		Band:         stroke.Band{Polygon: poly, Bounds: geom.BoundsOf(poly), Area: 100},
		Mask: raster.Mask{
			Width:  2,
			Height: 2,
			Data:   []byte{1, 0, 0, 1},
		},
	}
}

func TestCommitAllLoadAndVersionDiscard(t *testing.T) {
	root := t.TempDir()
	store := checkpoint.New(root)
	meta := testMeta()
	records := []checkpoint.Record{testRecord(1, 0), testRecord(2, 1)}
	if err := store.CommitAll(meta, records); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0].SegmentID != 1 || loaded[1].SegmentID != 2 {
		t.Fatalf("unexpected load order: %+v", loaded)
	}
	if loaded[0].AlgorithmVersion != meta.AlgorithmVersion || loaded[0].ParamVersion != meta.ParamVersion ||
		loaded[0].GlyphHash != meta.GlyphHash || loaded[0].RecordHash == "" {
		t.Fatalf("version/hash fields missing: %+v", loaded[0])
	}
	manifestPath := filepath.Join(root, meta.GlyphID, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["algorithmVersion"] = "different"
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Load(meta)
	if !errors.Is(err, checkpoint.ErrVersionMismatch) {
		t.Fatalf("expected version mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, meta.GlyphID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot not discarded: %v", err)
	}
}

func TestPartialSnapshotIsRejectedWhole(t *testing.T) {
	store := checkpoint.New(t.TempDir())
	meta := testMeta()
	if err := store.Reset(meta); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(meta, testRecord(1, 0), 0); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(meta)
	if !errors.Is(err, checkpoint.ErrVersionMismatch) {
		t.Fatalf("expected incomplete snapshot mismatch, got %v", err)
	}
}

func TestNoSnapshot(t *testing.T) {
	store := checkpoint.New(t.TempDir())
	_, err := store.Load(testMeta())
	if !errors.Is(err, checkpoint.ErrNoSnapshot) {
		t.Fatalf("expected ErrNoSnapshot, got %v", err)
	}
}

func TestConcurrentStoreInstancesDoNotInterleave(t *testing.T) {
	root := t.TempDir()
	a := checkpoint.New(root)
	b := checkpoint.New(root)
	meta := testMeta()
	recordsA := []checkpoint.Record{testRecord(1, 0), testRecord(2, 1)}
	recordsB := []checkpoint.Record{testRecord(1, 0), testRecord(2, 1)}
	for i := range recordsA {
		recordsA[i].Mask.Data = []byte{1, 0, 0, 1}
		recordsB[i].Mask.Data = []byte{2, 2, 2, 2}
	}
	var wg sync.WaitGroup
	errsCh := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errsCh <- a.CommitAll(meta, recordsA) }()
	go func() { defer wg.Done(); errsCh <- b.CommitAll(meta, recordsB) }()
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := checkpoint.New(root).Load(meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d records", len(loaded))
	}
	first := loaded[0].Mask.Data[0]
	for _, rec := range loaded {
		if rec.Mask.Data[0] != first {
			t.Fatalf("snapshot mixed concurrent commits: %d vs %d", rec.Mask.Data[0], first)
		}
	}
}

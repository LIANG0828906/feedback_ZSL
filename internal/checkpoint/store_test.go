package checkpoint

import (
	"path/filepath"
	"testing"

	"strokeforge/internal/checkpoint/pb"
	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

func TestSaveLoadAndTamper(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cp")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := pb.Manifest{
		Algorithm:    model.AlgorithmVersion,
		GlyphHashes:  map[string]string{"G": "h"},
		ParamVersion: "p",
		Seed:         1,
	}
	if err := store.WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}
	frame := raster.Frame{Width: 4, Height: 4}
	snap := pb.SegmentSnapshot{
		Algorithm: model.AlgorithmVersion, ContentHash: "h", ContourHash: "ch",
		ParamVersion: "p", GlyphID: "G", SegmentID: 0, ContourID: "c",
		CacheKey: "k", MaskWidth: 4, MaskHeight: 4,
		MaskPix: []byte{1, 2, 3, 4}, Densities: []int{10, 20},
	}
	if err := store.SaveRecord(snap); err != nil {
		t.Fatal(err)
	}
	frames := map[string]raster.Frame{"G": frame}
	records, report, err := store.Load(manifest, frames)
	if err != nil {
		t.Fatal(err)
	}
	if report.Invalidated || len(records) != 1 {
		t.Fatalf("unexpected report %+v", report)
	}
	// Tamper must invalidate the whole store (no partial mixing).
	if err := store.Tamper("G", 0); err != nil {
		t.Fatal(err)
	}
	_, report, err = store.Load(manifest, frames)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Invalidated || !report.FullRebuild {
		t.Fatalf("expected full invalidation, got %+v", report)
	}
	// After invalidation the directory was wiped, so another load also fails
	// closed (manifest missing) and remains safe rather than mixing state.
	_, report2, err := store.Load(manifest, frames)
	if err != nil {
		t.Fatal(err)
	}
	if !report2.FullRebuild {
		t.Fatalf("expected rebuild flag, got %+v", report2)
	}
}

func TestVersionMismatchInvalidates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cp")
	store, _ := Open(dir)
	manifest := pb.Manifest{Algorithm: "stroke-forge/1.0.0", GlyphHashes: map[string]string{"G": "h"}, ParamVersion: "p"}
	_ = store.WriteManifest(manifest)
	_ = store.SaveRecord(pb.SegmentSnapshot{
		Algorithm: "stroke-forge/1.0.0", ContentHash: "h", ContourHash: "ch",
		ParamVersion: "p", GlyphID: "G", SegmentID: 1,
		MaskWidth: 2, MaskHeight: 2, MaskPix: []byte{0, 0, 0, 0},
	})
	wrong := manifest
	wrong.ParamVersion = "different"
	_, report, err := store.Load(wrong, map[string]raster.Frame{"G": {Width: 2, Height: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Invalidated {
		t.Fatalf("expected invalidation, got %+v", report)
	}
}

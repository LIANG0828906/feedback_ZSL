package storage_test

import (
	"path/filepath"
	"testing"

	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/storage"
)

func TestWindowCommitAndPairLoad(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	meta := storage.Meta{
		ProjectHash: "h1", AlgoVersion: "algo-1", GeoScaleBits: 4,
		ParamVersions: "{}", GeomVersions: "{}",
	}
	if err := store.SaveMeta(meta); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LoadMeta()
	if err != nil || !found || got.ProjectHash != "h1" {
		t.Fatalf("meta roundtrip failed: %v found=%v", err, found)
	}

	results := []model.Result{
		{SourceID: "S1", ListenerID: "L1", SPL: 1.5, Curve: []float64{1, 2}},
		{SourceID: "S2", ListenerID: "L1", SPL: 2.5, Curve: []float64{3, 4}},
	}
	rec := storage.WindowRecord{
		WindowID: 0, PairKeys: []string{"S1->L1", "S2->L1"},
		InputHash: "in1", Summary: "{}", ProfileStat: "{}",
	}
	if err := store.CommitWindow(rec, results); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := store.LoadPair("S1->L1", "in1")
	if err != nil || !hit {
		t.Fatalf("pair load failed: %v hit=%v", err, hit)
	}
	if loaded.SPL != 1.5 || !loaded.CacheHit {
		t.Fatal("loaded payload mismatch or cache-hit flag not set")
	}
	if _, hit, _ := store.LoadPair("S1->L1", "stale-hash"); hit {
		t.Fatal("stale input hash must not hit cache")
	}

	windows, _ := store.LoadedWindows()
	if len(windows) != 1 || windows[0] != 0 {
		t.Fatalf("windows mismatch: %v", windows)
	}

	// Reset 后元数据与窗口整体作废。
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.LoadMeta(); found {
		t.Fatal("meta must be cleared after reset")
	}
	if w, _ := store.LoadedWindows(); len(w) != 0 {
		t.Fatal("windows must be cleared after reset")
	}
	if _, hit, _ := store.LoadPair("S1->L1", "in1"); hit {
		t.Fatal("pair cache must be cleared after reset")
	}
}

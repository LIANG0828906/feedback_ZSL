package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

func testSegment(i int) model.SegmentResult {
	return model.SegmentResult{
		Index: i, StartFrame: i * 10, EndFrame: i*10 + 9,
		Frames: []model.FrameResult{{Frame: i * 10, Summary: "sum", ContentHash: "content", Verdict: "intact", ResidualEnergy: 0.1}},
	}
}

func TestCommitLoadSequentialAndAtomic(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	cfg := model.Config{BlockSize: 8, History: 1, MaxWorkers: 1}
	if err := store.Commit("content", "params", "algo", cfg, testSegment(0)); err != nil {
		t.Fatal(err)
	}
	if e := apperr.As(store.Commit("content", "params", "algo", cfg, testSegment(2))); e == nil || e.Code != apperr.State {
		t.Fatalf("out-of-order commit = %#v", e)
	}
	if err := store.Commit("content", "params", "algo", cfg, testSegment(1)); err != nil {
		t.Fatal(err)
	}
	segments, inv, err := store.Load("content", "params", "algo")
	if err != nil || inv != nil {
		t.Fatalf("load err=%v inv=%v", err, inv)
	}
	if len(segments) != 2 || segments[0].Index != 0 || segments[1].Index != 1 {
		t.Fatalf("segments = %+v", segments)
	}
	temps, _ := filepath.Glob(filepath.Join(dir, "segments", "*.tmp"))
	if len(temps) != 0 {
		t.Fatalf("temporary files left behind: %v", temps)
	}
}

func TestVersionMismatchArchivesWholeSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	cfg := model.Config{BlockSize: 8, History: 1, MaxWorkers: 1}
	if err := store.Commit("content", "params", "algo", cfg, testSegment(0)); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit("content", "params", "algo", cfg, testSegment(1)); err != nil {
		t.Fatal(err)
	}
	segments, inv, err := store.Load("content", "params-v2", "algo")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 || inv == nil {
		t.Fatalf("segments=%d inv=%#v", len(segments), inv)
	}
	if len(inv.Fields) != 1 || inv.Fields[0] != "param_version" {
		t.Fatalf("fields = %v", inv.Fields)
	}
	if _, err := os.Stat(filepath.Join(dir, "segments")); !os.IsNotExist(err) {
		t.Fatal("invalid snapshot was not archived as a whole")
	}
	archives, _ := filepath.Glob(filepath.Join(dir, "invalid", "snapshot-*"))
	if len(archives) != 1 {
		t.Fatalf("archives = %v", archives)
	}
}

func TestContentAndAlgorithmMismatchReportBothIdentifiers(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	cfg := model.Config{BlockSize: 8, History: 1, MaxWorkers: 1}
	if err := store.Commit("content", "params", "algo", cfg, testSegment(0)); err != nil {
		t.Fatal(err)
	}
	_, inv, err := store.Load("content-v2", "params-v2", "algo-v2")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Fields) != 3 {
		t.Fatalf("fields = %v", inv.Fields)
	}
	for _, field := range []string{"content_hash", "param_version", "algorithm_version"} {
		if inv.Expected[field] == "" || inv.Actual[field] == "" {
			t.Fatalf("missing expected/actual for %s: %+v", field, inv)
		}
	}
}

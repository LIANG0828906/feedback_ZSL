package engine

import (
	"context"
	"path/filepath"
	"testing"

	"ms-workbench/internal/samples"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/store"
)

func TestRetryFailedSegmentPreservesCommittedNeighbours(t *testing.T) {
	pkg, err := samples.Generate(samples.Options{Seed: 77, Width: 64, Height: 48}, "retry", "Retry")
	if err != nil {
		t.Fatal(err)
	}
	s, err := scene.Decode(pkg.Files[3])
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	cfg := DefaultConfig()
	e, err := New(s, cp, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), RunOptions{Mode: ModeFull}); err != nil {
		t.Fatal(err)
	}
	target := 5
	e.mu.Lock()
	rgbBefore := append([]float64(nil), e.segments[target-1].Result.Chroma.LinearRGB...)
	e.segments[target].State = Failed
	e.segments[target].Result = nil
	e.segments[target].FusionDirty = true
	e.segments[target].ChromaDirty = true
	e.segments[target].LastError = "injected local failure"
	e.mu.Unlock()
	report, err := e.RetrySegment(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if report.NewCommits != 1 {
		t.Fatalf("retry commits = %d", report.NewCommits)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.segments[target].State != Committed {
		t.Fatalf("retried segment state = %s", e.segments[target].State)
	}
	if !sameFloatSlice(rgbBefore, e.segments[target-1].Result.Chroma.LinearRGB) {
		t.Fatal("retry polluted a committed neighbour")
	}
}

func sameFloatSlice(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

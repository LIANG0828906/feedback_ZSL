package pipeline

import (
	"testing"
)

func TestRunVerification(t *testing.T) {
	v, err := RunVerification(t.TempDir())
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	if len(v.Full.Blocks) == 0 {
		t.Fatal("no blocks")
	}
	if err := CompareReports(v.Full, v.Recovery); err != nil {
		t.Fatalf("recovery mismatch: %v", err)
	}
	if err := CompareReports(v.Full, v.IncrementalFinal); err != nil {
		t.Fatalf("incremental mismatch: %v", err)
	}
	reusedUnaffected := false
	for _, b := range v.IncrementalGain.Blocks {
		if b.Block.String() == "3,2" && b.Source == "persistent-block-cache-hit" {
			reusedUnaffected = true
		}
	}
	if !reusedUnaffected {
		t.Fatalf("unaffected block 3,2 was not reused from persistent store")
	}
	lutHits := 0
	for _, b := range v.Dense.Blocks {
		for _, br := range b.Quant.Bands {
			lutHits += br.LUTHits
			if b.Quant.Decision.Method != "dense_block_lut" {
				t.Fatalf("unexpected dense method %s", b.Quant.Decision.Method)
			}
		}
	}
	if lutHits == 0 {
		t.Fatal("expected dense path to exercise bounded LUT hits")
	}
	if bc := v.Medium.Blocks[0].Quant.BoundaryCheck; bc == nil || !bc.Match {
		t.Fatalf("medium boundary recomputation mismatch: %#v", bc)
	}
}

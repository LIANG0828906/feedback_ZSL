package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
	"qualitybench/internal/store"
)

func TestConcurrentCommitsAreSerializedWithStableSequence(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	meta := checkpoint.RunMeta{
		RunID: "serial", ImageID: "img", AlgorithmVersion: "v1", ChainFingerprint: "chain",
		ProjectFingerprint: "project", TruthFingerprint: "truth", Width: 80, Height: 64,
		Channels: 3, BlockSize: 16, StepCount: 1, Status: "running",
	}
	if err := st.EnsureRun(ctx, meta, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			block := core.Rect{X: (i % 5) * 16, Y: (i / 5) * 16, W: 16, H: 16}
			pixels := make([]byte, 16*16*3)
			for j := range pixels {
				pixels[j] = byte(i)
			}
			a := checkpoint.Artifact{
				RunID: "serial", ImageID: "img", Step: 1, Block: block,
				State: core.StateCommitted, CacheKey: fmt.Sprintf("key-%d", i),
				InputHash: checkpoint.PixelHash(pixels), OutputHash: checkpoint.PixelHash(pixels),
				ChainFingerprint: "chain", AlgorithmVersion: "v1", Pixels: pixels,
			}
			a.OutputDigest = a.ComputeOutputDigest()
			_, err := st.CommitArtifact(ctx, a)
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.LoadArtifacts(ctx, "serial", "img")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 20 {
		t.Fatalf("expected 20 unique blocks, got %d", len(rows))
	}
	seen := make(map[int64]bool)
	for _, row := range rows {
		if seen[row.CommitSeq] {
			t.Fatalf("duplicate commit sequence %d", row.CommitSeq)
		}
		seen[row.CommitSeq] = true
	}
	for i := int64(1); i <= 20; i++ {
		if !seen[i] {
			t.Fatalf("missing commit sequence %d", i)
		}
	}
}

package hashx

import (
	"encoding/hex"
	"sync"
	"testing"

	"gameintegrity/internal/model"
)

func testPixels(w, h int) []byte {
	out := make([]byte, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			off := (y*w + x) * 3
			out[off] = byte(x*13 + y*7)
			out[off+1] = byte(x*3 + y*11)
			out[off+2] = byte(x*5 + y*2)
		}
	}
	return out
}

func TestDigestFramePureAndThreadIndependent(t *testing.T) {
	pixels := testPixels(16, 16)
	baseline := DigestFrame(pixels, 16, 16, 3, 4, 1)
	const workers = 8
	results := make([]Digest, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			results[slot] = DigestFrame(pixels, 16, 16, 3, 4, 1)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if results[i].Summary != baseline.Summary {
			t.Fatalf("worker %d summary differs", i)
		}
		for j := range baseline.Blocks {
			if results[i].Blocks[j].Hash != baseline.Blocks[j].Hash {
				t.Fatalf("worker %d block %d hash differs", i, j)
			}
		}
	}
}

func TestSinglePixelChangesItsBlock(t *testing.T) {
	pixels := testPixels(16, 16)
	before := DigestFrame(pixels, 16, 16, 3, 4, 1)
	pixels[(2*16+2)*3] ^= 0xff
	after := DigestFrame(pixels, 16, 16, 3, 4, 1)
	changed := 0
	for i := range before.Blocks {
		if before.Blocks[i].Hash != after.Blocks[i].Hash {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("changed block count = %d, want 1", changed)
	}
	// Pixel (2,2) is in block row=0,col=0 for block size 4.
	if before.Blocks[0].Hash == after.Blocks[0].Hash {
		t.Fatal("target block hash did not change")
	}
	if before.Summary == after.Summary {
		t.Fatal("frame summary did not change")
	}
}

func TestSparseScreenDigestStaysDeterministic(t *testing.T) {
	pixels := testPixels(16, 16)
	a := DigestFrame(pixels, 16, 16, 3, 4, 2)
	b := DigestFrame(pixels, 16, 16, 3, 4, 2)
	if a.Summary != b.Summary {
		t.Fatal("screening digest is not deterministic")
	}
}

func TestSummaryIndependentOfBlockProcessingOrder(t *testing.T) {
	pixels := testPixels(16, 16)
	digest := DigestFrame(pixels, 16, 16, 3, 4, 1)
	reversed := make([]model.BlockMetric, len(digest.Blocks))
	for i := range digest.Blocks {
		reversed[len(digest.Blocks)-1-i] = digest.Blocks[i]
	}
	if got := SummarizeBlocks(16, 16, 3, 4, reversed); got != digest.Summary {
		t.Fatalf("reversed summary = %s, want %s", got, digest.Summary)
	}
	if _, err := hex.DecodeString(digest.Summary); err != nil {
		t.Fatal(err)
	}
}

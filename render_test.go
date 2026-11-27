package volumefog

import (
	"path/filepath"
	"testing"

	"volumefog/store"
)

func openTestStore(t *testing.T, path string) store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

func TestRenderFullFrameAndCache(t *testing.T) {
	scene := testScene(t)
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	st := openTestStore(t, path)
	defer st.Close()
	renderer := NewRenderer(st)
	frame, err := renderer.Render(scene, RenderOptions{
		Mode: "exact", Steps: scene.Recipe.Steps, ShadowSamples: scene.Recipe.ShadowSamples,
		Persist: true,
	})
	if err != nil {
		t.Fatalf("Render exact: %v", err)
	}
	if len(frame.Chunks) != 8 {
		t.Fatalf("chunks=%d want 8", len(frame.Chunks))
	}
	wantSeq := []int64{0, 1, 2, 3, 4, 5, 6, 7}
	if len(frame.CommittedSequence) != len(wantSeq) {
		t.Fatalf("commits=%v", frame.CommittedSequence)
	}
	for i := range wantSeq {
		if frame.CommittedSequence[i] != wantSeq[i] {
			t.Fatalf("commit sequence=%v want %v", frame.CommittedSequence, wantSeq)
		}
	}
	truth := map[ChunkCoord]ScreenTruth{}
	for _, ct := range scene.Truth.Chunks {
		truth[ChunkCoord{X: ct.ChunkX, Y: ct.ChunkY}] = ct
	}
	for _, ch := range frame.Chunks {
		want := truth[ChunkCoord{X: ch.ChunkX, Y: ch.ChunkY}]
		if absFloat(ch.CenterTransmittance-want.Transmittance) > 2e-7 {
			t.Fatalf("chunk %s center T=%g want %g", ChunkCoord{X: ch.ChunkX, Y: ch.ChunkY},
				ch.CenterTransmittance, want.Transmittance)
		}
		for c := 0; c < 3; c++ {
			if absFloat(ch.CenterColor[c]-want.Color[c]) > 2e-7 {
				t.Fatalf("chunk %s center color[%d]=%g want %g", ChunkCoord{X: ch.ChunkX, Y: ch.ChunkY},
					c, ch.CenterColor[c], want.Color[c])
			}
		}
	}
	frame2, err := renderer.Render(scene, RenderOptions{Mode: "resume", Persist: true, UseCache: true})
	if err != nil {
		t.Fatalf("Render cached: %v", err)
	}
	if frame2.Stats.CacheHits != 8 || frame2.Stats.ComputedChunks != 0 {
		t.Fatalf("cache stats hits=%d computed=%d", frame2.Stats.CacheHits, frame2.Stats.ComputedChunks)
	}
	if err := CompareFrames(frame, frame2); err != nil {
		t.Fatalf("cached frame differs: %v", err)
	}
}

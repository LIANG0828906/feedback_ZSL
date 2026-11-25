package audiofp

import (
	"reflect"
	"testing"
)

func searchFixture() (Recipe, []FrameFingerprint, []AssetAnalysis) {
	r := Recipe{FrameLen: 64, FrameShift: 32, Bands: 4, QuantBits: 3,
		SimilarityThreshold: 0.4, CandidateLimit: 5}
	makeFrame := func(asset string, frame int, levels []uint8) FrameFingerprint {
		start := frame * r.FrameShift
		packed := PackLevels(levels, r.QuantBits)
		return FrameFingerprint{
			AssetID: asset, FrameNo: frame, StartSample: start, Bands: r.Bands,
			BitWidth: r.QuantBits, Levels: levels, PackedHex: packed,
			Summary: FrameSummary(start, levels, r.QuantBits, false),
		}
	}
	frames := []FrameFingerprint{
		makeFrame("q", 0, []uint8{7, 1, 2, 0}),
		makeFrame("q", 1, []uint8{1, 6, 0, 2}),
	}
	aFrames := []FrameFingerprint{
		makeFrame("a", 0, []uint8{0, 0, 1, 7}),
		makeFrame("a", 1, []uint8{7, 1, 2, 0}),
		makeFrame("a", 2, []uint8{1, 6, 0, 2}),
		makeFrame("a", 3, []uint8{2, 2, 2, 2}),
	}
	bFrames := []FrameFingerprint{
		makeFrame("b", 0, []uint8{6, 1, 2, 1}),
		makeFrame("b", 1, []uint8{1, 5, 0, 3}),
		makeFrame("b", 2, []uint8{3, 2, 0, 2}),
	}
	analyses := []AssetAnalysis{
		{AssetID: "b", Frames: bFrames, Index: BuildIndexEntries("b", "hash-b", r, bFrames)},
		{AssetID: "a", Frames: aFrames, Index: BuildIndexEntries("a", "hash-a", r, aFrames)},
	}
	return r, frames, analyses
}

func TestSearchIsPureAndTieBrokenDeterministically(t *testing.T) {
	r, query, analyses := searchFixture()
	first, err := SearchQuery("q0", query, analyses, r, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Matches) == 0 || first.Matches[0].AssetID != "a" || first.Matches[0].StartSample != 32 {
		t.Fatalf("exact query match did not rank first: %+v", first.Matches)
	}
	reversed := []AssetAnalysis{analyses[1], analyses[0]}
	second, err := SearchQuery("q0", query, reversed, r, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("search changed with insertion order")
	}
	if _, err := SearchQuery("q0", query, analyses, r, 0, 0); CodeOf(err) != CodeParameter {
		t.Fatalf("got %v, want E_PARA for candidate limit", err)
	}
}

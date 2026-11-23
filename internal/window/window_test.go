package window_test

import (
	"testing"

	"captionx/internal/envelope"
	"captionx/internal/model"
	"captionx/internal/window"
)

// stubDetection builds a detection whose quantised boundaries equal the
// declared segments (no quantisation noise) for deterministic tests.
func stubDetection(segs []model.SegmentManifest) *envelope.Detection {
	d := &envelope.Detection{}
	for _, s := range segs {
		d.Segments = append(d.Segments, envelope.DetectedSegment{
			Start: model.BoundaryObservation{SegmentID: s.ID, Kind: "start", Raw: s.Start, Quantized: s.Start},
			End:   model.BoundaryObservation{SegmentID: s.ID, Kind: "end", Raw: s.End, Quantized: s.End},
		})
	}
	return d
}

func reading(rateNum, maxNum int64) model.ReadingParams {
	return model.ReadingParams{
		SpeechRateNum: rateNum, SpeechRateDen: 1,
		MaxReadRateNum: maxNum, MaxReadRateDen: 1,
		HighlightMode: model.HighlightWeighted,
	}
}

func TestDerivePacksWithinSegment(t *testing.T) {
	segs := []model.SegmentManifest{{ID: 0, Start: 1000, End: 11000}}
	in := &window.Input{
		Captions: []model.CaptionManifest{
			{ID: 1, Text: "aaaaa", SegmentID: 0},
			{ID: 2, Text: "bbbbb", SegmentID: 0},
		},
		CharCounts: []int{5, 5},
		Segments:   segs,
		Detection:  stubDetection(segs),
		Reading:    reading(1, 1), // 1 char/sec at 1000 samples/sec -> 5000 samples each
		SampleRate: 1000,
		Total:      11000,
	}
	wins, decisions, err := window.Derive(in)
	if err != nil {
		t.Fatal(err)
	}
	if wins[0] != (model.Window{Start: 1000, End: 6000}) {
		t.Fatalf("win0 = %+v", wins[0])
	}
	if wins[1] != (model.Window{Start: 6000, End: 11000}) {
		t.Fatalf("win1 = %+v", wins[1])
	}
	if len(decisions) != 0 {
		t.Fatalf("unexpected decisions: %+v", decisions)
	}
}

func TestMinDurationExtendIntoGaps(t *testing.T) {
	segs := []model.SegmentManifest{{ID: 0, Start: 1000, End: 2000}}
	in := &window.Input{
		Captions:   []model.CaptionManifest{{ID: 1, Text: "aaaaa", SegmentID: 0}},
		CharCounts: []int{5},
		Segments:   segs,
		Detection:  stubDetection(segs),
		// speech rate 10 chars/sec -> desired 500; ceiling 1 char/sec -> min 5000
		Reading:    reading(10, 1),
		SampleRate: 1000,
		Total:      10000,
	}
	wins, decisions, err := window.Derive(in)
	if err != nil {
		t.Fatal(err)
	}
	// need = 4500; left reclaim = 1000 (silence before segment),
	// right reclaim = tail 500 + trailing silence 8000 -> capped at 3500.
	if wins[0].Len() != 5000 {
		t.Fatalf("len = %d, want 5000", wins[0].Len())
	}
	if wins[0].Start != 0 {
		t.Fatalf("start = %d, want 0 (left reclaim capped at 1000)", wins[0].Start)
	}
	if len(decisions) != 1 || decisions[0].Kind != model.DecisionExtend {
		t.Fatalf("decisions = %+v", decisions)
	}
	if decisions[0].CaptionID != 1 || decisions[0].Reason == "" {
		t.Fatalf("decision not localised: %+v", decisions[0])
	}
}

func TestMinDurationWholeSegmentFallback(t *testing.T) {
	segs := []model.SegmentManifest{{ID: 0, Start: 0, End: 10000}}
	in := &window.Input{
		Captions: []model.CaptionManifest{
			{ID: 1, Text: "x", SegmentID: 0, ExplicitWindow: &model.Window{Start: 0, End: 9000}},
			{ID: 2, Text: "aaaaa", SegmentID: 0},
		},
		CharCounts: []int{1, 5},
		Segments:   segs,
		Detection:  stubDetection(segs),
		Reading:    reading(10, 1), // desired 500, min 5000
		SampleRate: 1000,
		Total:      10000,
	}
	wins, decisions, err := window.Derive(in)
	if err != nil {
		t.Fatal(err)
	}
	// caption 2: window 500, left gap 0, tail 500 -> still short;
	// whole segment 10000 >= 5000 -> fall back to the whole segment.
	if wins[1] != (model.Window{Start: 0, End: 10000}) {
		t.Fatalf("win1 = %+v", wins[1])
	}
	var kinds []model.DecisionKind
	for _, d := range decisions {
		kinds = append(kinds, d.Kind)
	}
	if len(kinds) != 2 || kinds[0] != model.DecisionExtend || kinds[1] != model.DecisionWholeSegment {
		t.Fatalf("decision kinds = %v", kinds)
	}
}

func TestMinDurationCompressDwellFallback(t *testing.T) {
	segs := []model.SegmentManifest{{ID: 0, Start: 0, End: 4000}}
	in := &window.Input{
		Captions: []model.CaptionManifest{
			{ID: 1, Text: "x", SegmentID: 0, ExplicitWindow: &model.Window{Start: 0, End: 3500}},
			{ID: 2, Text: "aaaaa", SegmentID: 0},
		},
		CharCounts: []int{1, 5},
		Segments:   segs,
		Detection:  stubDetection(segs),
		Reading:    reading(10, 1), // desired 500, min 5000 > whole segment 4000
		SampleRate: 1000,
		Total:      4000,
	}
	wins, decisions, err := window.Derive(in)
	if err != nil {
		t.Fatal(err)
	}
	if wins[1] != (model.Window{Start: 3500, End: 4000}) {
		t.Fatalf("win1 = %+v (window kept, dwell compressed)", wins[1])
	}
	last := decisions[len(decisions)-1]
	if last.Kind != model.DecisionCompressDwell || last.CaptionID != 2 {
		t.Fatalf("last decision = %+v", last)
	}
}

func TestDeriveDeterministicDecisions(t *testing.T) {
	segs := []model.SegmentManifest{{ID: 0, Start: 0, End: 4000}}
	in := &window.Input{
		Captions: []model.CaptionManifest{
			{ID: 1, Text: "x", SegmentID: 0, ExplicitWindow: &model.Window{Start: 0, End: 3500}},
			{ID: 2, Text: "aaaaa", SegmentID: 0},
		},
		CharCounts: []int{1, 5},
		Segments:   segs,
		Detection:  stubDetection(segs),
		Reading:    reading(10, 1),
		SampleRate: 1000,
		Total:      4000,
	}
	w1, d1, err1 := window.Derive(in)
	w2, d2, err2 := window.Derive(in)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	for i := range w1 {
		if w1[i] != w2[i] {
			t.Fatalf("window %d differs across runs", i)
		}
	}
	if len(d1) != len(d2) {
		t.Fatalf("decision sequences differ in length")
	}
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Fatalf("decision %d differs across runs: %+v vs %+v", i, d1[i], d2[i])
		}
	}
}

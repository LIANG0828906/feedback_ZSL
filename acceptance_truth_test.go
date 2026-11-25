package audiobus_test

import (
	"encoding/json"
	"os"
	"testing"

	"audiobus/internal/loader"
	"audiobus/internal/pipeline"
)

type truthFile struct {
	Frames  map[string][]int64 `json:"frames"`
	Packets []struct {
		Window      int    `json:"window"`
		Channel     int    `json:"channel"`
		Seq         int    `json:"seq"`
		Start       int    `json:"start"`
		Samples     int    `json:"samples"`
		Disposition string `json:"disposition"`
		Mark        int64  `json:"mark"`
		SHA256      string `json:"sha256"`
	} `json:"packets"`
}

func TestTruthFrames(t *testing.T) {
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("artifacts/sample/truth.json")
	if err != nil {
		t.Fatal(err)
	}
	var truth truthFile
	if err := json.Unmarshal(raw, &truth); err != nil {
		t.Fatal(err)
	}
	events := loader.CanonicalEvents(set)
	engine := pipeline.NewEngine(set.Manifest, events, nil)
	result, err := engine.RunRealtime(nil)
	if err != nil {
		t.Fatal(err)
	}
	for ch := 0; ch < set.Manifest.Channels; ch++ {
		want := truth.Frames[string(rune('0'+ch))]
		got := result.Report.Frames[ch]
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ch=%d first mismatch i=%d got=%d want=%d", ch, i, got[i], want[i])
			}
		}
	}
	if len(result.Report.Packets) != len(truth.Packets) {
		t.Fatalf("packets got=%d want=%d", len(result.Report.Packets), len(truth.Packets))
	}
	byKey := map[[3]int]int{}
	for i, p := range truth.Packets {
		byKey[[3]int{p.Window, p.Channel, p.Seq}] = i
	}
	for _, got := range result.Report.Packets {
		ti := byKey[[3]int{got.Window, got.Channel, got.Seq}]
		want := truth.Packets[ti]
		if got.Start != want.Start || got.Samples != want.Samples || got.Disposition != want.Disposition || got.Mark != want.Mark || got.ContentHash != want.SHA256 {
			t.Fatalf("packet mismatch key=%v got=(start%d n%d disp%s mark%d hash%s) want=(start%d n%d disp%s mark%d hash%s)",
				[3]int{got.Window, got.Channel, got.Seq}, got.Start, got.Samples, got.Disposition, got.Mark, got.ContentHash,
				want.Start, want.Samples, want.Disposition, want.Mark, want.SHA256)
		}
	}
}

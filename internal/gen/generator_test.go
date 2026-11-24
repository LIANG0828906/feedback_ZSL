package gen

import (
	"path/filepath"
	"testing"

	"replaykernel/internal/model"
)

func TestGenerateDeterministic(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	spec := Spec{Seed: 123, MatchSpecs: []MatchSpec{{MatchID: "g1", Turns: 50, Units: 4}}}

	a, err := Generate(Spec{OutDir: dirA, Seed: spec.Seed, MatchSpecs: spec.MatchSpecs})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(Spec{OutDir: dirB, Seed: spec.Seed, MatchSpecs: spec.MatchSpecs})
	if err != nil {
		t.Fatal(err)
	}
	if a["g1"] != b["g1"] {
		t.Fatal("same seed must produce identical replay hash")
	}

	truthBytes := mustReadFile(t, filepath.Join(dirA, "truth", "g1.truth.json"))
	var truth model.MatchTruth
	if err := jsonUnmarshal(truthBytes, &truth); err != nil {
		t.Fatal(err)
	}
	if len(truth.Turns) != 50 {
		t.Fatalf("truth turns=%d", len(truth.Turns))
	}
	if len(truth.Turns[0].Units) != 4 {
		t.Fatalf("truth units=%d", len(truth.Turns[0].Units))
	}
}

func TestInjectSamples(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(Spec{
		OutDir: dir, Seed: 1,
		MatchSpecs: []MatchSpec{{MatchID: "g2", Turns: 40, Units: 4}},
	}); err != nil {
		t.Fatal(err)
	}
	replay := loadGenerated(t, filepath.Join(dir, "replays", "g2.json"))
	specs := []InjectSpec{
		{MatchID: "d", Kind: "decode"},
		{MatchID: "o", Kind: "order", Turn: 5},
		{MatchID: "c", Kind: "conflict", Turn: 5},
		{MatchID: "s", Kind: "stage", Turn: 5},
	}
	if err := BuildInjectSamples(replay, dir, specs); err != nil {
		t.Fatal(err)
	}
	for _, sp := range specs {
		if !exists(filepath.Join(dir, "faults", sp.MatchID+".json")) {
			t.Fatalf("missing injection file %s", sp.MatchID)
		}
	}
}

package generator_test

import (
	"testing"

	"retargetlab/internal/generator"
	"retargetlab/internal/model"
)

func TestFixedSeedProducesIdenticalWorkspace(t *testing.T) {
	opt := generator.Options{Seed: 314159, FrameCount: 20, PhaseSize: 5}
	a, err := generator.Generate(opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generator.Generate(opt)
	if err != nil {
		t.Fatal(err)
	}
	if model.StableHash(a) != model.StableHash(b) {
		t.Fatal("same seed produced different workspaces")
	}
	if a.Seed != b.Seed || len(a.Clip.Frames) != 20 || len(a.Targets) != 3 {
		t.Fatalf("unexpected generated shape: seed=%d frames=%d targets=%d", a.Seed, len(a.Clip.Frames), len(a.Targets))
	}
	if len(a.Truths) != len(a.Targets) {
		t.Fatalf("truth count %d does not match target count %d", len(a.Truths), len(a.Targets))
	}
}

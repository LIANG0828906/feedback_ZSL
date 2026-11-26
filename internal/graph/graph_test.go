package graph

import (
	"testing"

	"msworkbench/internal/domain"
	"msworkbench/internal/quant"
	"msworkbench/internal/sample"
)

func TestGainEditInvalidatesOnlyActiveDependencies(t *testing.T) {
	p := sample.NewCanonicalProject()
	set, err := sample.Generate(p)
	if err != nil {
		t.Fatal(err)
	}
	g := Build(p, set, domain.ScaleSparse, quant.DefaultThresholds())
	m, err := g.EditGain("B05", 1_050_000)
	if err != nil {
		t.Fatal(err)
	}

	invalid := map[string]bool{}
	for _, x := range m.Invalidated {
		invalid[x.NodeID] = true
	}
	if !invalid["quant:0,0:B05"] || !invalid["recon:0,0"] {
		t.Fatalf("active B05 block should be invalidated: %#v", m.Invalidated)
	}
	if invalid["recon:3,2"] || invalid["quant:3,2:B05"] {
		t.Fatalf("all-zero B05 block should remain valid: invalid=%v", invalid)
	}
	if invalid["quant:0,0:B01"] {
		t.Fatal("unrelated band cache node should not be invalidated")
	}

	foundReuse := false
	for _, r := range m.Reused {
		if r.NodeID == "recon:3,2" {
			foundReuse = true
		}
	}
	if !foundReuse {
		t.Fatalf("missing reuse manifest for unaffected block 3,2: %#v", m.Reused)
	}
}

func TestEquivalentVersionSwitchDoesNotInvalidateBlocks(t *testing.T) {
	p := sample.NewCanonicalProject()
	set, _ := sample.Generate(p)
	g := Build(p, set, domain.ScaleDense, quant.DefaultThresholds())
	next := sample.NewCanonicalProject()
	next.SourceSchema = domain.SchemaV1
	m := g.SwitchVersion(next)
	for _, x := range m.Invalidated {
		if x.Kind == KindQuant || x.Kind == KindRecon {
			t.Fatalf("equivalent schema switch should not invalidate block outputs: %#v", x)
		}
	}
	if len(m.Reused) == 0 {
		t.Fatal("expected block reuse entries")
	}
}

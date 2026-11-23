package metrics

import (
	"testing"

	"vqa/internal/errs"
)

func TestWindowStrideValidation(t *testing.T) {
	p := DefaultParams(8, 64, 64, 1)
	if err := p.Validate(); err != nil {
		t.Fatalf("default params invalid: %v", err)
	}
	bad := p
	bad.Window = 12 // larger than block size
	if errs.CodeOf(bad.Validate()) != errs.EPara {
		t.Fatal("window > block must return E_PARA")
	}
	bad = p
	bad.Stride = p.Window + 1
	if errs.CodeOf(bad.Validate()) != errs.EPara {
		t.Fatal("stride > window must return E_PARA")
	}
}

func TestHaloBandRule(t *testing.T) {
	p := DefaultParams(8, 64, 64, 1)
	p.Window = 7
	p.Stride = 4
	if got := p.RequiredHalo(); got != 6 {
		t.Fatalf("required halo = %d, want 6", got)
	}
	p.Halo = 5
	err := p.Validate()
	if errs.CodeOf(err) != errs.EBand {
		t.Fatalf("want E_BAND, got %v", err)
	}
}

func TestGridOwnershipCoversAllWindows(t *testing.T) {
	p := DefaultParams(8, 64, 64, 1)
	g := BuildGrid(p)
	if err := g.CheckBand(); err != nil {
		t.Fatalf("band check: %v", err)
	}
	owners := map[[2]int]bool{}
	for _, w := range g.Tops {
		owners[[2]int{w.OwnerBX, w.OwnerBY}] = true
	}
	if len(owners) != 64 {
		t.Fatalf("owned blocks = %d, want 64", len(owners))
	}
}

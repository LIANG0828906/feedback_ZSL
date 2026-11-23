package propagation

import (
	"testing"

	"vqa/internal/errs"
	"vqa/internal/sample"
)

func TestVerifyBoundDetectsBreach(t *testing.T) {
	r := &Report{
		Frame: 2,
		Blocks: []BlockPropagation{
			{BX: 1, BY: 3, MaxPixelErr: 99, PixelBound: 10},
		},
		Bound: FrameBound{Frame: 2, PSNRLowerBound: 20},
	}
	err := VerifyBound(r, 40)
	if errs.CodeOf(err) != errs.EBound {
		t.Fatalf("want E_BOUND, got %v", err)
	}
	be := err.(*errs.Error)
	if be.Fields["frame"] != "2" || be.Fields["bx"] != "1" || be.Fields["by"] != "3" {
		t.Fatalf("bound error missing coordinates: %+v", be.Fields)
	}
}

func TestVerifyBoundAcceptsValidReport(t *testing.T) {
	r := &Report{
		Frame: 0,
		Blocks: []BlockPropagation{
			{BX: 0, BY: 0, MaxPixelErr: 5, PixelBound: 10},
		},
		Bound: FrameBound{Frame: 0, PSNRLowerBound: 20},
	}
	if err := VerifyBound(r, 40); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	// PSNR below the analytic lower bound must also breach.
	if err := VerifyBound(r, 10); errs.CodeOf(err) != errs.EBound {
		t.Fatalf("want E_BOUND for PSNR breach, got %v", err)
	}
}

func TestAnalyzeBoundMatchesDerivation(t *testing.T) {
	// PixelBound for block n and step s is exactly n*s+1 by derivation.
	if got := sample.PixelBound(8, sample.QuantStep(70)); got != 8*sample.QuantStep(70)+1 {
		t.Fatalf("pixel bound derivation mismatch: %d", got)
	}
	if FrameBoundPSNR(sample.PixelBound(8, sample.QuantStep(90))) <=
		FrameBoundPSNR(sample.PixelBound(8, sample.QuantStep(50))) {
		t.Fatal("coarser step must yield a lower PSNR bound")
	}
}

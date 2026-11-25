package pipeline

import (
	"testing"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/sample"
)

func TestOverLargeWeldReportsDegenerateRing(t *testing.T) {
	samples, err := sample.GenerateAll(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var octa *sample.Sample
	for i := range samples {
		if samples[i].Manifest.ID == "octa" {
			octa = &samples[i]
		}
	}
	_, report, err := Weld(&octa.Mesh, 0.6)
	if errs.Code(err) != errs.EMERGE {
		t.Fatalf("err=%v want E_MERGE", err)
	}
	if report.FirstDegenerateLoop == nil || *report.FirstDegenerateLoop != 0 {
		t.Fatalf("first ring=%v want 0", report.FirstDegenerateLoop)
	}
	details := errs.Details(err)
	if details["first_degenerate_ring"] != 0 || details["merged_pairs"].(int) < 1 {
		t.Fatalf("error details missing ring/pairs: %+v", details)
	}
}

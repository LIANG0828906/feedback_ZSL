package report_test

import (
	"testing"

	"captionx/internal/errs"
	"captionx/internal/model"
	"captionx/internal/report"
)

func terms(id int, bq, wd, sr, dd int64) model.CaptionErrorTerms {
	return model.CaptionErrorTerms{
		CaptionID: id, BoundaryQuant: bq, WindowDeviation: wd,
		SchedResidual: sr, DurationDeviation: dd,
	}
}

func TestAssembleCumulativeBounds(t *testing.T) {
	hop := int64(160)
	in := []model.CaptionErrorTerms{
		terms(1, 80, 100, 3, 40),
		terms(2, 0, 0, 5, 0),
	}
	rep, err := report.Assemble(in, []int{10, 12}, hop, hop, 9000, 9010, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.BoundaryQuantBound) != 2 {
		t.Fatal("missing cumulative bounds")
	}
	if rep.BoundaryQuantBound[0] != hop || rep.BoundaryQuantBound[1] != 2*hop {
		t.Fatalf("boundary bounds = %v", rep.BoundaryQuantBound)
	}
	if rep.SchedResidualBound[1] != 22 {
		t.Fatalf("sched bound = %d, want 22 (chars)", rep.SchedResidualBound[1])
	}
	if rep.TotalDurationDeviation != 10 || rep.TotalDurationBound != 2*hop {
		t.Fatalf("total = %d bound %d", rep.TotalDurationDeviation, rep.TotalDurationBound)
	}
}

func TestAssembleBoundViolation(t *testing.T) {
	hop := int64(160)
	in := []model.CaptionErrorTerms{terms(7, 0, hop+1, 0, 0)}
	_, err := report.Assemble(in, []int{5}, hop, hop, 100, 100, true)
	if errs.Code(err) != errs.EBound {
		t.Fatalf("code = %s, want E_BOUND", errs.Code(err))
	}
	e := err.(*errs.Error)
	if e.CaptionID != 7 || e.Term != "windowDeviation" || e.Measured != hop+1 {
		t.Fatalf("E_BOUND localisation: %+v", e)
	}
}

func TestAssembleTotalViolation(t *testing.T) {
	hop := int64(160)
	in := []model.CaptionErrorTerms{terms(1, 0, 0, 0, 0)}
	_, err := report.Assemble(in, []int{5}, hop, hop, 100+hop+1, 100, true)
	if errs.Code(err) != errs.EBound {
		t.Fatalf("code = %s, want E_BOUND", errs.Code(err))
	}
	if e := err.(*errs.Error); e.Term != "totalDuration" {
		t.Fatalf("term = %s", e.Term)
	}
}

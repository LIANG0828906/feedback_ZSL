package maskqa

import (
	"context"
	"testing"
)

func TestSmokeFullRun(t *testing.T) {
	set, err := GenerateSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range set.Fixtures {
		t.Logf("fixture %s project=%dx%d candidate=%vx%v", fx.Name, fx.Project.Width, fx.Project.Height, func() int {
			if fx.Project.Candidate != nil {
				return fx.Project.Candidate.Width
			}
			return -1
		}(), func() int {
			if fx.Project.Candidate != nil {
				return fx.Project.Candidate.Height
			}
			return -1
		}())
		eng, err := NewEngine(fx.Project, fx.Truth, DefaultSchedulerConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := eng.Run(context.Background(), RunOptions{})
		if err != nil {
			if qe, ok := AsError(err); ok {
				t.Fatalf("%s report=%+v issuesTop=%+v dir=%+v", qe, res.Report, qe.Domains, qe.Segments)
			}
			t.Fatal(err)
		}
		if !res.Mask.Equal(fx.Truth.Mask) {
			tile, p, msg := FirstMaskDifference(res.Mask, fx.Truth.Mask, eng.Tiles)
			t.Fatalf("%s mask mismatch tile=%d p=%v %s", fx.Name, tile, p, msg)
		}
		t.Logf("%s mask=%s domains=%+v", fx.Name, res.Report.MaskHash[:12], res.Report.Topology.Domains)
		for _, d := range res.Report.Direction.Segments {
			t.Logf("dir %s exp=%s act=%s measured=%.2f normal=%.2f err=%.2f ok=%v", d.SegmentID, d.ExpectedCategory, d.ActualCategory, d.MeasuredAngle, d.NormalAngle, d.AngleError, d.OK)
		}
	}
}

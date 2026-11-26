package maskqa

import "testing"

func TestSmokeCompute(t *testing.T) {
	for idx := 0; idx < len(SampleSpecs()); idx++ {
		p, err := GenerateSample(idx)
		if err != nil {
			t.Fatal(err)
		}
		res, err := ComputeFull(p, DefaultSchedulerOptions(), nil)
		if err != nil {
			t.Fatalf("sample %d: %+v", idx, err)
		}
		if res == nil || res.Report == nil {
			t.Fatal("missing report")
		}
		for _, d := range res.Report.Domains {
			if d.Direction == nil || !d.Direction.Consistent {
				t.Fatalf("sample %d domain %d direction inconsistent: %+v", idx, d.DomainNo, d.Direction)
			}
			t.Logf("sample=%d domain no=%d truth=%d area=%d/%d holes=%d/%d band=%d/%d dev=%d dir=%s score=%d",
				idx, d.DomainNo, d.TruthNo, d.Component.Area, d.Truth.Area,
				d.Component.Holes, d.Truth.Holes,
				d.Component.BandWidthMilli, d.Truth.BandWidthMilli,
				len(d.Deviations), d.Direction.Observed, d.Direction.ScorePermyriad)
		}
	}
}

package quantforensics

import "testing"

func TestConfigurableDistanceMetrics(t *testing.T) {
	p, _ := DefaultProject()
	b := p.Blocks[3]
	im := p.Images[0]
	for _, metric := range []Metric{MetricWeightedRGB, MetricEuclidean, MetricMaxRGB} {
		params := p.Params
		params.Metric = metric
		q1, err := QuantizeBlock(im, b, p.Palette, params)
		if err != nil {
			t.Fatalf("%s: %v", metric, err)
		}
		q2, err := QuantizeBlock(im, b, p.Palette, params)
		if err != nil {
			t.Fatal(err)
		}
		if len(q1.Indices) != b.W*b.H || len(q1.PixelErrors) != b.W*b.H {
			t.Fatalf("%s produced incomplete output", metric)
		}
		if CanonicalHash(q1) != CanonicalHash(q2) {
			t.Fatalf("%s is not deterministic", metric)
		}
	}
}

func TestCompareLocatesFirstInconsistency(t *testing.T) {
	p, _ := DefaultProject()
	rep, err := NewEngine(p, NewMemoryStore("compare"), EngineOptions{Mode: "compare"}).Run()
	if err != nil {
		t.Fatal(err)
	}
	bad := *rep
	bad.Outcomes = append([]BlockOutcome(nil), rep.Outcomes...)
	out := bad.Outcomes[0]
	out.Quant.Indices = append([]int(nil), out.Quant.Indices...)
	out.Quant.Indices[0] = (out.Quant.Indices[0] + 1) % len(p.Palette.Entries)
	bad.Outcomes[0] = out

	cr := CompareRunReports(rep, &bad)
	if cr.Consistent || cr.FirstInconsistency == nil {
		t.Fatal("expected inconsistency")
	}
	if cr.FirstInconsistency.Coordinate.BlockID == "" || cr.FirstInconsistency.Field == "" {
		t.Fatalf("inconsistency location incomplete: %+v", cr.FirstInconsistency)
	}
}

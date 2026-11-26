package scale_test

import (
	"testing"

	"schedlab/internal/gen"
	"schedlab/internal/scale"
)

func TestAuditCriticalCases(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	results := scale.AuditCases(b.EdgeCases)
	if len(results) != len(b.EdgeCases) {
		t.Fatalf("case count %d want %d", len(results), len(b.EdgeCases))
	}
	for _, r := range results {
		if !r.Within {
			t.Fatalf("case %s outside tolerance: got=%d diff=%d",
				r.Case.Name, r.Got, r.Diff)
		}
	}
}

func TestExactConversions(t *testing.T) {
	if v, _, err := scale.PermilleToBPS(350); err != nil || v != 3500 {
		t.Fatalf("permille conversion = %d, %v", v, err)
	}
	if v, _, err := scale.CoresToMilli("0.25"); err != nil || v != 250 {
		t.Fatalf("cores conversion = %d, %v", v, err)
	}
	if v, _, err := scale.RatioToMilli("12.5%", 8000); err != nil || v != 1000 {
		t.Fatalf("ratio conversion = %d, %v", v, err)
	}
	if v, _, err := scale.TickToMs(15); err != nil || v != 1500 {
		t.Fatalf("tick conversion = %d, %v", v, err)
	}
	if _, _, err := scale.BPSToPermille(3505); err == nil {
		t.Fatal("non-integral permille must be rejected")
	}
}

func TestRankingUnchanged(t *testing.T) {
	order := []string{"a", "b", "c"}
	a := map[string]int64{"a": 100, "b": 80, "c": 50}
	b := map[string]int64{"a": 1000, "b": 800, "c": 500} // same ordering, scaled
	if !scale.RankingUnchanged(a, b, order) {
		t.Fatal("scaled scores must preserve ranking")
	}
	c := map[string]int64{"a": 10, "b": 99, "c": 50}
	if scale.RankingUnchanged(a, c, order) {
		t.Fatal("changed ordering must be detected")
	}
}

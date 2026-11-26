package quantforensics

import "testing"

func TestSmokeThreePaths(t *testing.T) {
	p, _ := DefaultProject()
	res, err := RunThreePathVerification(p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Consistency.Consistent {
		t.Fatalf("inconsistent: %+v details=%v", res.Consistency.FirstInconsistency, res.Consistency.Details)
	}
	if len(res.Realtime.Outcomes) != len(p.Blocks) {
		t.Fatalf("outcomes=%d blocks=%d", len(res.Realtime.Outcomes), len(p.Blocks))
	}
}

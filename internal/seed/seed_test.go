package seed

import "testing"

func TestFullProfileShape(t *testing.T) {
	specs := FullProfile(20261119)
	if len(specs) != 200 {
		t.Fatalf("count=%d", len(specs))
	}
	total, _, _, smallN, mediumN := SumSizes(specs)
	if smallN != 140 || mediumN != 60 {
		t.Fatalf("small=%d medium=%d", smallN, mediumN)
	}
	if total < 280<<20 || total > 320<<20 {
		t.Fatalf("total=%d bytes, want approximately 300 MiB", total)
	}
	for _, sp := range specs {
		if sp.Kind == "small" && (sp.Size < 64<<10 || sp.Size > 512<<10) {
			t.Fatalf("small size out of range: %d", sp.Size)
		}
		if sp.Kind == "medium" && (sp.Size < 1<<20 || sp.Size > 4<<20) {
			t.Fatalf("medium size out of range: %d", sp.Size)
		}
	}
}

func TestGenerateDeterministic(t *testing.T) {
	a := Generate(42, 100000)
	b := Generate(42, 100000)
	if len(a) != len(b) {
		t.Fatal("length differs")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("generator is not deterministic")
		}
	}
}

package fixedpoint

import "testing"

func TestCriticalCasesStayWithinTolerance(t *testing.T) {
	cases := CriticalCases(64, DefaultTickMs, 1)
	for _, c := range cases {
		if !c.WithinTolerance() {
			t.Fatalf("case %s outside tolerance: got=%d want=%d err=%d",
				c.Name, c.Got, c.Want, c.Error)
		}
	}
	if !AllWithinTolerance(cases) {
		t.Fatal("aggregated tolerance check failed")
	}
}

func TestSaturationCaseMarked(t *testing.T) {
	cases := CriticalCases(64, DefaultTickMs, 1)
	for _, c := range cases {
		if c.Name == "narrow_width_saturation" {
			if c.Got != maxInt32() {
				t.Fatalf("saturation clamp wrong: %d", c.Got)
			}
			return
		}
	}
	t.Fatal("saturation case missing")
}

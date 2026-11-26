package kernel

import (
	"encoding/json"
	"testing"
)

func TestSmokeGenerateAndLoad(t *testing.T) {
	b, err := GenerateBundle(42)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []map[string]any{b.V1, b.V2, b.V3} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		p, err := LoadJSON(data)
		if err != nil {
			t.Fatalf("load %s: %v", v["schema"], err)
		}
		if p.Fingerprint() != b.Project.Fingerprint() {
			t.Fatalf("fingerprint mismatch: got %s want %s", p.Fingerprint(), b.Project.Fingerprint())
		}
	}
	r, err := RunEngine(b.Project, DefaultBudgets(), ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range r.Ticks {
		for _, a := range tick.Allocations {
			if !a.WithinBudget {
				t.Fatalf("budget violated %s: %d > %d", a.RequestID, a.DelayMS, a.BudgetMS)
			}
		}
	}
	data, _ := json.MarshalIndent(b.Truth.Tick(2), "", "  ")
	t.Logf("tick2=%s", data)
}

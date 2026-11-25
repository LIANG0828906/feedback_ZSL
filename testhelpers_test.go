package thermalcache

import (
	"encoding/json"
	"errors"
	"testing"
)

func fixture42(t *testing.T) *Fixture {
	t.Helper()
	f, err := GenerateFixture(42)
	if err != nil {
		t.Fatalf("GenerateFixture: %v", err)
	}
	return f
}

func loadVersion(t *testing.T, f *Fixture, version string) *Project {
	t.Helper()
	p, err := LoadProject(f.Documents[version])
	if err != nil {
		t.Fatalf("LoadProject(%s): %v", version, err)
	}
	return p
}

func runProject(t *testing.T, p *Project) *SimulationResult {
	t.Helper()
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, err := eng.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func tickAt(t *testing.T, res *SimulationResult, at int64) TickResult {
	t.Helper()
	for _, tick := range res.Ticks {
		if tick.AtMs == at {
			return tick
		}
	}
	t.Fatalf("tick %d not found", at)
	return TickResult{}
}

func objectAt(t *testing.T, tick TickResult, id string) ObjectTick {
	t.Helper()
	for _, obj := range tick.Objects {
		if obj.ObjectID == id {
			return obj
		}
	}
	t.Fatalf("object %s not found at %d", id, tick.AtMs)
	return ObjectTick{}
}

func findBatch(res *SimulationResult, id string) (BatchResult, bool) {
	for _, b := range res.Batches {
		if b.ID == id {
			return b, true
		}
	}
	return BatchResult{}, false
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if got := ErrorCode(err); got != code {
		t.Fatalf("expected code %s, got %s (%v)", code, got, err)
	}
	var be *BizError
	if !errors.As(err, &be) || len(be.Context) == 0 {
		t.Fatalf("business error has no locatable context: %v", err)
	}
}

func normalizedResultJSON(t *testing.T, res *SimulationResult) string {
	t.Helper()
	cp := *res
	cp.SchemaVersion = ""
	cp.Fingerprint = ""
	cp.Ticks = append([]TickResult(nil), res.Ticks...)
	for i := range cp.Ticks {
		cp.Ticks[i].LayerMetrics = nil
	}
	b, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

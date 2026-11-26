package faultinject_test

import (
	"encoding/json"
	"testing"

	"schedlab/internal/domain"
	"schedlab/internal/faultinject"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"schedlab/internal/runtime"
)

func TestInjectionChainCodes(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	var v3 gen.V3Project
	_ = json.Unmarshal(b.V3JSON, &v3)

	cases := []struct {
		name string
		code faults.Code
		err  error
	}{
		{"weight", faults.E_WEIGHT_RANGE, asErr(faultinject.InjectWeightOutOfRange(&v3))},
		{"negative", faults.E_RESOURCE_NEGATIVE, asErr(faultinject.InjectNegativeResource(&v3))},
		{"topo", faults.E_TOPO_KEY_CONFLICT, asErr(faultinject.InjectTopoKeyConflict(&v3))},
		{"concurrency", faults.E_CONCURRENCY_BUDGET, asErr(faultinject.InjectConcurrencyBudget(p))},
		{"buffer", faults.E_BUFFER_BUDGET, asErr(faultinject.InjectBufferBudget(p))},
		{"backpressure", faults.E_BACKPRESSURE, asErr(faultinject.InjectBackpressure(p))},
	}
	for _, c := range cases {
		if !faults.Is(c.err, c.code) {
			t.Fatalf("%s: got %v want %s", c.name, c.err, c.code)
		}
	}

	store, _ := runtime.OpenStore(t.TempDir())
	trunc, err := faultinject.InjectTruncatedCheckpoint(store, p)
	if err != nil {
		t.Fatal(err)
	}
	if trunc.Code != faults.E_CHECKPOINT_TRUNCATED {
		t.Fatalf("truncated code = %s", trunc.Code)
	}
	store2, _ := runtime.OpenStore(t.TempDir())
	mismatch, err := faultinject.InjectStateVersionMismatch(store2, p)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch.Code != faults.E_STATE_VERSION_MISMATCH {
		t.Fatalf("mismatch code = %s", mismatch.Code)
	}
}

func TestSourceNotMutated(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	before := faultinject.SourceChecksum(b.V3JSON)
	var v3 gen.V3Project
	_ = json.Unmarshal(b.V3JSON, &v3)
	_ = faultinject.InjectWeightOutOfRange(&v3)
	_ = faultinject.InjectNegativeResource(&v3)
	after := faultinject.SourceChecksum(b.V3JSON)
	if before != after {
		t.Fatal("source project JSON mutated by injection chain")
	}
}

func asErr(r faultinject.InjectionResult) error {
	if r.Code == "" {
		return nil
	}
	return faults.New(r.Code, "test", mapToKV(r.Context)...)
}

func mapToKV(m map[string]string) []string {
	var kv []string
	for k, v := range m {
		kv = append(kv, k, v)
	}
	return kv
}

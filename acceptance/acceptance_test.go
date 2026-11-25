package acceptance_test

import (
	"testing"

	"coldkernel/internal/engine"
	"coldkernel/internal/oracle"
	"coldkernel/internal/sample"
	"coldkernel/internal/testutil"
)

const defaultSeed = uint64(20261126)

func loadTriple(t *testing.T) *sample.Triple {
	t.Helper()
	tr, err := sample.Build(defaultSeed)
	if err != nil {
		t.Fatalf("sample build: %v", err)
	}
	return tr
}

func TestThreeSchemasLoadIdentical(t *testing.T) {
	tr := loadTriple(t)
	testutil.AssertProjectsEqual(t, tr.ProjectV1, tr.ProjectV2, tr.ProjectV3)
}

func TestOracleIdenticalAcrossSchemas(t *testing.T) {
	tr := loadTriple(t)
	r1 := oracle.Simulate(tr.ProjectV1)
	r2 := oracle.Simulate(tr.ProjectV2)
	r3 := oracle.Simulate(tr.ProjectV3)
	if err := testutil.AssertRunsEqual("v2-vs-v1", r1, r2); err != nil {
		t.Fatal(err)
	}
	if err := testutil.AssertRunsEqual("v3-vs-v1", r1, r3); err != nil {
		t.Fatal(err)
	}
}

func TestEngineMatchesOracleUnderAllBudgets(t *testing.T) {
	tr := loadTriple(t)
	want := oracle.Simulate(tr.ProjectV1)
	configs := []engine.Config{
		{Workers: 1, FuncBuffer: 4, SubmitBatch: 1},
		{Workers: 2, FuncBuffer: 4, SubmitBatch: 1},
		{Workers: 4, FuncBuffer: 4, SubmitBatch: 2},
		{Workers: 16, FuncBuffer: 8, SubmitBatch: 3},
	}
	for i, cfg := range configs {
		got, timings, err := engine.Run(tr.ProjectV1, cfg)
		if err != nil {
			t.Fatalf("config %d run: %v", i, err)
		}
		if len(timings) == 0 {
			t.Fatalf("config %d produced no timing diagnostics", i)
		}
		if err := testutil.AssertRunsEqual("engine-cfg", want, got); err != nil {
			t.Fatalf("config %+v: %v", cfg, err)
		}
	}
}

func TestBackpressureCodesAreStable(t *testing.T) {
	tr := loadTriple(t)
	if _, _, err := engine.Run(tr.ProjectV1, engine.Config{Workers: 0, FuncBuffer: 1}); err == nil {
		t.Fatal("expected worker backpressure error")
	}
	if _, _, err := engine.Run(tr.ProjectV1, engine.Config{Workers: 1, FuncBuffer: 0}); err == nil {
		t.Fatal("expected invalid buffer backpressure error")
	}
	if _, _, err := engine.Run(tr.ProjectV1, engine.Config{Workers: 1, FuncBuffer: 1, SubmitBatch: 1}); err == nil {
		t.Fatal("expected buffer-exhaustion backpressure with 4 funcs and buffer 1")
	}
}

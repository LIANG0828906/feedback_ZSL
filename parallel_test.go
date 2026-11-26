package locklab

import "testing"

func TestParallelSchedulingDoesNotChangeSemantics(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	reference, err := Run(project, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	variants := []ParallelOptions{
		{Workers: 1, BatchSize: 1, ScheduleSeed: 1},
		{Workers: 2, BatchSize: 3, ScheduleSeed: 2},
		{Workers: 4, BatchSize: 7, ScheduleSeed: 99},
	}
	for _, options := range variants {
		result, err := RunParallel(project, options)
		if err != nil {
			t.Fatalf("%+v: %v", options, err)
		}
		if result.Run.SemanticDigest() != reference.SemanticDigest() {
			t.Fatalf("%+v changed lock/visibility semantics", options)
		}
		if len(result.Diagnostics.StepObservations) != len(result.Run.Steps) {
			t.Fatalf("missing per-step diagnostics: got %d want %d",
				len(result.Diagnostics.StepObservations), len(result.Run.Steps))
		}
		for _, observation := range result.Diagnostics.StepObservations {
			if observation.ElapsedMicros < 0 {
				t.Fatalf("negative elapsed observation")
			}
		}
	}
}

func TestParallelBudgetBackpressure(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	if _, err := RunParallel(project, ParallelOptions{Workers: MaxWorkers + 1}); ErrorCodeOf(err) != EBackpressureWorkers {
		t.Fatalf("want worker backpressure, got %v", err)
	}
	if _, err := RunParallel(project, ParallelOptions{Workers: 2, MemoryBudgetBytes: 128}); ErrorCodeOf(err) != EBackpressureMemory {
		t.Fatalf("want memory backpressure, got %v", err)
	}
	project.Transactions[0].Ops[2].Value = string(make([]byte, 8192))
	if _, err := RunParallel(project, ParallelOptions{Workers: 2, MemoryBudgetBytes: 4096}); ErrorCodeOf(err) != EBackpressureMemory {
		t.Fatalf("want per-operation memory backpressure, got %v", err)
	}
	for tx := range project.Transactions {
		for op := range project.Transactions[tx].Ops {
			project.Transactions[tx].Ops[op].Value = string(make([]byte, 8192))
		}
	}
	if _, err := RunParallel(project, ParallelOptions{
		Workers: 2, MemoryBudgetBytes: 4096, BatchSize: 7,
	}); ErrorCodeOf(err) != EBackpressureMemory {
		t.Fatalf("want multi-operation memory backpressure, got %v", err)
	}
}

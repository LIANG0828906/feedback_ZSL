package locklab

import "testing"

func TestFaultMatrixStableCodesAndSourceImmutability(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	before := project.CanonicalDigest()
	observations, err := RunFaultMatrix(project, "")
	if err != nil {
		t.Fatalf("fault matrix: %v", err)
	}
	if len(observations) != 8 {
		t.Fatalf("want 8 fault cases, got %d", len(observations))
	}
	expected := map[FaultKind]ErrorCode{
		FaultDuplicateItem:             EDuplicateItem,
		FaultWorkerBudget:              EBackpressureWorkers,
		FaultMemoryBudget:              EBackpressureMemory,
		FaultWaitGraphDangling:         EWaitGraphDangling,
		FaultTimeoutReversed:           ETimeoutReversed,
		FaultCheckpointTruncated:       ECheckpointTruncated,
		FaultProjectSnapshotMismatch:   EProjectVersion,
		FaultCheckpointVersionMismatch: ECheckpointVersion,
	}
	for _, observation := range observations {
		if expected[observation.Kind] != observation.Code {
			t.Fatalf("%s: got %s want %s", observation.Kind, observation.Code, expected[observation.Kind])
		}
		if observation.Path == "" {
			t.Fatalf("%s: missing locatable path", observation.Kind)
		}
		if !observation.SourceUnchanged {
			t.Fatalf("%s: source mutated", observation.Kind)
		}
	}
	if project.CanonicalDigest() != before {
		t.Fatalf("source project digest changed")
	}
}

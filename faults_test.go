package queryopt

import (
	"encoding/json"
	"testing"
)

func TestFaultInjectionStableCodes(t *testing.T) {
	tests := []struct {
		name  string
		fault string
		want  string
		load  bool
	}{
		{"histogram", FaultHistogramReversed, CodeHistogramOrder, false},
		{"selectivity scale", FaultSelectivityScale, CodeSelectivityScale, true},
		{"missing table", FaultMissingTableEdge, CodeMissingTable, false},
		{"negative rows", FaultNegativeRows, CodeNegativeRows, false},
		{"worker budget", FaultWorkerBudgetInvalid, CodeBudgetInvalid, false},
		{"buffer budget", FaultBufferBudgetInvalid, CodeBudgetInvalid, false},
		{"snapshot mismatch", FaultSnapshotMismatch, CodeSnapshotMismatch, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := InjectFault(tc.fault)
			if err != nil {
				t.Fatal(err)
			}
			var got error
			switch {
			case tc.fault == FaultWorkerBudgetInvalid || tc.fault == FaultBufferBudgetInvalid:
				got = ValidateBudget(env.Workers, env.Buffer)
			case tc.load && tc.fault == FaultSnapshotMismatch:
				_, got = LoadSnapshot(env.LoadVersion, env.Data)
			case tc.load:
				_, got = LoadProject(env.LoadVersion, env.Data)
			default:
				got = ValidateProject(env.Project)
			}
			if code := errorCode(got); code != tc.want {
				t.Fatalf("code=%s want %s err=%v", code, tc.want, got)
			}
			coded, _ := AsError(got)
			if coded.Context == nil || len(coded.Context) == 0 {
				t.Fatalf("error %s has no locatable context: %+v", tc.want, coded)
			}
		})
	}
}

func TestProcessingDoesNotMutateSourceProject(t *testing.T) {
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(sample.Project)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := EstimateProject(sample.Project, sample.Bindings); err != nil {
		t.Fatal(err)
	}
	if _, err := RunParallel(sample.Project, nil, sample.Bindings, OptionsFromProject(sample.Project)); err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(sample.Project, sample.Bindings, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EditHistogram("does.not.exist", nil); errorCode(err) != CodeInputInvalid {
		t.Fatalf("missing column edit code=%v", err)
	}
	if _, err := NewBoundedScheduler(0, 64); errorCode(err) != CodeBudgetInvalid {
		t.Fatalf("budget code=%v", err)
	}

	after, err := json.Marshal(sample.Project)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("source project JSON changed during processing")
	}
}

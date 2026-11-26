package slikernel

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

const fixtureSeed int64 = 20261127

func testOptions() Options {
	return Options{Workers: 4, PerWorkerBuffer: 16, SubmissionOrder: "forward"}
}

func fixture(t *testing.T) Fixture {
	t.Helper()
	result, err := GenerateFixture(fixtureSeed)
	if err != nil {
		t.Fatalf("GenerateFixture: %v", err)
	}
	return result
}

func compute(t *testing.T, project Project, options Options) *Report {
	t.Helper()
	engine, err := NewEngine(project, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	report, err := engine.ComputeAll(context.Background())
	if err != nil {
		t.Fatalf("ComputeAll: %v", err)
	}
	return report
}

func normalizeResult(result WindowResult) WindowResult {
	result.Provenance = Provenance{}
	return result
}

func assertReportCoreEqual(t *testing.T, expected, actual *Report) {
	t.Helper()
	if expected.ProjectID != actual.ProjectID {
		t.Fatalf("project id mismatch: %q != %q", expected.ProjectID, actual.ProjectID)
	}
	if expected.RuleVersion != actual.RuleVersion {
		t.Fatalf("rule version mismatch: %q != %q", expected.RuleVersion, actual.RuleVersion)
	}
	if expected.CommittedObservationSeq != actual.CommittedObservationSeq {
		t.Fatalf("observation seq mismatch: %d != %d", expected.CommittedObservationSeq, actual.CommittedObservationSeq)
	}
	if len(expected.Results) != len(actual.Results) {
		t.Fatalf("result count mismatch: %d != %d", len(expected.Results), len(actual.Results))
	}
	for index := range expected.Results {
		want := normalizeResult(expected.Results[index])
		got := normalizeResult(actual.Results[index])
		if !reflect.DeepEqual(want, got) {
			wantJSON, _ := json.MarshalIndent(want, "", "  ")
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			t.Fatalf("result %d mismatch\nwant:\n%s\ngot:\n%s", index, wantJSON, gotJSON)
		}
	}
}

func resultIndex(report *Report) map[ResultKey]WindowResult {
	index := make(map[ResultKey]WindowResult, len(report.Results))
	for _, result := range report.Results {
		index[result.Key()] = result
	}
	return index
}

func keySet[T comparable](values []T) map[T]bool {
	set := make(map[T]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func mutateJSON(t *testing.T, data []byte, mutation func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal mutation input: %v", err)
	}
	mutation(document)
	output, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal mutation output: %v", err)
	}
	return output
}

func serviceMap(t *testing.T, document map[string]any) []any {
	t.Helper()
	services, ok := document["services"].([]any)
	if !ok {
		t.Fatalf("services missing")
	}
	return services
}

func sliMaps(t *testing.T, service map[string]any, field string) []any {
	t.Helper()
	slis, ok := service[field].([]any)
	if !ok {
		t.Fatalf("%s missing", field)
	}
	return slis
}

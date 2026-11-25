package engine_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/generator"
)

func TestCanonicalProjectsMatchGeneratorTruth(t *testing.T) {
	suite := testSuite(t)
	for _, batchID := range suite.Order {
		batch := suite.Batches[batchID]
		t.Run(batchID, func(t *testing.T) {
			report, err := engine.Run(context.Background(), batch.Project, engine.DefaultOptions())
			if err != nil {
				t.Fatalf("engine run: %v", err)
			}
			assertReadsMatch(t, batch.Truth, report)
			assertWritesMatch(t, batch.Truth, report)
			assertCommitsMatch(t, batch.Truth, report)
			assertGCMatch(t, batch.Truth, report)
		})
	}
}

func testSuite(t *testing.T) *generator.Suite {
	t.Helper()
	suite, err := generator.DefaultSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func assertReadsMatch(t *testing.T, truth *domain.Truth, report *engine.Report) {
	t.Helper()
	if len(report.Reads) != len(truth.Reads) {
		t.Fatalf("read count=%d want %d", len(report.Reads), len(truth.Reads))
	}
	for _, got := range report.Reads {
		want := truth.Reads[got.OpID]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("read %s mismatch\ngot  %s\nwant %s", got.OpID, mustJSON(got), mustJSON(want))
		}
	}
}

func assertWritesMatch(t *testing.T, truth *domain.Truth, report *engine.Report) {
	t.Helper()
	if len(report.Writes) != len(truth.Writes) {
		t.Fatalf("write count=%d want %d", len(report.Writes), len(truth.Writes))
	}
	for _, got := range report.Writes {
		want := truth.Writes[got.OpID]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("write %s mismatch\ngot  %s\nwant %s", got.OpID, mustJSON(got), mustJSON(want))
		}
	}
}

func assertCommitsMatch(t *testing.T, truth *domain.Truth, report *engine.Report) {
	t.Helper()
	wantList := truth.CommitList()
	if len(report.Commits) != len(wantList) {
		t.Fatalf("commit count=%d want %d", len(report.Commits), len(wantList))
	}
	for i, got := range report.Commits {
		want := wantList[i]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("commit txn=%d mismatch\ngot  %s\nwant %s", got.TxnID, mustJSON(got), mustJSON(want))
		}
	}
}

func assertGCMatch(t *testing.T, truth *domain.Truth, report *engine.Report) {
	t.Helper()
	if len(report.GC) != len(truth.GC) {
		t.Fatalf("gc count=%d want %d", len(report.GC), len(truth.GC))
	}
	for i := range report.GC {
		got := report.GC[i]
		want := truth.GC[i]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("gc step %d mismatch\ngot  %s\nwant %s", i, mustJSON(got), mustJSON(want))
		}
	}
}

func mustJSON(v any) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

package incremental_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/generator"
	"mvccworkbench/internal/incremental"
)

func TestIsolationChangeRecomputesDependencyClosureOnly(t *testing.T) {
	suite, err := generator.DefaultSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	original := suite.Batches["B1-visibility"].Project
	originalBytes := mustJSON(t, original)

	svc, err := incremental.NewService(original, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := svc.ChangeIsolation(3, domain.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.ReusedReads) == 0 {
		t.Fatalf("expected reused reads, recomputed=%v", analysis.RecomputedReads)
	}
	if !contains(analysis.RecomputedReads, "OP-005") || !contains(analysis.RecomputedReads, "OP-008") {
		t.Fatalf("txn reads not invalidated: %+v", analysis.RecomputedReads)
	}
	for _, id := range analysis.RecomputedReads {
		if len(analysis.ReadResults[id].InvalidatedBy) == 0 {
			t.Fatalf("recomputed read %s lacks source", id)
		}
	}

	candidate := svc.CurrentProject()
	full, err := engine.Run(context.Background(), candidate, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range full.Reads {
		got := analysis.ReadResults[want.OpID]
		if !reflect.DeepEqual(got.Decision, want) {
			t.Fatalf("incremental read %s != full\ngot=%s\nwant=%s", want.OpID, mustJSON(t, got.Decision), mustJSON(t, want))
		}
	}
	for _, want := range full.GC {
		got := analysis.GCResults[want.StepID]
		if !reflect.DeepEqual(got.Decision, want) {
			t.Fatalf("incremental gc %s != full\ngot=%s\nwant=%s", want.StepID, mustJSON(t, got.Decision), mustJSON(t, want))
		}
	}
	assertOriginalUnchanged(t, original, originalBytes)
}

func TestStartChangeAndAppendDeleteMatchFullRecompute(t *testing.T) {
	suite, _ := generator.DefaultSuite(424242)
	project := suite.Batches["B1-visibility"].Project

	svc, err := incremental.NewService(project, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	startAnalysis, err := svc.ChangeTxnStart(2, 45)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(startAnalysis.RecomputedReads, "OP-004") || !contains(startAnalysis.RecomputedReads, "OP-009") {
		t.Fatalf("start change missed owned reads: %+v", startAnalysis.RecomputedReads)
	}
	assertAnalysisMatchesFull(t, svc, startAnalysis)

	appendAnalysis, err := svc.AppendOp(domain.Op{
		Kind: domain.OpRead, TxnID: 8, ItemID: "alpha", AtMS: 290,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(appendAnalysis.ReusedReads) == 0 {
		t.Fatalf("append should reuse prefix reads")
	}
	if len(appendAnalysis.RecomputedReads) != 1 {
		t.Fatalf("new reads=%v want exactly one", appendAnalysis.RecomputedReads)
	}
	assertAnalysisMatchesFull(t, svc, appendAnalysis)

	deleteAnalysis, err := svc.DeleteOp("OP-008")
	if err != nil {
		t.Fatal(err)
	}
	if contains(deleteAnalysis.RecomputedReads, "OP-005") {
		t.Fatalf("prefix read OP-005 should remain cached")
	}
	assertAnalysisMatchesFull(t, svc, deleteAnalysis)
}

func assertAnalysisMatchesFull(t *testing.T, svc *incremental.Service, analysis *incremental.Analysis) {
	t.Helper()
	candidate := svc.CurrentProject()
	full, err := engine.Run(context.Background(), candidate, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.ReadResults) != len(full.Reads) {
		t.Fatalf("incremental reads=%d full=%d", len(analysis.ReadResults), len(full.Reads))
	}
	for _, want := range full.Reads {
		got, ok := analysis.ReadResults[want.OpID]
		if !ok {
			t.Fatalf("missing incremental read %s", want.OpID)
		}
		if !reflect.DeepEqual(got.Decision, want) {
			t.Fatalf("read %s mismatch:\ngot  %s\nwant %s", want.OpID, mustJSON(t, got.Decision), mustJSON(t, want))
		}
	}
	if len(analysis.GCResults) != len(full.GC) {
		t.Fatalf("incremental gc=%d full=%d", len(analysis.GCResults), len(full.GC))
	}
	for _, want := range full.GC {
		got, ok := analysis.GCResults[want.StepID]
		if !ok {
			t.Fatalf("missing incremental gc %s", want.StepID)
		}
		if !reflect.DeepEqual(got.Decision, want) {
			t.Fatalf("gc %s mismatch:\ngot  %s\nwant %s", want.StepID, mustJSON(t, got.Decision), mustJSON(t, want))
		}
	}
}

func assertOriginalUnchanged(t *testing.T, original *domain.Project, before []byte) {
	t.Helper()
	after := mustJSON(t, original)
	if string(before) != string(after) {
		t.Fatalf("source project was modified\nbefore=%s\nafter=%s", before, after)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

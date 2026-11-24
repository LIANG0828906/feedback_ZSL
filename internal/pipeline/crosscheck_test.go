package pipeline_test

import (
	"encoding/json"
	"testing"

	"integritykernel/internal/core"
	"integritykernel/internal/pipeline"
)

func TestThreePathCrossCheckAcceptance(t *testing.T) {
	sampleDir := t.TempDir()
	artifactDir := t.TempDir()
	result, err := pipeline.RunCrossCheck(sampleDir, artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("cross-check failed: mismatch=%#v truth=%#v review=%#v", result.FirstMismatch, result.TruthCheck, result.Review)
	}
	if !result.TransactionCheckOK {
		t.Fatalf("transaction/recovery sequence check failed: injected=%#v restart=%#v log=%d",
			result.InjectedBatchError, result.RestartTransaction, len(result.InitialRecoveryLog))
	}
	if len(result.InitialRecoveryLog) != 96 {
		t.Fatalf("expected 96 ordered evidence records, got %d", len(result.InitialRecoveryLog))
	}
	final := result.States[len(result.States)-1]
	intervals := final.IncrementalReport.Intervals
	if len(intervals) != 1 || intervals[0].StartFrame != 13 || intervals[0].EndFrame != 16 {
		t.Fatalf("unexpected intervals: %s", core.MustJSON(intervals))
	}
	for _, state := range result.States {
		if state.IncrementalVsFullError != nil {
			t.Fatalf("state %s incremental mismatch: %#v", state.Name, state.IncrementalVsFullError)
		}
		if state.RecoveryVsFullError != nil {
			t.Fatalf("state %s recovery mismatch: %#v", state.Name, state.RecoveryVsFullError)
		}
	}
}

func TestCompareReportsLocatesNoiseBlock(t *testing.T) {
	sampleDir := t.TempDir()
	artifactDir := t.TempDir()
	result, err := pipeline.RunCrossCheck(sampleDir, artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	report := result.States[0].FullReport
	changed := cloneReport(t, report)
	block := changed.Frames[7].Noise.Blocks[3]
	changed.Frames[7].Noise.Blocks[3].NoiseVariance += 0.12345
	diff := pipeline.CompareReports(report, changed)
	if diff == nil || diff.Code != core.CodeDiff {
		t.Fatalf("expected E_DIFF, got %#v", diff)
	}
	if diff.Frame == nil || *diff.Frame != 7 || diff.EvidenceType != core.EvidenceNoise {
		t.Fatalf("unexpected diff location: %#v", diff)
	}
	if diff.BlockX == nil || diff.BlockY == nil || *diff.BlockX != block.BlockX || *diff.BlockY != block.BlockY {
		t.Fatalf("expected block (%d,%d), got (%v,%v)", block.BlockX, block.BlockY, diff.BlockX, diff.BlockY)
	}
	if diff.Field != "noise_variance" {
		t.Fatalf("expected noise_variance field, got %s", diff.Field)
	}
}

func cloneReport(t *testing.T, report *core.Report) *core.Report {
	t.Helper()
	data := core.CanonicalJSON(report)
	var out core.Report
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

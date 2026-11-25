package acceptance_test

import (
	"context"
	"path/filepath"
	"testing"

	"mvccworkbench/internal/diagnostics"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/schema"
	"mvccworkbench/internal/workbench"
)

func TestAcceptanceThreeEditionsAndTruth(t *testing.T) {
	svc, err := workbench.NewService(424242)
	if err != nil {
		t.Fatal(err)
	}
	schemas := []string{"", domain.SchemaV1, domain.SchemaV2, domain.SchemaV3}
	for _, batchID := range svc.BatchIDs() {
		for _, schemaVersion := range schemas {
			for _, parallel := range []bool{false, true} {
				report, err := svc.Verify(context.Background(), batchID, schemaVersion, parallel, engine.Options{
					Workers: 4, ItemBufferLimit: 128, ChainCap: 1024,
				})
				if err != nil {
					t.Fatalf("%s/%s parallel=%v: %v", batchID, schemaVersion, parallel, err)
				}
				if !report.OK {
					t.Fatalf("%s/%s parallel=%v mismatches=%d first=%+v", batchID, schemaVersion, parallel, len(report.Mismatches), report.Mismatches)
				}
			}
		}
	}
}

func TestAcceptanceInjectedInputExceptions(t *testing.T) {
	svc, _ := workbench.NewService(424242)
	project, _, err := svc.Project("B1-visibility", "canonical")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		diagnostics.InjectDuplicateItem:  domain.EInputDuplicateItem,
		diagnostics.InjectBadDeleteRef:   domain.EInputBadDeleteRef,
		diagnostics.InjectTimeOutOfRange: domain.EInputTimeOutOfRange,
	}
	for injection, wantCode := range cases {
		_, err := diagnostics.InjectInput(project, injection)
		if domain.CodeOf(err) != wantCode {
			t.Fatalf("%s code=%s want %s err=%v", injection, domain.CodeOf(err), wantCode, err)
		}
		if de, ok := domain.AsError(err); !ok || de.Path == "" {
			t.Fatalf("%s error lacks locatable path: %v", injection, err)
		}
	}
	_, _, err = schema.LoadDocument([]byte(`{"schemaVersion":"mvcc.project/v99"}`))
	if domain.CodeOf(err) != domain.ESchemaUnknown {
		t.Fatalf("unknown schema code=%s err=%v", domain.CodeOf(err), err)
	}
}

func TestAcceptanceCrashResumeUsesMatchedCheckpoint(t *testing.T) {
	svc, _ := workbench.NewService(424242)
	project, _, err := svc.Project("B2-conflict-gc", "canonical")
	if err != nil {
		t.Fatal(err)
	}
	report, err := workbench.CrashAndResume(context.Background(), project,
		filepath.Join(t.TempDir(), "acceptance.db"), 8, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if report.RecoveryMode != workbench.RecoveryResumed {
		t.Fatalf("mode=%s want %s", report.RecoveryMode, workbench.RecoveryResumed)
	}
	if report.CheckpointSeqAtCrash != 8 {
		t.Fatalf("crash seq=%d want 8", report.CheckpointSeqAtCrash)
	}
	if report.FinalSeq != len(project.Ops) {
		t.Fatalf("final seq=%d want %d", report.FinalSeq, len(project.Ops))
	}
}

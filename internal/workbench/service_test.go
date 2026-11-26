package workbench

import (
	"context"
	"path/filepath"
	"testing"

	"enginelab.local/internal/model"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(Config{DBPath: filepath.Join(t.TempDir(), "workbench.sqlite"), ResetDB: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestTimelineAllMomentsEqualFullRecompute(t *testing.T) {
	svc := newTestService(t)
	if len(svc.Timeline()) != 5 {
		t.Fatalf("moments=%d", len(svc.Timeline()))
	}
	for _, moment := range svc.Timeline() {
		if !moment.Comparison.Equal {
			t.Fatalf("moment %s not equal: %+v", moment.Key, moment.Comparison.Differences)
		}
	}
	report, err := svc.RestoreCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Parity {
		t.Fatalf("checkpoint restore parity failed: %v", report.Differences)
	}
}

func TestEveryInjectionHasStableBusinessCode(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		req  InjectRequest
		code model.ErrorCode
		ok   bool
	}{
		{"dup ref", InjectRequest{Kind: "duplicate_segment_ref"}, model.EDupRef, false},
		{"base conflict", InjectRequest{Kind: "cross_version_base_conflict"}, model.EBaseConflict, false},
		{"dup record", InjectRequest{Kind: "duplicate_record"}, model.EDupRecord, false},
		{"order", InjectRequest{Kind: "sequence_order"}, model.EOrder, false},
		{"cycle", InjectRequest{Kind: "dependency_cycle"}, model.EDepCycle, false},
		{"missing", InjectRequest{Kind: "dependency_missing"}, model.EDepMissing, false},
		{"trunc segment", InjectRequest{Kind: "truncated_segment_file"}, model.ESegmentTruncated, true},
		{"incomplete", InjectRequest{Kind: "incomplete_record"}, model.ERecordIncomplete, true},
		{"trunc checkpoint", InjectRequest{Kind: "truncated_checkpoint"}, model.ECPCorrupt, true},
		{"threads", InjectRequest{Kind: "budget_threads"}, model.EBudgetThreads, false},
		{"memory", InjectRequest{Kind: "budget_memory"}, model.EBudgetMemory, false},
		{"backpressure", InjectRequest{Kind: "backpressure"}, model.EBackpressure, false},
		{"schema version", InjectRequest{Kind: "schema_version_mismatch"}, model.ESchemaVersion, false},
		{"version", InjectRequest{Kind: "version_mismatch"}, model.EVersionMismatch, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := svc.Inject(ctx, tc.req)
			if resp.Code != tc.code {
				t.Fatalf("code=%s want %s err=%v", resp.Code, tc.code, resp.Error)
			}
			if resp.OK != tc.ok {
				t.Fatalf("ok=%v want %v response=%+v", resp.OK, tc.ok, resp)
			}
			if resp.Error == nil || resp.Error.Locator == "" {
				t.Fatalf("missing locatable error context: %+v", resp.Error)
			}
			if tc.ok && resp.Result == nil {
				t.Fatalf("recoverable injection must include result")
			}
		})
	}
}

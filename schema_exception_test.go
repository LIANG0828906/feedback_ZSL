package leasekit

import (
	"encoding/json"
	"testing"
)

func TestLoaderConflictAndUnknownFieldAreLocated(t *testing.T) {
	p := GenerateProject(FixedSeed)
	v3, err := ExportV3(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(v3, &doc); err != nil {
		t.Fatal(err)
	}
	tree := doc["LockTree"].([]any)
	first := tree[0].(map[string]any)
	window := first["RenewWindow"].(map[string]any)
	window["DeadlineAtSecond"] = 9
	bad, _ := json.Marshal(doc)
	if _, err := Load(bad); err == nil {
		t.Fatal("expected absolute window/TTL conflict")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeFieldConflict {
		t.Fatalf("expected E_FIELD_CONFLICT, got %v", err)
	}

	v1, err := ExportV1(p)
	if err != nil {
		t.Fatal(err)
	}
	var flat map[string]any
	_ = json.Unmarshal(v1, &flat)
	flat["unexpected_new_field"] = true
	unknown, _ := json.Marshal(flat)
	if _, err := Load(unknown); err == nil {
		t.Fatal("expected strict unknown field error")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeInvalidSchema {
		t.Fatalf("expected E_INVALID_SCHEMA, got %v", err)
	}

	if _, err := Load([]byte(`{"schema":"lease.project/v9"}`)); err == nil {
		t.Fatal("expected unsupported schema version")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeSchemaVersionMismatch {
		t.Fatalf("expected E_SCHEMA_VERSION_MISMATCH, got %v", err)
	}
}

func TestCompetitionMarksTemporalOverlap(t *testing.T) {
	p := GenerateProject(FixedSeed)
	truth, err := GenerateTruth(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{14, 16, 17} {
		r := truth.Report.ResultBySeq(seq)
		if r.Arbitration == nil || !r.Arbitration.TemporalOverlap {
			t.Fatalf("seq=%d missing overlapping arbitration: %+v", seq, r.Arbitration)
		}
	}
}

func TestExceptionDrillIsStableAndDoesNotMutateProject(t *testing.T) {
	p := GenerateProject(FixedSeed)
	before := string(mustJSON(p))
	obs, err := DrillExceptions(p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) < 8 {
		t.Fatalf("expected at least 8 exception observations, got %d: %+v", len(obs), obs)
	}
	want := map[string]bool{
		CodeDuplicateHolder:       false,
		CodeInvalidDuration:       false,
		CodeRenewLimitReached:     false,
		CodeWorkerBudgetOOR:       false,
		CodeAttemptBudgetOOR:      false,
		CodeCheckpointTruncated:   false,
		CodeSchemaVersionMismatch: false,
	}
	for _, o := range obs {
		if !o.Stable {
			t.Fatalf("unstable observation %+v", o)
		}
		if _, ok := want[o.Code]; ok {
			want[o.Code] = true
		}
	}
	for code, seen := range want {
		if !seen {
			t.Fatalf("missing exception code %s in %+v", code, obs)
		}
	}
	after := string(mustJSON(p))
	if before != after {
		t.Fatal("source project was modified by exception injection/drill")
	}
}

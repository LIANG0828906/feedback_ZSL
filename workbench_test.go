package fogkernel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type apiResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *BizError       `json:"error"`
}

func postJSON(t *testing.T, h http.Handler, path string, body any) apiResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	return resp
}

func TestWorkbenchInjectionAndEditAPI(t *testing.T) {
	wb, err := NewWorkbench()
	if err != nil {
		t.Fatal(err)
	}
	h := wb.Handler()

	get := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status %d", rec.Code)
	}

	kinds := map[string]string{
		"density_out_of_range":         CodeDensityRange,
		"density_shape_mismatch":       CodeDensityShape,
		"negative_coefficient":         CodeCoeffNegative,
		"distance_overflow":            CodeFixedOverflow,
		"unknown_schema":               CodeSchemaVersion,
		"mixed_schema_combination":     CodeSchemaCombination,
		"budget_workers":               CodeBudgetWorkers,
		"budget_buffer":                CodeBudgetBuffer,
		"backpressure":                 CodeBackpressure,
		"audit_tolerance_breach":       CodeAuditTolerance,
		"truncated_checkpoint":         CodeCheckpointTrunc,
		"version_mismatch_checkpoint":  CodeCheckpointVersion,
		"definitely_unknown_injection": CodeInjectionUnknown,
	}
	for kind, code := range kinds {
		resp := postJSON(t, h, "/api/inject", InjectionRequest{Kind: kind})
		if resp.OK || resp.Error == nil || resp.Error.Code != code {
			t.Fatalf("injection %s expected %s, got ok=%v err=%+v", kind, code, resp.OK, resp.Error)
		}
		if resp.Error.Path == "" && code != CodeBackpressure && code != CodeBudgetWorkers && code != CodeBudgetBuffer {
			// Budget validators set $.budget paths; audit sets path. Require locatable context broadly.
			if resp.Error.Path == "" {
				t.Fatalf("injection %s missing location path", kind)
			}
		}
	}

	beforeHash := wb.SourceHash
	edit := postJSON(t, h, "/api/edit", EditRequest{Kind: "voxel", MediumID: "fog-alpha", X: 1, Y: 1, Z: 1, Value: 199})
	if !edit.OK {
		t.Fatalf("edit failed: %+v", edit.Error)
	}
	if wb.SourceHash != beforeHash {
		t.Fatal("source hash changed after working-copy edit")
	}
}

func TestWorkbenchCheckpointCrashResumeAPI(t *testing.T) {
	wb, err := NewWorkbench()
	if err != nil {
		t.Fatal(err)
	}
	h := wb.Handler()
	path := t.TempDir() + "/wb.db"
	crash := postJSON(t, h, "/api/checkpoint", checkpointRequest{Action: "crash", DBPath: path, CrashAfterBlocks: 2})
	if !crash.OK {
		t.Fatalf("crash action failed: %+v", crash.Error)
	}
	resume := postJSON(t, h, "/api/checkpoint", checkpointRequest{Action: "resume", DBPath: path})
	if !resume.OK {
		t.Fatalf("resume failed: %+v", resume.Error)
	}
	var data struct {
		MatchesFull bool `json:"matches_full"`
	}
	if err := json.Unmarshal(resume.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.MatchesFull {
		t.Fatal("workbench checkpoint resume does not match full solve")
	}
}

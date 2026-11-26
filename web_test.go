package mirror

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnosticHandlerShowsRequiredSections(t *testing.T) {
	sample := mustGenerate(t, 42)
	paths, err := RunThreePaths(context.Background(), sample.Session, t.TempDir(), DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	app := &DiagnosticApp{Sample: sample, Paths: paths, Checkpoint: paths.Realtime.Checkpoint}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, needle := range []string{"拓扑", "逐窗口与镜像包", "误差报告与累计上界", "检查点"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("diagnostic page missing %q", needle)
		}
	}

	req = httptest.NewRequest("GET", "/api/state", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("api status = %d", rec.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("api JSON: %v", err)
	}
	if decoded["sessionId"] == "" {
		t.Fatalf("api payload = %+v", decoded)
	}
}

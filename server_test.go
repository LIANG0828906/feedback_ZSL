package dialogue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnosticHTTPRoutes(t *testing.T) {
	data, err := BuildDiagnosticData()
	if err != nil {
		t.Fatal(err)
	}
	if !data.OK {
		t.Fatalf("diagnostic self-checks failed: %+v", data.Checks)
	}
	server := NewServer(data)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "true") {
		t.Fatalf("health route code=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/diagnostic", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnostic api code=%d", rec.Code)
	}
	var decoded DiagnosticData
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("diagnostic JSON should decode: %v", err)
	}
	if !decoded.OK || len(decoded.Frames) == 0 {
		t.Fatalf("unexpected diagnostic payload ok=%v frames=%d", decoded.OK, len(decoded.Frames))
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "逐帧声学") {
		t.Fatalf("index route code=%d length=%d", rec.Code, rec.Body.Len())
	}
}

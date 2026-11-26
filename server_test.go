package maskqa

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkbenchHTTPRunAndPNG(t *testing.T) {
	app := NewWebApp(filepath.Join(t.TempDir(), "work.sqlite"))
	h := app.Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(`{"action":"run"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok": true`) {
		t.Fatalf("action status=%d body=%s", rec.Code, rec.Body.String())
	}

	stateReq := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	stateRec := httptest.NewRecorder()
	h.ServeHTTP(stateRec, stateReq)
	if stateRec.Code != http.StatusOK || !strings.Contains(stateRec.Body.String(), "final_mismatch") {
		t.Fatalf("state status=%d body=%s", stateRec.Code, stateRec.Body.String()[:minInt(200, stateRec.Body.Len())])
	}

	pngReq := httptest.NewRequest(http.MethodGet, "/png?sample=icon-ring-80x80&kind=diff", nil)
	pngRec := httptest.NewRecorder()
	h.ServeHTTP(pngRec, pngReq)
	if pngRec.Code != http.StatusOK || pngRec.Header().Get("Content-Type") != "image/png" || pngRec.Body.Len() < 100 {
		t.Fatalf("png status=%d type=%s bytes=%d", pngRec.Code, pngRec.Header().Get("Content-Type"), pngRec.Body.Len())
	}
}

package ocrflow

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticsPageAndEndpoints(t *testing.T) {
	shots, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(DefaultConfig(filepath.Join(t.TempDir(), "diag.db")), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if _, err := svc.Run("diagnostics"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"检测框叠加", "逐行结果", "失效传播", "预算占用", "E_SCHEMA", "E_DIFF", "shot-0001"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "template:") || !strings.Contains(body, "缓存条目") {
		t.Fatalf("dashboard template did not finish rendering: %s", body[maxInt(0, len(body)-500):])
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	apiRec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK || !strings.Contains(apiRec.Body.String(), `"line_sequence"`) {
		t.Fatalf("api state failed code=%d", apiRec.Code)
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK || !strings.Contains(healthRec.Body.String(), "E_DECODE") {
		t.Fatalf("health payload missing error codes: %d %s", healthRec.Code, healthRec.Body.String())
	}

	imgReq := httptest.NewRequest(http.MethodGet, "/shots/shot-0001/image.png", nil)
	imgRec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(imgRec, imgReq)
	if imgRec.Code != http.StatusOK {
		t.Fatalf("image status=%d", imgRec.Code)
	}
	if ct := imgRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type=%q", ct)
	}
	png := imgRec.Body.Bytes()
	if len(png) < 8 || string(png[:4]) != "\x89PNG" {
		t.Fatalf("response is not PNG: %x", png[:minInt(8, len(png))])
	}
}

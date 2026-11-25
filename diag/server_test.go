package diag

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiagnosticHTTPFlows(t *testing.T) {
	svc, err := NewService(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server := httptest.NewServer(svc.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("state status %d", resp.StatusCode)
	}

	post := func(path string, body any) map[string]any {
		b, _ := json.Marshal(body)
		r, err := http.Post(server.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	preview := post("/api/render/preview-compare", map[string]any{})
	if preview["ok"] != true {
		t.Fatalf("preview compare failed: %v", preview["comparison_error"])
	}
	dragPreview := post("/api/render/preview", map[string]any{"preview_steps": 3})
	if dragPreview["ok"] != true {
		t.Fatalf("drag preview failed: %v", dragPreview["error"])
	}
	edit := post("/api/edit/density", map[string]any{"declared": true})
	if edit["ok"] != true {
		t.Fatalf("density edit failed: %v", edit["error"])
	}
	fault := post("/api/faults/inject", map[string]any{"kind": "block", "block_id": 11})
	errObj, _ := fault["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "E_BLOCK" {
		t.Fatalf("expected E_BLOCK response, got %v", fault)
	}
	retry := post("/api/faults/retry", map[string]any{"block_id": 11})
	if retry["ok"] != true {
		t.Fatalf("retry failed: %v", retry["error"])
	}
	verify := post("/api/verify/truth", map[string]any{})
	if verify["ok"] != true {
		t.Fatalf("truth verification failed: %v", verify["error"])
	}
}

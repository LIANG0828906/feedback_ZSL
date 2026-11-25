package maskqa

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
)

func TestHTTPDiagnosticEndpoints(t *testing.T) {
	server, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body) < 1000 || !contains(string(body), "笔画方向") {
		t.Fatalf("index status=%d bytes=%d", resp.StatusCode, len(body))
	}

	for _, path := range []string{
		"/api/fixtures",
		"/api/project?sample=0&version=v1",
		"/api/project?sample=0&version=v2",
		"/api/project?sample=0&version=v3",
		"/api/diagnose?sample=0",
		"/api/image?sample=0",
	} {
		r, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s status=%d", path, r.StatusCode)
		}
	}

	r, err := ts.Client().Get(ts.URL + "/api/diagnose?sample=0&edit_stroke=1&dx=3&point=start")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("edit status=%d body=%s", r.StatusCode, string(b))
	}
	var diag diagnoseResponse
	if err := json.NewDecoder(r.Body).Decode(&diag); err != nil {
		t.Fatal(err)
	}
	if !diag.PixelMatchFull || diag.FirstDiffTile != -1 {
		t.Fatalf("HTTP incremental mismatch match=%v first=%d msg=%s", diag.PixelMatchFull, diag.FirstDiffTile, diag.FirstDiffMessage)
	}
	if len(diag.RecomputedTiles) != 1 || diag.RecomputedTiles[0] != 0 || len(diag.ReusedTiles) == 0 {
		t.Fatalf("HTTP invalidation recomputed=%v reused=%v", diag.RecomputedTiles, diag.ReusedTiles)
	}

	r2, err := ts.Client().Get(ts.URL + "/api/three-paths?sample=0")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var paths struct {
		Report *ThreePathReport `json:"report"`
	}
	if err := json.NewDecoder(r2.Body).Decode(&paths); err != nil {
		t.Fatal(err)
	}
	if paths.Report == nil || !paths.Report.OK {
		t.Fatalf("three path HTTP report=%+v", paths.Report)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

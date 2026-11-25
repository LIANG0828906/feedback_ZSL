package diagnostics_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lipsync/diagnostics"
	"lipsync/domain"
)

func TestDiagnosticHandlers(t *testing.T) {
	state := &diagnostics.State{
		ProjectID: "p", SourceVersion: "dialogue/v3", TruthFingerprint: "fp",
		PackageCount: 1, FrameCount: 1,
		Packages: []domain.ScheduledPackage{{Package: domain.Package{ID: "pkg", DialogueID: "d"}}},
		Frames:   []domain.FrameRecord{{Frame: 3, PackageIDs: []string{"pkg"}}},
	}
	srv := httptest.NewServer(diagnostics.Handler(state))
	defer srv.Close()
	for _, path := range []string{"/healthz", "/v1/summary", "/v1/packages?dialogue=d", "/v1/frames?from=0&to=5"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.StatusCode)
		}
		var v any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		resp.Body.Close()
	}
}

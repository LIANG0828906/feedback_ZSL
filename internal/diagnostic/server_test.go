package diagnostic_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"presentkernel/internal/diagnostic"
)

func TestDiagnosticPageAndState(t *testing.T) {
	srv, err := diagnostic.NewServer(filepath.Join(t.TempDir(), "diagnostic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health=%d", res.StatusCode)
	}

	res, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page=%d", res.StatusCode)
	}

	res, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state diagnostic.State
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Verification == nil || !state.Verification.OK {
		t.Fatalf("verification not ok: %+v", state.Verification)
	}
	for _, c := range state.CrossVersion {
		if !c.OK {
			t.Fatalf("cross version %s not ok: %+v", c.Version, c.Mismatch)
		}
	}
	for _, f := range state.Faults {
		if !f.OK {
			t.Fatalf("fault %s not mapped: %+v", f.Name, f.Error)
		}
	}
	if state.CheckpointStatus == nil || state.CheckpointStatus.RunRows == 0 {
		t.Fatal("expected checkpoint status rows")
	}
}

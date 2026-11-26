package maskqa

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHTTPLifecycle(t *testing.T) {
	server, err := NewHTTPServer(filepath.Join(t.TempDir(), "web"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	indexResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer indexResp.Body.Close()
	if indexResp.StatusCode != http.StatusOK {
		t.Fatalf("index status %d", indexResp.StatusCode)
	}

	stateResp, err := http.Get(ts.URL + "/api/state?sample=0")
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	var state stateResponse
	if err := json.NewDecoder(stateResp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.AccumulatedPix) == 0 || state.Result == nil {
		t.Fatal("state missing pixels/result")
	}

	body := []byte(`{"sample_index":0,"stroke_id":"stroke-h","weight_permyriad":7700}`)
	editResp, err := http.Post(ts.URL+"/api/edit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer editResp.Body.Close()
	var edited stateResponse
	if err := json.NewDecoder(editResp.Body).Decode(&edited); err != nil {
		t.Fatal(err)
	}
	if edited.Result == nil || edited.Result.Invalidation == nil {
		t.Fatal("edit response missing invalidation")
	}
	if len(edited.Result.RecomputedTiles) == 0 || len(edited.Result.ReusedTiles) == 0 {
		t.Fatal("expected both recomputed and reused tiles")
	}

	exportResp, err := http.Get(ts.URL + "/api/export?sample=0&schema=v2")
	if err != nil {
		t.Fatal(err)
	}
	defer exportResp.Body.Close()
	if exportResp.StatusCode != http.StatusOK {
		t.Fatalf("export status %d", exportResp.StatusCode)
	}
	ts.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewHTTPServer(server.baseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	ts2 := httptest.NewServer(restarted.Handler())
	defer ts2.Close()
	restartResp, err := http.Get(ts2.URL + "/api/state?sample=0")
	if err != nil {
		t.Fatal(err)
	}
	defer restartResp.Body.Close()
	var restartedState stateResponse
	if err := json.NewDecoder(restartResp.Body).Decode(&restartedState); err != nil {
		t.Fatal(err)
	}
	var weight int
	for _, stroke := range restartedState.Project.Strokes {
		if stroke.ID == "stroke-h" {
			weight = stroke.WeightPermyriad
		}
	}
	if weight != 7700 {
		t.Fatalf("restarted weight = %d, want 7700", weight)
	}
	if restartedState.Recovery.LastSequence == 0 {
		t.Fatal("expected checkpoint recovery after restart")
	}
}

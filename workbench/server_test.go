package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"loudnessworkbench/samplegen"
)

func TestStateEndpointAndStaticUI(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := samplegen.Generate(dataDir, 12345); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(dataDir, "v3")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d", response.StatusCode)
	}
	var state stateResponse
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Version != "v3" || len(state.Segments) != 6 {
		t.Fatalf("unexpected state: %+v", state)
	}
	if len(state.Interoperability) != 3 {
		t.Fatalf("interop pairs = %d", len(state.Interoperability))
	}
	index, err := http.Get(httpServer.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Body.Close()
	if index.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", index.StatusCode)
	}
}

package silhouette

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPServerRunAndCompare(t *testing.T) {
	server := httptest.NewServer(NewHTTPHandler())
	defer server.Close()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("page status=%d", response.StatusCode)
	}
	var page bytes.Buffer
	_, _ = page.ReadFrom(response.Body)
	if !strings.Contains(page.String(), "三路径比对") {
		t.Fatal("page controls missing")
	}
	body, _ := json.Marshal(RunRequest{Seed: DefaultSeed, Version: 3, Tolerance: DefaultTolerance, Budget: 90, Workers: 2, VerticesPerWorker: DefaultVerticesPerWorker})
	runResp, err := http.Post(server.URL+"/api/run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer runResp.Body.Close()
	if runResp.StatusCode != http.StatusOK {
		t.Fatalf("run status=%d", runResp.StatusCode)
	}
	var run RunResponse
	if err := json.NewDecoder(runResp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.Run == nil || run.Run.Assembly == nil || len(run.Run.Assembly.Tiles) != 4 {
		t.Fatalf("unexpected run response: %+v", run)
	}
	compareResp, err := http.Post(server.URL+"/api/compare", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer compareResp.Body.Close()
	if compareResp.StatusCode != http.StatusOK {
		t.Fatalf("compare status=%d", compareResp.StatusCode)
	}
	var compared struct {
		Comparison ConsistencyReport `json:"comparison"`
	}
	if err := json.NewDecoder(compareResp.Body).Decode(&compared); err != nil {
		t.Fatal(err)
	}
	if !compared.Comparison.Consistent {
		t.Fatalf("three-path comparison failed: %+v", compared.Comparison)
	}
}

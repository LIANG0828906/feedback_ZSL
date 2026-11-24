package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"storyboardstudio/internal/api"
	"storyboardstudio/internal/sample"
	"storyboardstudio/internal/workbench"
)

func TestHTTPWorkbenchRunEditAndExport(t *testing.T) {
	wb, err := workbench.NewMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	project := sample.Generate(252002)
	if err := wb.AddProject(project); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.New(wb).Handler())
	defer server.Close()

	response, err := http.Post(server.URL+"/api/projects/"+project.ID+"/run", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d", response.StatusCode)
	}
	var run struct {
		Shots []json.RawMessage `json:"shots"`
	}
	if err := json.NewDecoder(response.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if len(run.Shots) == 0 {
		t.Fatal("run returned no shots")
	}

	checkpointBody := []byte(`{"session_id":"api-session","interrupt_at":2}`)
	response, err = http.Post(server.URL+"/api/projects/"+project.ID+"/checkpoint", "application/json", bytes.NewReader(checkpointBody))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status = %d", response.StatusCode)
	}
	response.Body.Close()

	resumeBody := []byte(`{"session_id":"api-session","interrupt_at":-1}`)
	response, err = http.Post(server.URL+"/api/projects/"+project.ID+"/resume", "application/json", bytes.NewReader(resumeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d", response.StatusCode)
	}
	var resumeResponse struct {
		Result struct {
			Shots []json.RawMessage `json:"shots"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&resumeResponse); err != nil {
		t.Fatal(err)
	}
	if len(resumeResponse.Result.Shots) != len(run.Shots) {
		t.Fatalf("resume shots = %d, want %d", len(resumeResponse.Result.Shots), len(run.Shots))
	}

	body := []byte(`{"node_id":"n06_epilogue","kind":"duration_ms","local_index":0,"duration_ms":1750}`)
	response, err = http.Post(server.URL+"/api/projects/"+project.ID+"/edit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("edit status = %d", response.StatusCode)
	}
	var editReport struct {
		Comparison struct {
			OK bool `json:"ok"`
		} `json:"comparison"`
		CacheMisses []int `json:"cache_misses"`
	}
	if err := json.NewDecoder(response.Body).Decode(&editReport); err != nil {
		t.Fatal(err)
	}
	if !editReport.Comparison.OK || len(editReport.CacheMisses) != 1 {
		t.Fatalf("bad edit report: %+v", editReport)
	}

	response, err = http.Get(server.URL + "/api/projects/" + project.ID + "/export?node_id=n00_world&version=1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", response.StatusCode)
	}
	var exported map[string]any
	if err := json.NewDecoder(response.Body).Decode(&exported); err != nil {
		t.Fatal(err)
	}
	if exported["schema"] != "storyboard/1.0" {
		t.Fatalf("unexpected export: %+v", exported["schema"])
	}
}

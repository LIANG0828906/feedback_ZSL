package httpui_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wardrobe/engine"
	"wardrobe/generator"
	"wardrobe/httpui"
)

func TestDiagnosticsAPIAndImmediateConflictCheck(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	engineUnderTest, err := engine.New(bundle.Projects, engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpui.New(engineUnderTest).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Wardrobe Batch Diagnostics") {
		t.Fatalf("unexpected index response: %d %s", response.StatusCode, body)
	}

	project := bundle.Projects[0]
	truthCase := project.Truth.Cases[0]
	response, err = http.Get(server.URL + "/api/diagnostics?project=" + project.ProjectID + "&case=" + truthCase.CaseID)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics httpui.Diagnostics
	if err := json.NewDecoder(response.Body).Decode(&diagnostics); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("diagnostics status=%d", response.StatusCode)
	}
	if diagnostics.TruthMismatch != nil {
		t.Fatal(diagnostics.TruthMismatch)
	}
	if len(diagnostics.Graph.Nodes) == 0 || len(diagnostics.Result.Batches) == 0 {
		t.Fatal("diagnostics did not include graph or batches")
	}

	conflictCase := project.Truth.Cases[2]
	payload, _ := json.Marshal(map[string]any{
		"projectId":       project.ProjectID,
		"selectedPartIds": conflictCase.SelectedPartIDs,
	})
	response, err = http.Post(server.URL+"/api/validate-assembly", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var assemblyResult struct {
		Valid     bool `json:"valid"`
		Conflicts []struct {
			Category string `json:"category"`
		} `json:"conflicts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&assemblyResult); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if assemblyResult.Valid || len(assemblyResult.Conflicts) == 0 {
		t.Fatalf("expected immediate conflicts: %+v", assemblyResult)
	}
	categories := make(map[string]bool)
	for _, conflict := range assemblyResult.Conflicts {
		categories[conflict.Category] = true
	}
	if !categories["mount_contention"] || !categories["explicit_exclusion"] || !categories["skin_incompatible"] {
		t.Fatalf("missing conflict category: %+v", categories)
	}
}

func TestTriCompareAndBatchRetryAPI(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	engineUnderTest, err := engine.New(bundle.Projects, engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpui.New(engineUnderTest).Handler())
	defer server.Close()
	project := bundle.Projects[1]
	truthCase := project.Truth.Cases[0]

	payload, _ := json.Marshal(engine.EditRequest{
		ProjectID: project.ProjectID, CaseID: truthCase.CaseID,
		PartID: "hand_r_glove", ScalePercent: 90,
	})
	response, err := http.Post(server.URL+"/api/tri-compare", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var tri engine.TriCompareReport
	if err := json.NewDecoder(response.Body).Decode(&tri); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !tri.Equal {
		t.Fatalf("tri compare failed: %+v", tri.FirstMismatch)
	}

	response, err = http.Get(server.URL + "/api/diagnostics?project=" + project.ProjectID + "&case=" + truthCase.CaseID)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	retryPayload, _ := json.Marshal(map[string]string{
		"projectId": project.ProjectID, "caseId": truthCase.CaseID, "batchId": "B001",
	})
	response, err = http.Post(server.URL+"/api/retry-batch", "application/json", bytes.NewReader(retryPayload))
	if err != nil {
		t.Fatal(err)
	}
	var batch struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&batch); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || batch.ID != "B001" {
		t.Fatalf("retry status=%d batch=%+v", response.StatusCode, batch)
	}
}

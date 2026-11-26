package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dialogueforge/dialogue"
)

func TestWorkbenchStateAndEditEndpoints(t *testing.T) {
	project, err := dialogue.GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := dialogue.NewPipeline(project, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Build()
	if err != nil {
		t.Fatal(err)
	}
	wb := &workbench{pipeline: pipeline, result: result}

	stateRecorder := httptest.NewRecorder()
	wb.handleState(stateRecorder, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if stateRecorder.Code != http.StatusOK {
		t.Fatalf("state status %d", stateRecorder.Code)
	}
	var state stateView
	if err := json.Unmarshal(stateRecorder.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Progress.Committed != 4 || len(state.Segments) != 4 || len(state.Segments[0].Envelope) == 0 {
		t.Fatalf("bad state %+v", state)
	}

	editRecorder := httptest.NewRecorder()
	editRequest := httptest.NewRequest(http.MethodPost, "/api/edit", strings.NewReader(`{"kind":"segment-gain","segment_id":"seg-02","value":9900}`))
	wb.handleEdit(editRecorder, editRequest)
	if editRecorder.Code != http.StatusOK {
		t.Fatalf("edit status %d body=%s", editRecorder.Code, editRecorder.Body.String())
	}
	var edited stateView
	if err := json.Unmarshal(editRecorder.Body.Bytes(), &edited); err != nil {
		t.Fatal(err)
	}
	if edited.Invalidation == nil || len(edited.Invalidation.Invalidated) != 1 || edited.Segments[1].SessionGain != 9900 {
		t.Fatalf("edit not reflected: %+v", edited)
	}
	if !strings.Contains(pageHTML, "<canvas") || !strings.Contains(pageHTML, "/api/state") {
		t.Fatal("diagnostic page missing core UI")
	}
}

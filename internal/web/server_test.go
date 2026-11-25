package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lipsync/internal/web"
)

func TestDiagnosticServerRoutes(t *testing.T) {
	server, err := web.NewServer(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(html, "口型包时序") {
		t.Fatalf("index page status=%d containsTitle=%v", resp.StatusCode, strings.Contains(html, "口型包时序"))
	}

	resp, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	err = json.NewDecoder(resp.Body).Decode(&state)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if state["result"] == nil || state["project"] == nil {
		t.Fatalf("state missing payload: %+v", state)
	}

	resp, err = http.Post(ts.URL+"/api/inject", "application/json", strings.NewReader(`{"kind":"order"}`))
	if err != nil {
		t.Fatal(err)
	}
	var errorResponse struct {
		Error struct {
			Code       string `json:"code"`
			DialogueID string `json:"dialogueId"`
			ShardIndex int    `json:"shardIndex"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errorResponse)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || errorResponse.Error.Code != "E_ORDER" ||
		errorResponse.Error.DialogueID == "" {
		t.Fatalf("unexpected injection response status=%d body=%+v", resp.StatusCode, errorResponse)
	}
}

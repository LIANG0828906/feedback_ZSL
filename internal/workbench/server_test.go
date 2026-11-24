package workbench_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"economycore/internal/samples"
	"economycore/internal/workbench"
)

func TestWorkbenchHTTPFlow(t *testing.T) {
	dir := t.TempDir()
	if _, err := samples.Generate(dir, 5, []samples.Profile{{
		ID: "http", Intents: 120, Horizon: 240, TruthSamples: 5,
	}}); err != nil {
		t.Fatal(err)
	}
	server, err := workbench.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	check := func(method, path string, body any, want int) []byte {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(data)
		} else {
			reader = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, ts.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(res.Body); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, res.StatusCode, want, buf.String())
		}
		return buf.Bytes()
	}

	check(http.MethodGet, "/", nil, http.StatusOK)
	check(http.MethodGet, "/api/health", nil, http.StatusOK)
	check(http.MethodGet, "/api/samples", nil, http.StatusOK)
	raw := check(http.MethodPost, "/api/sessions", map[string]any{"sample_id": "http"}, http.StatusCreated)
	var view struct {
		SessionID string `json:"session_id"`
		Nodes     []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.SessionID == "" || len(view.Nodes) == 0 {
		t.Fatalf("bad session response: %s", raw)
	}
	check(http.MethodGet, "/api/sessions/"+view.SessionID, nil, http.StatusOK)
	check(http.MethodPost, "/api/sessions/"+view.SessionID+"/preview", map[string]any{
		"batch_id": "http-preview",
		"edits": []map[string]any{{
			"kind": "node_workers", "node_id": view.Nodes[0].ID, "workers": 0,
		}},
	}, http.StatusOK)
	badBody := check(http.MethodPost, "/api/sessions/"+view.SessionID+"/commit", map[string]any{
		"batch_id": "http-bad",
		"edits": []map[string]any{{
			"kind": "node_workers", "node_id": view.Nodes[0].ID, "workers": 9999,
		}},
	}, http.StatusConflict)
	var errBody struct {
		Code    string `json:"code"`
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal(badBody, &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Code != "E_TXN" || errBody.BatchID != "http-bad" {
		t.Fatalf("bad HTTP error envelope: %s", badBody)
	}
}

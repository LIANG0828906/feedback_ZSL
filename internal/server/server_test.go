package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"example.com/dualring-config/internal/model"
	"example.com/dualring-config/internal/store"
)

func TestHTTPContractAndBusinessCodes(t *testing.T) {
	configStore, err := store.Open(filepath.Join(t.TempDir(), "server.db"), store.Options{
		CanaryPercent: 10,
		HistoryLimit:  20,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer configStore.Close()
	api := New(configStore, log.New(&bytes.Buffer{}, "", 0), 50_000_000)
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	bootstrap := map[string]any{
		"namespace": "payments",
		"operator":  "test",
		"items": map[string]string{
			"timeout": "100ms",
			"limit":   "10",
		},
	}
	status, _ := requestJSON(t, httpServer.URL+"/v1/admin/bootstrap", http.MethodPost, bootstrap)
	if status != http.StatusCreated {
		t.Fatalf("bootstrap status=%d", status)
	}
	update := map[string]any{
		"namespace":        "payments",
		"key":              "timeout",
		"value":            "200ms",
		"expected_version": 1,
		"operator":         "test",
	}
	status, _ = requestJSON(t, httpServer.URL+"/v1/admin/items", http.MethodPut, update)
	if status != http.StatusOK {
		t.Fatalf("update status=%d", status)
	}
	status, body := requestJSON(t, httpServer.URL+"/v1/admin/items", http.MethodPut, update)
	if status != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", status, body)
	}
	var conflict model.BusinessError
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if conflict.Code != model.CodeConflict {
		t.Fatalf("conflict code=%s", conflict.Code)
	}

	pullURL := httpServer.URL + "/v1/config/pull?instance_id=canary-1&namespace=payments&local_version=0"
	status, body = requestJSON(t, pullURL, http.MethodGet, nil)
	if status != http.StatusOK {
		t.Fatalf("pull status=%d body=%s", status, body)
	}
	var pull model.PullResponse
	if err := json.Unmarshal(body, &pull); err != nil {
		t.Fatalf("decode pull: %v", err)
	}
	if pull.Version <= 0 || pull.Ring == "" || pull.Digest != model.DigestItems(pull.Items) {
		t.Fatalf("invalid pull response: %+v", pull)
	}

	releaseBody := map[string]any{
		"namespace": "payments",
		"operator":  "test",
		"changes": []map[string]any{
			{"key": "timeout", "value": "300ms"},
		},
		"observation_seconds": 1,
	}
	status, body = requestJSON(t, httpServer.URL+"/v1/releases", http.MethodPost, releaseBody)
	if status != http.StatusCreated {
		t.Fatalf("create release status=%d body=%s", status, body)
	}
	var release model.Release
	if err := json.Unmarshal(body, &release); err != nil {
		t.Fatalf("decode release: %v", err)
	}
	transitionBody := map[string]any{"target": "full", "operator": "test"}
	status, body = requestJSON(t,
		httpServer.URL+"/v1/releases/"+release.ID+"/transition", http.MethodPost, transitionBody)
	if status != http.StatusConflict {
		t.Fatalf("invalid transition status=%d body=%s", status, body)
	}
	var invalid model.BusinessError
	if err := json.Unmarshal(body, &invalid); err != nil {
		t.Fatalf("decode invalid transition: %v", err)
	}
	if invalid.Code != model.CodeInvalidTransition {
		t.Fatalf("invalid transition code=%s", invalid.Code)
	}
}

func requestJSON(t *testing.T, endpoint, method string, payload any) (int, []byte) {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer response.Body.Close()
	var output bytes.Buffer
	if _, err := output.ReadFrom(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, output.Bytes()
}

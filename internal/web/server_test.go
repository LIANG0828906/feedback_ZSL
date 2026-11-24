package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"roomworkbench/internal/wb"
	"roomworkbench/internal/web"
)

func TestHTTPEndpointsAndErrorEnvelope(t *testing.T) {
	engine, apiErr := wb.NewEngine(wb.GenerateSample(wb.DefaultSeed), nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, apiErr = engine.RunFull(context.Background(), true); apiErr != nil {
		t.Fatal(apiErr)
	}
	server := web.NewServer(engine, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status %d", resp.StatusCode)
	}
	var state wb.StateView
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Progress.Committed != 4 {
		t.Fatalf("progress %+v", state.Progress)
	}

	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("房间混响区划工作台")) {
		t.Fatalf("index page status=%d bytes=%d", resp.StatusCode, len(body))
	}

	for _, tc := range []struct {
		body       string
		wantStatus int
		wantCode   string
	}{
		{`{"type":"source_out_of_bounds"}`, http.StatusBadRequest, wb.CodeSchema},
		{`{"type":"door_nonadjacent"}`, http.StatusUnprocessableEntity, wb.CodeGeom},
		{`{"type":"room_failure","target":"R3"}`, http.StatusConflict, wb.CodeState},
	} {
		status, postBody := postJSON(t, ts.URL+"/api/inject", tc.body)
		if status != tc.wantStatus {
			t.Fatalf("inject %s status=%d body=%s", tc.body, status, postBody)
		}
		var envelope struct {
			Error *wb.APIError `json:"error"`
		}
		if err := json.Unmarshal(postBody, &envelope); err != nil || envelope.Error == nil || envelope.Error.Code != tc.wantCode {
			t.Fatalf("inject %s envelope=%s err=%v", tc.body, postBody, err)
		}
	}

	if _, body := postJSON(t, ts.URL+"/api/recompute/resume", `{}`); !bytes.Contains(body, []byte(`"ok":true`)) {
		t.Fatalf("resume response = %s", body)
	}
	if status, body := postJSON(t, ts.URL+"/api/compare", `{}`); status != http.StatusOK || !bytes.Contains(body, []byte(`"match":true`)) {
		t.Fatalf("compare status=%d body=%s", status, body)
	}
}

func postJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

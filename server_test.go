package terrainbake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	demo, err := NewDemo(t.TempDir(), DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(demo)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func getJSON(t *testing.T, url string, dst any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func postJSON(t *testing.T, url string, body string, dst any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("decode response: %v body=%s", err, data)
		}
	}
	return resp.StatusCode
}

func TestHTTPPageStateEditVerifyAndSchemas(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(html), "分块着色网格诊断台") {
		t.Fatalf("page status=%d", resp.StatusCode)
	}

	var state StateView
	if code := getJSON(t, ts.URL+"/api/state", &state); code != 200 {
		t.Fatalf("state status=%d", code)
	}
	if len(state.Blocks) != 6 || len(state.Meshes) != 6 || len(state.SeamReports) != 7 {
		t.Fatalf("state blocks=%d meshes=%d seams=%d", len(state.Blocks), len(state.Meshes), len(state.SeamReports))
	}

	var edited StateView
	body := `{"block_id":"B02","height_delta_mm":120}`
	if code := postJSON(t, ts.URL+"/api/edit", body, &edited); code != 200 {
		t.Fatalf("edit status=%d", code)
	}
	if edited.ReusedCount != 3 {
		t.Fatalf("edit reused = %d (%v)", edited.ReusedCount, edited.Reused)
	}

	var report ThreePathReport
	if code := postJSON(t, ts.URL+"/api/verify", `{}`, &report); code != 200 {
		t.Fatalf("verify status=%d", code)
	}
	if !report.Equal {
		t.Fatalf("verify report = %+v", report)
	}

	var frame CameraFrame
	if code := getJSON(t, ts.URL+"/api/frame?t_ms=1500", &frame); code != 200 {
		t.Fatalf("frame status=%d", code)
	}
	if frame.Sample.TimeMS < 0 || frame.Sample.Focus == "" {
		t.Fatalf("frame = %+v", frame)
	}

	for _, v := range []string{"1", "2", "3"} {
		resp, err := http.Get(ts.URL + "/api/schema?v=" + v)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !json.Valid(data) {
			t.Fatalf("schema v%s status=%d valid=%v", v, resp.StatusCode, json.Valid(data))
		}
	}
}

func TestHTTPFaultCodes(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct {
		kind string
		code string
	}{
		{"decode", "E_DECODE"},
		{"overlap", "E_OVER"},
		{"adjacency", "E_ADJ"},
		{"block", "E_BLOCK"},
	}
	for _, tc := range cases {
		var payload struct {
			State StateView `json:"state"`
			Error *Error    `json:"error"`
		}
		body := `{"kind":"` + tc.kind + `","block_id":"B02"}`
		status := postJSON(t, ts.URL+"/api/fault", body, &payload)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s status=%d", tc.kind, status)
		}
		if payload.Error == nil || string(payload.Error.Code) != tc.code {
			t.Fatalf("%s error = %+v", tc.kind, payload.Error)
		}
		if tc.kind != "block" && payload.Error.Path == "" {
			t.Fatalf("%s lost field path: %+v", tc.kind, payload.Error)
		}
		if tc.kind == "block" {
			if payload.State.BlockErrors["B02"] == nil {
				t.Fatalf("block fault state missing B02 error")
			}
			if len(payload.State.Meshes) != 5 {
				t.Fatalf("block fault left %d committed meshes", len(payload.State.Meshes))
			}
		}
	}
}

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"stagepreview/stage"
)

func TestDiagnosticPageWorkflow(t *testing.T) {
	samples := t.TempDir()
	runtimeDir := t.TempDir()
	srv, err := New(samples, runtimeDir, 42)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	body := serveRequest(t, handler, http.MethodGet, "/", nil)
	if !strings.Contains(string(body), "舞台灯具阵列") {
		t.Fatalf("index body=%s", body[:min(len(body), 200)])
	}
	if !strings.Contains(string(body), `id="beam-preview"`) {
		t.Fatal("index page missing preview canvas")
	}

	form := url.Values{
		"id": {"F1"}, "pan_mdeg": {"5"}, "tilt_mdeg": {"-89"},
		"inner_mdeg": {"8000"}, "outer_mdeg": {"20000"},
		"level_permyriad": {"5000"}, "cct_kelvin": {"4000"},
		"r": {"10000"}, "g": {"8000"}, "b": {"6000"},
	}
	postStatus(t, handler, "/fixture", form)
	if _, err := os.Stat(runtimeDir + "/current-v3.json"); err != nil {
		t.Fatalf("edited project not persisted: %v", err)
	}
	liveBody := serveRequest(t, handler, http.MethodPost, "/api/live-preview", form)
	var live struct {
		OK         bool                    `json:"ok"`
		Sampling   stage.SamplingConfig    `json:"sampling"`
		Statistics stage.PreviewStatistics `json:"statistics"`
		Points     []json.RawMessage       `json:"visual_points"`
	}
	if err := json.Unmarshal(liveBody, &live); err != nil {
		t.Fatal(err)
	}
	if !live.OK || live.Sampling.Density != 2 || live.Statistics.PointCount == 0 || len(live.Points) == 0 {
		t.Fatalf("bad live preview: %+v", live)
	}

	postStatus(t, handler, "/preview", url.Values{})
	body = serveRequest(t, handler, http.MethodGet, "/", nil)
	if !strings.Contains(string(body), "低密度预览证据") {
		t.Fatalf("preview page missing evidence block: %s", body)
	}
	if !strings.Contains(string(body), "共享点比对") {
		t.Fatalf("preview page missing comparison block: %s", body[max(0, len(body)-1200):])
	}

	postStatus(t, handler, "/exact", url.Values{})
	entries, err := os.ReadDir(runtimeDir + "/checkpoints")
	if err != nil || len(entries) != 5 {
		t.Fatalf("checkpoints: entries=%d err=%v", len(entries), err)
	}
	postStatus(t, handler, "/verify", url.Values{})
	body = serveRequest(t, handler, http.MethodGet, "/", nil)
	if !strings.Contains(string(body), "三路径一致性") {
		t.Fatalf("verify page missing three-path block")
	}

	statusBody := serveRequest(t, handler, http.MethodGet, "/api/status", nil)
	var status map[string]any
	if err := json.Unmarshal(statusBody, &status); err != nil {
		t.Fatal(err)
	}
	if status["preview"] == nil || status["preview_comparison"] == nil {
		t.Fatalf("status missing preview evidence: %+v", status)
	}
}

func serveRequest(t *testing.T, handler http.Handler, method, path string, form url.Values) []byte {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s %s status=%d body=%s", method, path, rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

func postStatus(t *testing.T, handler http.Handler, path string, form url.Values) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST %s status=%d body=%s", path, rr.Code, rr.Body.String())
	}
}

func TestListenRejectsNonLoopback(t *testing.T) {
	srv := &Server{}
	if err := Listen("0.0.0.0:0", srv); err == nil {
		t.Fatal("expected non-loopback bind to be rejected")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

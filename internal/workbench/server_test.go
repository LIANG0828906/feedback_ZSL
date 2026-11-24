package workbench

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gameintegrity/internal/model"
	"gameintegrity/internal/sample"
)

func setupServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	spec := sample.Spec{
		ID: "tiny", Width: 80, Height: 64, Channels: 3, FrameCount: 16,
		BlockSize: 8, Replacement: model.Interval{Start: 6, End: 8},
		SegmentSeconds: 1, Tolerance: 1,
	}
	if err := sample.WriteSample(root, spec, 20261124); err != nil {
		t.Fatal(err)
	}
	server := New(root, filepath.Join(t.TempDir(), "work"))
	return httptest.NewServer(server.Handler())
}

func getJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

func TestWorkbenchHTTPFlow(t *testing.T) {
	ts := setupServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "录屏内容完整性核验台") {
		t.Fatalf("index status=%d", resp.StatusCode)
	}

	var samples map[string]any
	resp = getJSON(t, ts.URL+"/api/samples", &samples)
	if resp.StatusCode != 200 {
		t.Fatalf("samples status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, suffix := range []string{"/api/samples/tiny/frame/6.png", "/api/samples/tiny/residual/6.png", "/api/samples/tiny/noise/6.png"} {
		resp, err = http.Get(ts.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/png") || len(data) < 8 {
			t.Fatalf("%s status=%d type=%s bytes=%d", suffix, resp.StatusCode, resp.Header.Get("Content-Type"), len(data))
		}
	}

	resp, err = http.Post(ts.URL+"/api/samples/tiny/verify?mode=fine&fresh=1", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("verify status=%d", resp.StatusCode)
	}
	var job JobView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp = getJSON(t, ts.URL+"/api/samples/tiny/job", &job)
		resp.Body.Close()
		if job.Status == jobCompleted || job.Status == jobFailed {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if job.Status != jobCompleted || job.Report == nil {
		t.Fatalf("job status=%s err=%v", job.Status, job.Error)
	}
	if !job.Report.Replacement.WithinTolerance {
		t.Fatalf("replacement = %+v", job.Report.Replacement)
	}

	var segment model.SegmentResult
	resp = getJSON(t, ts.URL+"/api/samples/tiny/segment/0", &segment)
	resp.Body.Close()
	if segment.Index != 0 || len(segment.Frames) == 0 {
		t.Fatalf("segment = %+v", segment)
	}
	resp, err = http.Get(ts.URL + "/api/samples/tiny/export")
	if err != nil {
		t.Fatal(err)
	}
	exportBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export status=%d", resp.StatusCode)
	}
	if !json.Valid(exportBody) {
		t.Fatal("export is not valid JSON")
	}
}

func TestWorkbenchMissingSampleReturnsErrorEnvelope(t *testing.T) {
	ts := setupServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/samples/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "E_DECODE" {
		t.Fatalf("body=%v", body)
	}
}

func TestWorkbenchRejectsInvalidBlockBeforeStartingJob(t *testing.T) {
	ts := setupServer(t)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/samples/tiny/verify?block=7", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "E_PARA" || errObj["field"] != "block_size" {
		t.Fatalf("body=%v", body)
	}
}

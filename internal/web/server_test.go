package web_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"spectralforge/internal/sample"
	"spectralforge/internal/verify"
	"spectralforge/internal/web"
)

func TestLocalPagePreviewExactAndVerify(t *testing.T) {
	sampleDir := t.TempDir()
	b, err := sample.Generate(sampleDir, 8080)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	server := web.NewServer(b, filepath.Join(root, "cp"), filepath.Join(root, "out"), "127.0.0.1:0")
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("SpectralForge")) {
		t.Fatalf("index failed: %d", resp.StatusCode)
	}

	exact := postSummary(t, ts.URL+"/api/exact")
	if !exact.Completed || !exact.TruthPass || exact.Version == "" {
		t.Fatalf("exact failed: %+v", exact)
	}
	imageResp, err := http.Get(ts.URL + "/api/image.png?kind=exact")
	if err != nil {
		t.Fatal(err)
	}
	imageResp.Body.Close()
	if imageResp.StatusCode != 200 || imageResp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image endpoint: status=%d type=%s", imageResp.StatusCode, imageResp.Header.Get("Content-Type"))
	}

	verifyResp, err := http.Post(ts.URL+"/api/verify", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifyResp.Body.Close()
	var vr verify.Report
	if err := json.NewDecoder(verifyResp.Body).Decode(&vr); err != nil {
		t.Fatal(err)
	}
	if !vr.OK || !vr.CommitOrderOK {
		t.Fatalf("verify failed: %+v", vr)
	}

	cfg := *b.Config
	cfg.Exposure += 0.1
	payload, _ := json.Marshal(cfg)
	resp, err = http.Post(ts.URL+"/api/params", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var snap web.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if snap.Version == exact.Version {
		t.Fatal("parameter snapshot version did not change")
	}
	preview := postSummary(t, ts.URL+"/api/preview")
	if preview.Version != snap.Version || !preview.Completed {
		t.Fatalf("preview did not use latest snapshot: %+v", preview)
	}

	cfg.Exposure += 0.1
	payload, _ = json.Marshal(cfg)
	resp, err = http.Post(ts.URL+"/api/params", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	retone := postSummary(t, ts.URL+"/api/preview")
	if retone.TilesRendered != 0 || retone.TilesReused != retone.TilesTotal || !retone.Completed {
		t.Fatalf("display-only preview recomputed blocks: %+v", retone)
	}
}

func postSummary(t *testing.T, url string) *web.RunSummary {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status=%d body=%s", url, resp.StatusCode, data)
	}
	var summary web.RunSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	return &summary
}

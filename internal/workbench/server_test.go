package workbench_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"volumekernel/internal/model"
	"volumekernel/internal/workbench"
)

func TestWorkbenchHTTPEndpoints(t *testing.T) {
	srv, err := workbench.New(workbench.Options{
		Seed: 20261127, CameraIndex: 2, DBPath: filepath.Join(t.TempDir(), "workbench.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stateBody struct {
		OK    bool `json:"ok"`
		State struct {
			Media   []map[string]any `json:"media"`
			Preview map[string]any   `json:"preview"`
			Exact   map[string]any   `json:"exact"`
			Compare map[string]any   `json:"compare"`
		} `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stateBody); err != nil {
		t.Fatal(err)
	}
	if !stateBody.OK || len(stateBody.State.Media) == 0 {
		t.Fatalf("bad state: %+v", stateBody)
	}
	if stateBody.State.Exact["blocks"] == nil {
		t.Fatal("state does not include exact blocks")
	}

	resp, err = http.Get(ts.URL + "/preview.png?mode=exact")
	if err != nil {
		t.Fatal(err)
	}
	pngBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(pngBytes) < 8 || string(pngBytes[1:4]) != "PNG" {
		t.Fatalf("expected PNG, status=%d bytes=%d", resp.StatusCode, len(pngBytes))
	}

	body := httpGet(t, ts.URL+"/api/render?mode=preview&steps=8&shadow=2")
	var renderBody struct {
		OK    bool           `json:"ok"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &renderBody); err != nil {
		t.Fatal(err)
	}
	if !renderBody.OK {
		t.Fatalf("preview render failed: %s", body)
	}

	body = httpGet(t, ts.URL+"/api/inject?kind=decode")
	var injectBody struct {
		Error struct {
			Code     model.Code `json:"code"`
			MediumID string     `json:"medium_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &injectBody); err != nil {
		t.Fatal(err)
	}
	if injectBody.Error.Code != model.EDecode || injectBody.Error.MediumID == "" {
		t.Fatalf("unexpected decode injection: %s", body)
	}

	body = httpGet(t, ts.URL+"/api/edit?medium=media-beta&x=2&y=2&z=2")
	var editBody struct {
		OK           bool `json:"ok"`
		Invalidation struct {
			Affected         []string `json:"affected_blocks"`
			Unaffected       int      `json:"unaffected_blocks"`
			CacheHits        int      `json:"cache_hits_after_edit"`
			CacheMisses      int      `json:"cache_misses_after_edit"`
			CacheRowsDeleted int64    `json:"cache_rows_deleted"`
		} `json:"invalidation"`
	}
	if err := json.Unmarshal(body, &editBody); err != nil {
		t.Fatal(err)
	}
	if !editBody.OK || len(editBody.Invalidation.Affected) == 0 ||
		editBody.Invalidation.CacheHits != editBody.Invalidation.Unaffected ||
		editBody.Invalidation.CacheMisses != len(editBody.Invalidation.Affected) {
		t.Fatalf("bad invalidation response: %s", body)
	}
}

func httpGet(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b
}

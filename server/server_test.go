package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terrain-bake/bake"
	"terrain-bake/internal/testutil"
	"terrain-bake/project"
	"terrain-bake/server"
)

func setupHTTP(t *testing.T) (*httptest.Server, *project.Project, *bake.Manager) {
	t.Helper()
	dataDir := testutil.CopyProject(t)
	p, e := project.Load(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	base := t.TempDir()
	m, e := bake.NewManager(p, g, bake.ManagerOptions{
		CacheDir:  filepath.Join(base, "cache"),
		OutputDir: filepath.Join(base, "build"),
		Params:    bake.ParamsFromManifest(p.Manifest),
	})
	if e != nil {
		t.Fatal(e)
	}
	if _, e := m.BakeAll(context.Background(), false); e != nil {
		t.Fatal(e)
	}
	ts := httptest.NewServer(server.New(m).Handler())
	t.Cleanup(ts.Close)
	return ts, p, m
}

func TestDiagnosticPageAndPreview(t *testing.T) {
	ts, _, _ := setupHTTP(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status %d: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, needle := range []string{"Terrain Bake Workbench", "B_0_0", "Invalidation Graph", "Cache &amp; Timing", "Apply edit"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("page missing %q", needle)
		}
	}
	resp, err = http.Get(ts.URL + "/preview/B_0_0.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("preview is not a PNG: status=%d len=%d", resp.StatusCode, len(data))
	}
}

func TestStatusAndInvalidationAPI(t *testing.T) {
	ts, _, _ := setupHTTP(t)
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		OK     bool               `json:"ok"`
		Status bake.ManagerStatus `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.OK || !status.Status.CacheValid || status.Status.ArtifactCount != 6 {
		t.Fatalf("bad status API response: %+v", status)
	}

	resp, err = http.Get(ts.URL + "/api/invalidate?block=B_0_0&kind=height")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var inv struct {
		OK           bool                     `json:"ok"`
		Invalidation *bake.InvalidationReport `json:"invalidation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatal(err)
	}
	if !inv.OK || len(inv.Invalidation.Affected) != 4 || len(inv.Invalidation.HitBlocks) != 2 {
		t.Fatalf("bad invalidation API response: %+v", inv)
	}
}

func TestHTTPEditTriggersIncrementalAndPersistsOverlay(t *testing.T) {
	ts, p, m := setupHTTP(t)
	payload := []byte(`{"block":"B_0_0","kind":"height","col":10,"row":10,"value":3.5}`)
	resp, err := http.Post(ts.URL+"/api/edit", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool             `json:"ok"`
		Report *bake.BakeReport `json:"report"`
		Error  *project.Error   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Report == nil || len(result.Report.Affected) != 4 {
		t.Fatalf("edit response: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(p.Root, "edits.json")); err != nil {
		t.Fatalf("edit overlay not persisted: %v", err)
	}
	if m.Artifact("B_0_0").Positions[0].Y == 0 && m.Project().TruthStale == false {
		t.Fatal("edit did not update manager state")
	}
	if !m.Project().TruthStale {
		t.Fatal("truth should be stale after diagnostic edit")
	}
	diff, e := m.CompareToFull(context.Background())
	if e != nil || !diff.Equal {
		t.Fatalf("incremental path does not match full after HTTP edit: %+v %v", diff, e)
	}

	// The page retains the last invalidation report and should show the source
	// path and cache-hit island.
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "edit:height") || !strings.Contains(string(body), "B_3_0") {
		t.Fatal("page did not render last invalidation propagation")
	}
}

func TestCompareAndMethodGuards(t *testing.T) {
	ts, _, _ := setupHTTP(t)
	resp, err := http.Post(ts.URL+"/api/compare", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		OK   bool             `json:"ok"`
		Diff *bake.DiffReport `json:"diff"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Diff == nil || !result.Diff.Equal {
		t.Fatalf("compare response: %+v", result)
	}
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method guard status = %d", resp2.StatusCode)
	}
}

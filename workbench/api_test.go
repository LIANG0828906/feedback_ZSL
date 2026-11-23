package workbench_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"storyworkbench/config"
	"storyworkbench/workbench"
)

func newTestServer(t *testing.T) (*httptest.Server, *workbench.App) {
	t.Helper()
	cfg := config.Default()
	cfg.SamplesDir = "../samples"
	cfg.CheckpointDB = filepath.Join(t.TempDir(), "checkpoints.db")
	cfg.SavesDir = filepath.Join(t.TempDir(), "saves")
	cfg.ArtifactsDir = filepath.Join(t.TempDir(), "artifacts")
	cfg.Workers = 2
	cfg.BufferPerWorker = 2
	app, err := workbench.NewApp(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	server := httptest.NewServer(workbench.NewServer(app).Handler())
	t.Cleanup(func() {
		server.Close()
		_ = app.Close()
	})
	return server, app
}

func requestJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("decode response %q: %v", string(data), err)
		}
	}
	return resp.StatusCode, decoded
}

func firstProjectID(t *testing.T, server *httptest.Server) string {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, server.URL+"/api/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("projects status %d: %v", status, body)
	}
	projects := body["projects"].([]any)
	return projects[0].(map[string]any)["id"].(string)
}

func TestAPIBaselineCompareSaveAndCheckpointFlow(t *testing.T) {
	server, _ := newTestServer(t)
	projectID := firstProjectID(t, server)

	status, detail := requestJSON(t, http.MethodGet, server.URL+"/api/project?id="+projectID, nil)
	if status != http.StatusOK || detail["ok"] != true {
		t.Fatalf("detail failed: %d %v", status, detail)
	}
	detailBody := detail["detail"].(map[string]any)
	if detailBody["baseline"] == nil || detailBody["dependency_graph"] == nil {
		t.Fatal("project detail is missing baseline or graph")
	}
	shotCount := int(detailBody["baseline"].(map[string]any)["shot_count"].(float64))

	status, save := requestJSON(t, http.MethodPost, server.URL+"/api/saves", map[string]any{
		"project_id": projectID, "shot_boundary": shotCount / 3, "label": "api-test",
	})
	if status != http.StatusCreated || save["ok"] != true {
		t.Fatalf("create save failed: %d %v", status, save)
	}
	saveID := save["result"].(map[string]any)["record"].(map[string]any)["id"].(string)

	status, resumed := requestJSON(t, http.MethodPost, server.URL+"/api/resume", map[string]any{
		"project_id": projectID, "save_id": saveID,
	})
	if status != http.StatusOK || resumed["ok"] != true {
		t.Fatalf("resume failed: %d %v", status, resumed)
	}
	if int(resumed["run"].(map[string]any)["shot_count"].(float64)) != shotCount {
		t.Fatal("resumed run has the wrong shot count")
	}

	status, compare := requestJSON(t, http.MethodPost, server.URL+"/api/compare", map[string]any{
		"project_id": projectID,
		"edit": map[string]any{
			"node_id":  "archive",
			"requires": detailBody["project"].(map[string]any)["nodes"].([]any)[3].(map[string]any)["requires"],
			"events": append(
				detailBody["project"].(map[string]any)["nodes"].([]any)[3].(map[string]any)["events"].([]any),
				map[string]any{"id": "ev-api-test", "kind": "set_flag", "key": "api_test", "value": true},
			),
		},
		"save_boundary": shotCount / 2,
	})
	if status != http.StatusOK {
		t.Fatalf("compare status %d: %v", status, compare)
	}
	if compare["ok"] != true {
		t.Fatalf("three paths diverged: %v", compare)
	}
	if compare["artifact_dir"] == "" {
		t.Fatal("compare did not retain artifacts")
	}

	archiveNode := detailBody["project"].(map[string]any)["nodes"].([]any)[3].(map[string]any)
	gateNode := detailBody["project"].(map[string]any)["nodes"].([]any)[5].(map[string]any)
	status, preview := requestJSON(t, http.MethodPost, server.URL+"/api/edit", map[string]any{
		"project_id": projectID,
		"edit": map[string]any{
			"node_id":  "archive",
			"requires": archiveNode["requires"],
			"events": append(archiveNode["events"].([]any),
				map[string]any{"id": "ev-preview-api", "kind": "set_flag", "key": "preview_api", "value": true}),
		},
	})
	if status != http.StatusOK || preview["ok"] != true || preview["edited_dependency_graph"] == nil {
		t.Fatalf("edit preview failed: %d %v", status, preview)
	}
	status, sequenceCompare := requestJSON(t, http.MethodPost, server.URL+"/api/compare", map[string]any{
		"project_id": projectID,
		"edits": []any{
			map[string]any{
				"node_id":  "archive",
				"requires": archiveNode["requires"],
				"events": append(archiveNode["events"].([]any),
					map[string]any{"id": "ev-seq-api-1", "kind": "set_flag", "key": "seq_api_1", "value": true}),
			},
			map[string]any{
				"node_id":  "gate",
				"requires": gateNode["requires"],
				"events": append(gateNode["events"].([]any),
					map[string]any{"id": "ev-seq-api-2", "kind": "set_flag", "key": "seq_api_2", "value": true}),
			},
		},
		"save_boundary": shotCount / 2,
	})
	if status != http.StatusOK || sequenceCompare["ok"] != true {
		t.Fatalf("edit sequence compare failed: %d %v", status, sequenceCompare)
	}

	status, started := requestJSON(t, http.MethodPost, server.URL+"/api/checkpoints/start", map[string]any{"project_id": projectID})
	if status != http.StatusOK || started["ok"] != true {
		t.Fatalf("start checkpoints: %d %v", status, started)
	}
	status, restored := requestJSON(t, http.MethodPost, server.URL+"/api/checkpoints/resume", map[string]any{"project_id": projectID})
	if status != http.StatusOK || restored["ok"] != true {
		t.Fatalf("resume checkpoints: %d %v", status, restored)
	}

	status, saves := requestJSON(t, http.MethodGet, server.URL+"/api/saves?project_id="+projectID, nil)
	if status != http.StatusOK || len(saves["saves"].([]any)) != 1 {
		t.Fatalf("save list failed: %d %v", status, saves)
	}
}

func TestAPIServesEmbeddedUIAndRejectsBadRequests(t *testing.T) {
	server, _ := newTestServer(t)
	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte("Narrative Relay")) {
		t.Fatalf("embedded UI not served: %d", resp.StatusCode)
	}
	status, body := requestJSON(t, http.MethodPost, server.URL+"/api/run", map[string]any{"unknown": true})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown request field, got %d %v", status, body)
	}
}

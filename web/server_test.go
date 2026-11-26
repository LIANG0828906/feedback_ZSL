package web_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"maskqc/engine"
	"maskqc/model"
	"maskqc/sample"
	"maskqc/schema"
	"maskqc/web"
)

func TestDashboardAndAPIs(t *testing.T) {
	bundle, err := sample.Generate(8100, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	generated := bundle.Projects[0]
	project, _, err := schema.Load(generated.Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.New([]web.ProjectInput{{
		ID: project.ID, Project: project, IconPNG: generated.ImagePNG,
		Versions: []string{"v1", "v2", "v3"}, Lossy: map[string]int{"v1": 14, "v2": 11, "v3": 0},
	}}, engine.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	index := getText(t, httpServer.URL+"/")
	if !bytes.Contains([]byte(index), []byte("MaskQC")) || !bytes.Contains([]byte(index), []byte("逐笔覆盖分量")) {
		t.Fatal("dashboard HTML is incomplete")
	}
	notFound, err := http.Get(httpServer.URL + "/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	notFound.Body.Close()
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path returned %d instead of 404", notFound.StatusCode)
	}
	health := getJSON(t, httpServer.URL+"/healthz")
	if health["status"] != "ok" {
		t.Fatal("health endpoint failed")
	}
	projects := getJSON(t, httpServer.URL+"/api/projects")
	if len(projects["projects"].([]any)) != 1 {
		t.Fatal("project list is wrong")
	}
	state := getJSON(t, httpServer.URL+"/api/project/"+project.ID)
	domains := state["domains"].(map[string]any)["domains"].([]any)
	if len(domains) != 3 {
		t.Fatalf("expected 3 domains in API, got %d", len(domains))
	}
	budget := state["budget"].(map[string]any)
	if budget["maxParallel"] == nil {
		t.Fatal("budget details missing")
	}
	graph := state["graph"].(map[string]any)
	if len(graph["nodes"].([]any)) == 0 || len(graph["edges"].([]any)) == 0 {
		t.Fatal("dependency graph is empty")
	}
	for _, direction := range domains {
		if direction.(map[string]any)["direction"] == "" {
			t.Fatal("direction annotation missing")
		}
	}
	for _, path := range []string{
		"/img/" + project.ID + "/candidate.png",
		"/img/" + project.ID + "/accumulated.png",
		"/img/" + project.ID + "/icon.png",
		"/img/" + project.ID + "/stroke/s01",
		"/img/" + project.ID + "/truth/d001",
	} {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/png" ||
			!bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) {
			t.Fatalf("image endpoint failed: %s status=%d", path, response.StatusCode)
		}
	}
	body, _ := json.Marshal(map[string]any{"strokeId": "s01", "dx1": 3, "hardness": 82})
	response, err := http.Post(httpServer.URL+"/api/project/"+project.ID+"/edit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var editResult map[string]any
	if err := json.NewDecoder(response.Body).Decode(&editResult); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("edit failed: %v", editResult)
	}
	invalidation, ok := editResult["invalidation"].(map[string]any)
	if !ok || len(invalidation["invalidatedBlocks"].([]any)) == 0 {
		t.Fatalf("edit did not expose invalidation: %+v", editResult)
	}
	after := getJSON(t, httpServer.URL+"/api/project/"+project.ID)
	if after["issues"] == nil {
		t.Fatal("diagnostic issues are not exposed")
	}
}

func getText(t *testing.T, url string) string {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, data)
	}
	return string(data)
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	data := getText(t, url)
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

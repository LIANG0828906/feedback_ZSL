package workbench

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *WorkbenchError `json:"error"`
}

func serverCall(t *testing.T, client *http.Client, method, url string, body any) (int, testEnvelope) {
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env testEnvelope
	if len(data) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("decode response %q: %v", string(data), err)
		}
	}
	return resp.StatusCode, env
}

func TestHTTPWorkbenchEndToEnd(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerWithScenes(t.TempDir(), scene)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	status, env := serverCall(t, client, "GET", httpServer.URL+"/api/health", nil)
	if status != 200 || !env.OK {
		t.Fatalf("health status=%d env=%+v", status, env)
	}
	status, env = serverCall(t, client, "POST", httpServer.URL+"/api/scenes/"+scene.ID+"/step", nil)
	if status != 200 || !env.OK {
		t.Fatalf("step status=%d env=%+v", status, env)
	}
	status, env = serverCall(t, client, "POST", httpServer.URL+"/api/scenes/"+scene.ID+"/start", nil)
	if status != 200 || !env.OK {
		t.Fatalf("start status=%d env=%+v", status, env)
	}
	deadline := time.Now().Add(20 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		status, env = serverCall(t, client, "GET", httpServer.URL+"/api/scenes/"+scene.ID+"/status", nil)
		if status == 200 && env.OK {
			var payload struct {
				Engine EngineStatus `json:"engine"`
			}
			if err := json.Unmarshal(env.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Engine.ResultReady {
				ready = true
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !ready {
		t.Fatal("engine did not complete")
	}
	status, env = serverCall(t, client, "GET", httpServer.URL+"/api/scenes/"+scene.ID+"/result", nil)
	if status != 200 || !env.OK {
		t.Fatalf("result status=%d env=%+v", status, env)
	}
	var view ResultView
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatal(err)
	}
	if !view.PeakPass || len(view.Patches) == 0 {
		t.Fatalf("truth verification failed: %+v", view)
	}
	for _, patch := range view.Patches {
		if !patch.Pass {
			t.Fatalf("patch %s failed with error %.6f", patch.ID, patch.MaxError)
		}
	}

	status, env = serverCall(t, client, "POST", httpServer.URL+"/api/scenes/"+scene.ID+"/compare", nil)
	if status != 200 || !env.OK {
		t.Fatalf("compare status=%d env=%+v", status, env)
	}
	var cmp ThreePathComparison
	if err := json.Unmarshal(env.Data, &cmp); err != nil {
		t.Fatal(err)
	}
	if !cmp.Consistent {
		t.Fatalf("comparison mismatch: %+v", cmp)
	}

	resp, err := client.Get(httpServer.URL + "/api/scenes/" + scene.ID + "/image/fused")
	if err != nil {
		t.Fatal(err)
	}
	pngData, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(pngData) < 8 || string(pngData[1:4]) != "PNG" {
		t.Fatalf("invalid PNG response: status=%d len=%d", resp.StatusCode, len(pngData))
	}

	resp, err = client.Get(httpServer.URL + "/api/scenes/" + scene.ID + "/export")
	if err != nil {
		t.Fatal(err)
	}
	zipData, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("export status=%d", resp.StatusCode)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"manifest.json", "truth.json", "fused.png", "rgb.png", "band-00.png", "README.txt"} {
		if !names[want] {
			t.Fatalf("export is missing %s", want)
		}
	}
}

func TestHTTPParameterErrorIsStructured(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerWithScenes(t.TempDir(), scene)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	cfg := DefaultProcessConfig()
	cfg.InterpolationOrder = 9
	status, env := serverCall(t, httpServer.Client(), "POST", httpServer.URL+"/api/scenes/"+scene.ID+"/params", cfg)
	if status != http.StatusUnprocessableEntity || env.Error == nil {
		t.Fatalf("status=%d env=%+v", status, env)
	}
	if env.Error.Code != CodePara || env.Error.Field != "interpolationOrder" {
		t.Fatalf("wrong structured error: %+v", env.Error)
	}
}

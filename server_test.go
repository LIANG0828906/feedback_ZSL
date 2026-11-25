package fog

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticHTTPWorkbench(t *testing.T) {
	workbench, err := NewWorkbench(filepath.Join(t.TempDir(), "workbench.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer workbench.Close()
	server := httptest.NewServer(workbench.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "体积雾") {
		t.Fatal("diagnostic HTML page did not render")
	}

	state := getSnapshot(t, server.URL+"/api/state")
	if len(state.Tiles) != 12 {
		t.Fatalf("expected 12 tile diagnostics, got %d", len(state.Tiles))
	}

	state = getSnapshot(t, server.URL+"/api/action?"+url.Values{"action": {"threeway"}}.Encode())
	if state.ThreeWay == nil || !state.ThreeWay.Equal {
		t.Fatalf("expected three-way equal conclusion, got %+v", state.ThreeWay)
	}

	state = getSnapshot(t, server.URL+"/api/action?"+url.Values{"action": {"inject"}, "kind": {"decode"}}.Encode())
	if state.Anomaly == nil || state.Anomaly.Code != CodeDecode {
		t.Fatalf("expected E_DECODE anomaly, got %+v", state.Anomaly)
	}

	blockQuery := url.Values{"action": {"inject"}, "kind": {"block"}, "x": {"1"}, "y": {"1"}}.Encode()
	state = getSnapshot(t, server.URL+"/api/action?"+blockQuery)
	if state.Anomaly == nil || state.Anomaly.Code != CodeBlock {
		t.Fatalf("expected E_BLOCK anomaly, got %+v", state.Anomaly)
	}
	foundFailed := false
	for _, tile := range state.Tiles {
		if tile.Tile == (TileCoord{X: 1, Y: 1}) && strings.HasPrefix(tile.Status, "failed") {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatal("block status was not marked failed")
	}

	retryQuery := url.Values{"action": {"retry"}, "x": {"1"}, "y": {"1"}}.Encode()
	state = getSnapshot(t, server.URL+"/api/action?"+retryQuery)
	if state.Anomaly != nil {
		t.Fatalf("retry should clear anomaly, got %+v", state.Anomaly)
	}
	foundRetried := false
	for _, tile := range state.Tiles {
		if tile.Tile == (TileCoord{X: 1, Y: 1}) && tile.Status == "committed-retry" {
			foundRetried = true
		}
	}
	if !foundRetried {
		t.Fatal("retried block was not marked committed-retry")
	}
}

func getSnapshot(t *testing.T, url string) WorkbenchSnapshot {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, string(body))
	}
	var snapshot WorkbenchSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v body=%s", err, string(body)[:minInt(200, len(body))])
	}
	return snapshot
}

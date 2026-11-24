package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"audiobench/internal/sample"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	varDir := filepath.Join(dir, "var")
	if _, err := sample.GenerateAll(sample.Options{
		OutDir: dataDir, Seed: 5, SampleRate: 16000,
		MaterialSecs: 6, EventCount: 4, WindowSize: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(dataDir, varDir)
	return httptest.NewServer(mgr.Router()), dataDir
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	buf, _ := json.Marshal(body)
	res, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestHTTPEndToEnd(t *testing.T) {
	srv, dataDir := newTestServer(t)
	defer srv.Close()

	// 健康检查与界面。
	if res, err := http.Get(srv.URL + "/api/health"); err != nil || res.StatusCode != 200 {
		t.Fatalf("health 异常: %v %v", res, err)
	}
	if res, err := http.Get(srv.URL + "/"); err != nil || res.StatusCode != 200 {
		t.Fatalf("UI 异常: %v %v", res, err)
	}

	// 打开会话。
	desc := filepath.Join(dataDir, "mat_main.json")
	status, body := postJSON(t, srv.URL+"/api/sessions", map[string]string{"descriptor": desc})
	if status != 200 {
		t.Fatalf("打开会话失败: %v", body)
	}
	id := body["material_id"].(string)

	// 实时标注。
	status, body = postJSON(t, srv.URL+"/api/sessions/"+id+"/run", nil)
	if status != 200 {
		t.Fatalf("实时标注失败: %v", body)
	}

	// 提交后改参数必须被 E_STATE 拒绝（冻结窗口不得静默改写）。
	params := body
	_ = params
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/sessions/"+id+"/params",
		bytes.NewReader([]byte(`{"frame_length":2048,"frame_shift":512,"bands":32,"gap_tolerance_frames":2,"min_hit_frames":2,"match_threshold":0.55,"quiet_ratio_threshold":0.05}`)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var errBody map[string]any
	json.NewDecoder(res.Body).Decode(&errBody)
	res.Body.Close()
	if errBody["code"] != "E_STATE" {
		t.Fatalf("期望冻结窗口参数改写被 E_STATE 拒绝，实际 %v", errBody)
	}

	// 三路径比对一致。
	status, body = postJSON(t, srv.URL+"/api/sessions/"+id+"/compare", nil)
	if status != 200 || body["consistent"] != true {
		t.Fatalf("三路径比对应一致: status=%d body=%v", status, body)
	}

	// 静音素材返回 E_QUIET。
	_, qbody := postJSON(t, srv.URL+"/api/sessions",
		map[string]string{"descriptor": filepath.Join(dataDir, "mat_quiet.json")})
	qid := qbody["material_id"].(string)
	status, qrun := postJSON(t, srv.URL+"/api/sessions/"+qid+"/full", nil)
	if status == 200 || qrun["code"] != "E_QUIET" {
		t.Fatalf("期望 E_QUIET，实际 status=%d body=%v", status, qrun)
	}
}

func TestHTTPBadParamsReturnEPara(t *testing.T) {
	srv, dataDir := newTestServer(t)
	defer srv.Close()
	_, body := postJSON(t, srv.URL+"/api/sessions",
		map[string]string{"descriptor": filepath.Join(dataDir, "mat_empty.json")})
	id := body["material_id"].(string)
	// 新会话无提交窗口，参数可写入；帧移>帧长必须 E_PARA。
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/sessions/"+id+"/params",
		bytes.NewReader([]byte(`{"frame_length":512,"frame_shift":1024,"bands":32,"gap_tolerance_frames":2,"min_hit_frames":2,"match_threshold":0.55,"quiet_ratio_threshold":0.05}`)))
	res, _ := http.DefaultClient.Do(req)
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if out["code"] != "E_PARA" || out["details"].(map[string]any)["field"] != "frame_shift" {
		t.Fatalf("期望 frame_shift 的 E_PARA，实际 %v", out)
	}
}

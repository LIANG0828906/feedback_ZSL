package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"maskqc"
)

func TestDashboardHTTPAndEditFlow(t *testing.T) {
	dir := t.TempDir()
	server, err := NewServer(dir, filepath.Join(dir, "workbench.db"), maskqc.BudgetConfig{
		MaxParallel: 2, MaxPixelsPerWorker: 4096, MaxQueuedTiles: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/?icon=" + maskqc.HarnessIconID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home status %d", resp.StatusCode)
	}
	for _, want := range []string{"笔画列表", "连通域统计", "失效传播", "提交历史", "预算占用"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("home missing section %q", want)
		}
	}

	pngResp, err := http.Get(ts.URL + "/mask.png?icon=" + maskqc.HarnessIconID + "&kind=accum")
	if err != nil {
		t.Fatal(err)
	}
	pngBody, _ := io.ReadAll(pngResp.Body)
	pngResp.Body.Close()
	if pngResp.StatusCode != 200 || len(pngBody) < 16 || string(pngBody[1:4]) != "PNG" {
		t.Fatalf("png response bad status=%d bytes=%d", pngResp.StatusCode, len(pngBody))
	}

	form := url.Values{}
	form.Set("icon_id", maskqc.HarnessIconID)
	form.Set("stroke_id", "bars-top")
	form.Set("weight", "7600")
	editResp, err := http.PostForm(ts.URL+"/edit/weight", form)
	if err != nil {
		t.Fatal(err)
	}
	editBody, _ := io.ReadAll(editResp.Body)
	editResp.Body.Close()
	if editResp.StatusCode != http.StatusOK {
		t.Fatalf("edit redirect should be followed to 200, got %d", editResp.StatusCode)
	}
	if !strings.Contains(string(editBody), "topology/direction") || !strings.Contains(string(editBody), "flash ok") {
		if len(editBody) > 2000 {
			t.Logf("response prefix: %s", editBody[:2000])
		}
		t.Fatal("edit response missing invalidation impact")
	}
}

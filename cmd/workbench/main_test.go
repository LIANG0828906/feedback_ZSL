package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"volumefog/fog"
)

func TestDashboardAndInjectionTemplates(t *testing.T) {
	opts := fog.DefaultGenerateOptions(77)
	opts.Width, opts.Height = 8, 6
	opts.TileWidth, opts.TileHeight = 4, 3
	scene := fog.Generate(opts)
	rep, err := fog.RunDiagnostics(scene, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("dashboard").Funcs(templateFuncs()).Parse(pageTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	if err := tpl.Execute(&page, buildDashboard(rep)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"双路径帧与一致判定", "精确失效传播", "三路径比对", "异常注入链路", "E_BLOCK"} {
		if !strings.Contains(page.String(), want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	injTpl, err := template.New("injection").Funcs(templateFuncs()).Parse(injectionTemplate)
	if err != nil {
		t.Fatal(err)
	}
	inj := fog.RunInjection(scene, fog.InjectBlock, "tile-r00-c00", scene.Media[0].ID)
	iv := injectionView{Report: inj}
	if inj.Frame != nil {
		for _, tile := range inj.Frame.Tiles {
			iv.Tiles = append(iv.Tiles, tile.ID)
		}
	}
	var injectPage bytes.Buffer
	if err := injTpl.Execute(&injectPage, iv); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(injectPage.String(), "E_BLOCK") || !strings.Contains(injectPage.String(), "tile-r00-c00") {
		t.Fatalf("injection page missing diagnostic detail")
	}

	srv := httptest.NewServer(newMux(scene, rep, tpl, injTpl))
	defer srv.Close()
	for _, path := range []string{"/", "/healthz", "/inject?kind=block&tile=tile-r00-c00"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, body.String())
		}
	}
	resp, err := http.Get(srv.URL + "/api/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("diagnostics endpoint JSON: %v", err)
	}
	if payload["three_path"] == nil || payload["exact"] == nil {
		t.Fatalf("diagnostics endpoint missing report fields")
	}
	bad, err := http.Get(srv.URL + "/inject?kind=nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid injection status=%d", bad.StatusCode)
	}
}

package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"acousticfield/internal/generator"
	"acousticfield/internal/rebuild"
	"acousticfield/internal/web"
	"context"
)

func TestDiagnosticPageAndAPI(t *testing.T) {
	level, truth := generator.Generate(generator.DefaultSeed)
	report, err := rebuild.NewService(level, truth, nil).Run(context.Background(), rebuild.RunOptions{RoundID: "r", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(web.NewHandler(report))
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	resp, err = http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "分块声学能量场") {
		t.Fatal("diagnostic HTML missing title")
	}
	resp, err = http.Get(server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status %d", resp.StatusCode)
	}
}

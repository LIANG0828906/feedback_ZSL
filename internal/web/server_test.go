package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lip-pipeline-prototype/internal/service"
	"lip-pipeline-prototype/internal/web"
)

func TestConsoleAndOverviewHTTP(t *testing.T) {
	svc, err := service.New(context.Background(), filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server := httptest.NewServer(web.New(svc).Handler())
	defer server.Close()

	res, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page status=%d", res.StatusCode)
	}

	res, err = http.Get(server.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("overview status=%d", res.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "lip-deterministic-project") {
		t.Fatal("overview did not contain deterministic project")
	}
}

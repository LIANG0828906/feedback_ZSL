package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"edgerefine/internal/sample"
)

func TestDashboardAPIAndVisualizations(t *testing.T) {
	root := t.TempDir()
	if _, err := sample.GenerateAll(root); err != nil {
		t.Fatal(err)
	}
	srv, err := New(root, filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) *http.Response {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := get("/")
	if r.StatusCode != 200 {
		t.Fatalf("index status %d", r.StatusCode)
	}
	r.Body.Close()

	r = get("/api/run?sample=spots96&workers=2")
	if r.StatusCode != 200 {
		t.Fatalf("run status %d", r.StatusCode)
	}
	r.Body.Close()

	for _, kind := range []string{"original", "separation", "clean", "skeleton", "refined", "overlay", "heatmap"} {
		r = get("/viz?sample=spots96&kind=" + kind)
		if r.StatusCode != 200 {
			t.Fatalf("viz %s status %d", kind, r.StatusCode)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
			t.Fatalf("viz %s content type %s", kind, ct)
		}
		r.Body.Close()
	}

	r = get("/api/verify?sample=spots96")
	if r.StatusCode != 200 {
		t.Fatalf("verify status %d", r.StatusCode)
	}
	r.Body.Close()
}

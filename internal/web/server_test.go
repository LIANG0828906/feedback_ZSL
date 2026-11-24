package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ocrworkbench/internal/app"
	"ocrworkbench/internal/web"
)

func TestLocalAPIStateAndImageServing(t *testing.T) {
	dir := t.TempDir()
	application, err := app.New(app.NewOptions{
		DataDir: filepath.Join(dir, "data"),
		DBPath:  filepath.Join(dir, "workbench.db"),
		Seed:    42,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	handler := web.New(application).Handler()

	request := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("state status = %d body=%s", response.Code, response.Body.String())
	}
	var state struct {
		Manifest struct {
			Images []any `json:"images"`
		} `json:"manifest"`
		Runs map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Manifest.Images) != 4 {
		t.Fatalf("state includes %d images", len(state.Manifest.Images))
	}

	imageRequest := httptest.NewRequest(http.MethodGet, "/image/dense_1280", nil)
	imageRequest.RemoteAddr = "127.0.0.1:43210"
	imageResponse := httptest.NewRecorder()
	handler.ServeHTTP(imageResponse, imageRequest)
	if imageResponse.Code != http.StatusOK || imageResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image response %d %s", imageResponse.Code, imageResponse.Header().Get("Content-Type"))
	}
}

func TestNonLoopbackRequestIsRejected(t *testing.T) {
	dir := t.TempDir()
	application, err := app.New(app.NewOptions{
		DataDir: filepath.Join(dir, "data"),
		DBPath:  filepath.Join(dir, "workbench.db"),
		Seed:    42,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	request.RemoteAddr = "192.0.2.20:1234"
	response := httptest.NewRecorder()
	web.New(application).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback request status = %d", response.Code)
	}
}

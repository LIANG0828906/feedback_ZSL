package webui_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"skeletonretarget/internal/demo"
	"skeletonretarget/internal/webui"
)

func TestDiagnosticPageAndReportAPI(t *testing.T) {
	result, err := demo.Build(context.Background(), demo.Options{Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Cleanup()
	handler, err := webui.NewHandler(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	pageBytes, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(pageBytes)
	if !strings.Contains(page, "Skeleton Retarget Diagnostics") || !strings.Contains(page, "Residual And Upper-Bound Curves") {
		t.Fatalf("page does not contain required sections")
	}

	response, err = server.Client().Get(server.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data demo.Data
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Frames) != 24 || len(data.Paths) != 3 {
		t.Fatalf("report is incomplete: frames=%d paths=%d", len(data.Frames), len(data.Paths))
	}
	for _, comparison := range data.Comparisons {
		if !comparison.Equal {
			t.Fatalf("comparison failed: %+v", comparison)
		}
	}
	if !data.Edit.Comparison.Equal {
		t.Fatal("edit comparison failed")
	}
}

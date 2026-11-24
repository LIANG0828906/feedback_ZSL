package workbench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"hdrproof/internal/engine"
	"hdrproof/internal/model"
)

func TestWorkbenchEndToEnd(t *testing.T) {
	app := New()
	app.Startup(context.Background())
	root := t.TempDir()
	ws, err := app.Generate(GenerateRequest{Root: root, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Routes) != 4 || ws.TruthFingerprint == "" {
		t.Fatalf("unexpected generated workspace: %+v", ws)
	}
	run, err := app.Run(RunRequest{
		Mode: string(engine.ModeFull), Params: model.DefaultParams(), MaxWorkers: 4, Buffer: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Error != nil || run.Result == nil {
		t.Fatalf("run failed: %+v", run)
	}
	if run.Workspace.Progress.RoutesCommitted != 4 || run.Workspace.Progress.SegmentsCommitted != 18 {
		t.Fatalf("incomplete run: %+v", run.Workspace.Progress)
	}
	detail, err := app.FrameDetail("arena-linear-bt2020", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Committed || detail.Proof == nil || !strings.HasPrefix(detail.SDRDataURL, "data:image/png;base64,") ||
		!strings.HasPrefix(detail.HDRDataURL, "data:image/png;base64,") {
		t.Fatalf("frame detail is incomplete: %+v", detail)
	}
	auditResponse := app.Audit()
	if auditResponse.Error != nil || auditResponse.Result == nil || !auditResponse.Result.Passed {
		t.Fatalf("audit failed: %+v", auditResponse)
	}
	compareResponse := app.Compare(CompareRequest{Left: ws.DBPath, Right: ws.DBPath})
	if compareResponse.Error != nil || compareResponse.Result == nil || !compareResponse.Result.Equal {
		t.Fatalf("compare failed: %+v", compareResponse)
	}
	export, err := app.Export(filepath.Join(root, "export"))
	if err != nil {
		t.Fatal(err)
	}
	if export.Frames != 95 || export.Proofs != 95 || export.HDRPreviews != 95 {
		t.Fatalf("unexpected export: %+v", export)
	}
	retry, err := app.RetrySegment(RetryRequest{RouteID: "arena-linear-bt2020", Segment: 0})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Error == nil || retry.Error.Code != "E_STATE" {
		t.Fatalf("committed segment was not frozen: %+v", retry)
	}
	if len(app.Events()) == 0 {
		t.Fatal("no events captured")
	}
}

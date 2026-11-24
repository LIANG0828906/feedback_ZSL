package verify

import (
	"os"
	"path/filepath"
	"testing"

	"texlab/internal/apperr"
	"texlab/internal/checkpoint"
	"texlab/internal/engine"
	"texlab/internal/runrecord"
	"texlab/internal/sample"
	"texlab/internal/truth"
)

func TestVerifierReportsFirstTruthDiffAndStableReport(t *testing.T) {
	root := t.TempDir()
	spec := sample.Spec{
		Name: "verify", Width: 64, Height: 48, Depth: 8, Layout: "RGBA", Seed: 1234,
		Patches: []sample.PatchSpec{
			{Name: "coral", XF: 0.1, YF: 0.1, Size: 5, RGBA: [4]int{250, 20, 10, 255}},
		},
	}
	if err := sample.Generate(root, spec); err != nil {
		t.Fatal(err)
	}
	p, err := sample.Load(root, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	cpDir := filepath.Join(work, "cp")
	store, err := checkpoint.NewStore(cpDir, p.Plan, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(p.Plan, p.Source, p.SourceFP, engine.Options{Workers: 3, PixelBufferPerWorker: 262144, Sink: store})
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(work, "run.json")
	if err := runrecord.Write(recordPath, runrecord.Build(res)); err != nil {
		t.Fatal(err)
	}
	reportA := filepath.Join(work, "a.json")
	reportB := filepath.Join(work, "b.json")
	opts := Options{
		PlanPath: filepath.Join(p.Dir, "plan.json"), TruthPath: filepath.Join(p.Dir, "truth.json"),
		CheckpointDir: cpDir, RecordPath: recordPath,
		SourceImage: p.Source, SourceFingerprint: p.SourceFP,
	}
	opts.ReportPath = reportA
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	opts.ReportPath = reportB
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(reportA)
	b, _ := os.ReadFile(reportB)
	if string(a) != string(b) {
		t.Fatal("verification reports are not byte-identical")
	}

	badTruth := p.Truth
	im, err := truth.ImageFromEntry(badTruth.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	pixel := im.SemanticPixel(3, 3)
	pixel[1] ^= 1
	im.SetSemanticPixel(3, 3, pixel)
	encoded, err := truth.EncodePixels(im)
	if err != nil {
		t.Fatal(err)
	}
	badTruth.Entries[0].PixelsZ = encoded
	badTruth.Entries[0].ContentHash = im.ContentHash()
	badTruthPath := filepath.Join(work, "bad-truth.json")
	if err := truth.Write(badTruthPath, badTruth); err != nil {
		t.Fatal(err)
	}
	opts.TruthPath = badTruthPath
	opts.ReportPath = filepath.Join(work, "bad-report.json")
	report, err := Run(opts)
	if !apperr.IsCode(err, apperr.EDiff) {
		t.Fatalf("want E_DIFF, got %v", err)
	}
	ae := apperr.As(err)
	if ae.Coordinate == nil || ae.Coordinate.X != 3 || ae.Coordinate.Y != 3 ||
		ae.EntryIndex != 1 || ae.Field != "pixels" {
		t.Fatalf("diff context = %+v", ae)
	}
	if report == nil || report.Status != "DIFF" || report.Error == nil {
		t.Fatalf("report = %+v", report)
	}
}

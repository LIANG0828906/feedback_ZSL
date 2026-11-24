package workbench

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPublicAPIThreePaths(t *testing.T) {
	dir := t.TempDir()
	names, err := GenerateBuiltin(dir)
	if err != nil || len(names) != 3 {
		t.Fatalf("generate: %v %v", names, err)
	}
	ds, err := LoadSample(dir, "stereo_48k")
	if err != nil {
		t.Fatal(err)
	}
	params := FrameParams{
		FrameLength: 1024, FrameHop: 512, WindowLen: 768, ConvWindow: 64,
		RenderBlock: 512, InterpOrder: 1, BoundScale: 1, MaxWorkers: 3, BufferCap: 2048,
	}
	sess, err := OpenSession(ds, params, filepath.Join(dir, "stereo_48k", "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rt, err := sess.Run(context.Background(), "realtime", 0, params)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := sess.Run(context.Background(), "resume", 0, params)
	if err != nil {
		t.Fatal(err)
	}
	rp, err := sess.Run(context.Background(), "replay", 0, params)
	if err != nil {
		t.Fatal(err)
	}
	if d := CompareReports(rt.Report, rs.Report); d != nil {
		t.Fatalf("resume divergence: %+v", d)
	}
	if d := CompareReports(rt.Report, rp.Report); d != nil {
		t.Fatalf("replay divergence: %+v", d)
	}
	if rt.Report.Committed != rt.Report.Frames {
		t.Fatal("all frames should commit")
	}
}

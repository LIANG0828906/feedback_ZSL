package service

import (
	"path/filepath"
	"testing"

	"strokeforge/internal/engine"
	"strokeforge/internal/generator"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(generator.DefaultSeed, engine.DefaultPoolConfig(),
		filepath.Join(t.TempDir(), "cp"))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestFullRenderAndEquality(t *testing.T) {
	svc := newTestService(t)
	stats, err := svc.FullRender()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rendered != 30 {
		t.Fatalf("rendered=%d", stats.Rendered)
	}
	next := svc.Doc().StrokeParams
	next.Width = 12
	entries, _, diff, err := svc.Incremental(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected invalidation entries")
	}
	if diff == nil || !diff.OK || diff.PixelDifference != 0 || diff.SequenceMismatch {
		t.Fatalf("incremental diverged: %+v", diff)
	}
}

func TestCheckpointResume(t *testing.T) {
	svc := newTestService(t)
	saved, err := svc.CheckpointPartial(7)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 7 {
		t.Fatalf("saved=%d", saved)
	}
	restart, err := New(generator.DefaultSeed, engine.DefaultPoolConfig(), svc.cpDir)
	if err != nil {
		t.Fatal(err)
	}
	report, diff, err := restart.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.Restored != 7 || report.FullRebuild {
		t.Fatalf("report=%+v", report)
	}
	if diff == nil || !diff.OK {
		t.Fatalf("resume divergence: %+v", diff)
	}
}

func TestFaultCodes(t *testing.T) {
	svc := newTestService(t)
	cases := map[string]string{
		engine.FaultSelfIntersect:   "E_SELF",
		engine.FaultZeroLength:      "E_ZLEN",
		engine.FaultDensityOverflow: "E_DENS",
		engine.FaultStageFailure:    "E_STAGE",
	}
	for kind, want := range cases {
		err := svc.Diagnose(kind, "Aegis", 3)
		if err == nil {
			t.Fatalf("%s: expected error", kind)
		}
		if got := err.Error(); got[:len(want)] != want {
			t.Fatalf("%s: want %s got %v", kind, want, err)
		}
	}
}

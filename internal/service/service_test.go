package service

import (
	"math"
	"testing"

	"mocapbench/internal/core"
)

func TestVerifyThreePathsAndRecovery(t *testing.T) {
	sampleDir := t.TempDir() + "/samples"
	if _, err := core.GenerateSampleSet(core.GeneratorOptions{OutDir: sampleDir, Seed: core.DefaultSeed}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(sampleDir, t.TempDir()+"/checkpoints")
	if err != nil {
		t.Fatal(err)
	}
	params := core.DefaultParameters()
	report, verr := svc.Verify("walk_cycle", "target_compact", params, core.ResourceConfig{Workers: 2, BufferPerWorker: 2}, 11)
	if verr != nil {
		t.Fatalf("Verify returned error: %v\nreport=%+v", verr, report)
	}
	if report == nil || !report.OK {
		t.Fatalf("report not OK: %+v", report)
	}
	if report.ThreeWay == nil || !report.ThreeWay.OK {
		t.Fatalf("three-way mismatch: %+v", report.ThreeWay)
	}
	if !report.Summary.RecoveryOK || !report.Summary.ConsistencyOK {
		t.Fatalf("recovery/consistency false: %+v", report.Summary)
	}
	if report.ContactCheck.MaxDrift > params.ContactDriftLimit {
		t.Fatalf("contact drift too high: %g", report.ContactCheck.MaxDrift)
	}
	for _, frame := range report.Frames {
		for _, anchor := range frame.Contacts.Anchors {
			if math.Abs(anchor[1]) > 1e-12 {
				t.Fatalf("contact anchor is not on ground plane: %+v", anchor)
			}
		}
		for _, d := range frame.Diagnostics {
			if d.FKObservedError > d.FKBound+1e-12 {
				t.Fatalf("FK observed error exceeds bound at f=%d joint=%s: %g>%g", frame.Frame, d.Joint, d.FKObservedError, d.FKBound)
			}
			if d.ReachResidual > d.ProjectionBound+1e-12 {
				t.Fatalf("reach residual exceeds projection bound at f=%d joint=%s", frame.Frame, d.Joint)
			}
			if d.Depth > 0 && d.QuantResidual > d.QuantBound+1e-12 {
				t.Fatalf("quant residual exceeds cumulative bound at f=%d joint=%s", frame.Frame, d.Joint)
			}
		}
	}
}

func TestVerifyContactBoundFailureReturnsEContact(t *testing.T) {
	sampleDir := t.TempDir() + "/samples"
	if _, err := core.GenerateSampleSet(core.GeneratorOptions{OutDir: sampleDir, Seed: core.DefaultSeed}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(sampleDir, t.TempDir()+"/checkpoints")
	if err != nil {
		t.Fatal(err)
	}
	params := core.DefaultParameters()
	params.ContactDriftLimit = 1e-18
	report, verr := svc.Verify("walk_cycle", "target_hero_tall", params, core.DefaultResources(), 0)
	if verr == nil || !core.IsCode(verr, core.ECodeContact) {
		t.Fatalf("expected E_CONTACT, got %v", verr)
	}
	if verr.Frame < 0 {
		t.Fatalf("E_CONTACT should identify a violating frame: %+v", verr)
	}
	if report == nil || report.ContactCheck.OK {
		t.Fatalf("report should contain failed contact check: %+v", report)
	}
}

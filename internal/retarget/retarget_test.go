package retarget_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/retarget"
	"mocapworkbench/internal/sample"
)

func TestMappingTraceAndOneToMany(t *testing.T) {
	clip, targets := fixture(t)
	mapping, err := retarget.BuildMapping(clip.Skeleton, targets["tall"])
	if err != nil {
		t.Fatal(err)
	}
	if mapping.ByTarget["thumb_l"].Source != "hand_l" || mapping.ByTarget["index_l"].Source != "hand_l" {
		t.Fatalf("one-to-many mapping not traceable: %#v", mapping.Trace.Rules)
	}
	if mapping.ByTarget["tail"].Strategy != "one_to_many" {
		t.Fatalf("expected tail one-to-many, got %s", mapping.ByTarget["tail"].Strategy)
	}
	compact, err := retarget.BuildMapping(clip.Skeleton, targets["compact"])
	if err != nil {
		t.Fatal(err)
	}
	if len(compact.Trace.UnmappedSources) == 0 {
		t.Fatal("compact target should explicitly list unmapped source joints")
	}
}

func TestNoFeasibleMapping(t *testing.T) {
	clip, targets := fixture(t)
	target := targets["compact"]
	target.Mapping = append([]model.MapRule(nil), target.Mapping...)
	var kept []model.MapRule
	for _, rule := range target.Mapping {
		if rule.Source != "hand_l" {
			kept = append(kept, rule)
		}
	}
	target.Mapping = kept
	_, err := retarget.BuildMapping(clip.Skeleton, target)
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.ErrMap {
		t.Fatalf("expected E_MAP, got %v", err)
	}
	if len(appErr.Details) == 0 {
		t.Fatalf("E_MAP must list unmapped joints: %v", appErr)
	}
}

func TestExactAndAdaptiveModes(t *testing.T) {
	_, targets := fixture(t)
	short := loadClip(t, "clip_short")
	medium := loadClip(t, "clip_medium")
	cfg := model.DefaultConfig()
	exact, err := retarget.Run(short, targets["tall"], cfg)
	if err != nil {
		t.Fatal(err)
	}
	if exact.Plan.Mode != "exact" {
		t.Fatalf("short clip should choose exact, got %s", exact.Plan.Mode)
	}
	checkAnimationBounds(t, short, exact)

	sampled, err := retarget.Run(medium, targets["tall"], cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sampled.Plan.Mode != "sampled" || sampled.Plan.Stride <= 1 {
		t.Fatalf("medium clip should be sampled with stride >1: %#v", sampled.Plan)
	}
	if !sampled.Plan.ShortenedSmoothing {
		t.Fatalf("sampled plan should explain shortened smoothing: %#v", sampled.Plan)
	}
	exactMediumCfg := cfg
	exactMediumCfg.Mode = "exact"
	exactMedium, err := retarget.Run(medium, targets["tall"], exactMediumCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := retarget.CheckScaleConsistency(exactMedium, sampled, medium.Truth.Tolerances); err != nil {
		t.Fatalf("scale tolerance should hold: %v", err)
	}
}

func TestDeterministicSmoothingDecisions(t *testing.T) {
	clip, targets := fixture(t)
	cfg := model.DefaultConfig()
	a, err := retarget.Run(clip, targets["wide"], cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := retarget.Run(clip, targets["wide"], cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Frames) != len(b.Frames) {
		t.Fatal("frame count changed")
	}
	for i := range a.Frames {
		if len(a.Frames[i].Decisions) != len(b.Frames[i].Decisions) {
			t.Fatalf("decision count differs at frame %d", i)
		}
		for j := range a.Frames[i].Decisions {
			if a.Frames[i].Decisions[j] != b.Frames[i].Decisions[j] {
				t.Fatalf("decision sequence differs at frame %d: %#v != %#v", i, a.Frames[i].Decisions[j], b.Frames[i].Decisions[j])
			}
		}
	}
}

func TestParameterErrors(t *testing.T) {
	clip, targets := fixture(t)
	cfg := model.DefaultConfig()
	cfg.SmoothWindow = 0
	_, err := retarget.Run(clip, targets["tall"], cfg)
	assertCodeField(t, err, model.ErrParam, "smooth_window")
	cfg = model.DefaultConfig()
	cfg.MaxIterations = 0
	_, err = retarget.Run(clip, targets["tall"], cfg)
	assertCodeField(t, err, model.ErrParam, "max_iterations")
}

func TestContactMarkerAndInfeasibleErrors(t *testing.T) {
	clip, targets := fixture(t)
	cfg := model.DefaultConfig()
	bad := clip
	bad.Frames = append([]model.Frame(nil), clip.Frames...)
	bad.Frames[5].Contacts = map[string]bool{"foot_l": false}
	err := retarget.ValidateContactMarkers(bad, targets["tall"])
	var markerErr *model.AppError
	if !errors.As(err, &markerErr) || markerErr.Code != model.ErrMark || markerErr.Frame != 5 {
		t.Fatalf("expected direct E_MARK at frame 5, got %v", err)
	}
	_, err = retarget.Run(bad, targets["tall"], cfg)
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.ErrMark {
		t.Fatalf("expected E_MARK, got %v", err)
	}

	shortArms := targets["compact"]
	shortArms.Joints = append([]model.Joint(nil), shortArms.Joints...)
	for i := range shortArms.Joints {
		switch shortArms.Joints[i].Name {
		case "upper_arm_l", "forearm_l", "hand_l":
			shortArms.Joints[i].Offset.X = 0.005
			if shortArms.Joints[i].Name == "hand_l" {
				shortArms.Joints[i].Offset.X = 0.01
			}
		}
	}
	_, err = retarget.Run(clip, shortArms, cfg)
	if !errors.As(err, &appErr) || appErr.Code != model.ErrInfeas {
		t.Fatalf("expected E_INFEAS, got %v", err)
	}
	if appErr.Frame < 0 || appErr.Joint == "" {
		t.Fatalf("E_INFEAS must identify frame and joint: %v", appErr)
	}
}

func TestScaleToleranceError(t *testing.T) {
	exact := &model.Animation{
		ClipID: "scale-test", ContactFrames: []int{0, 1, 2},
		RootStart: model.Vec3{X: 0}, RootEnd: model.Vec3{Z: 0},
	}
	sampled := &model.Animation{
		ClipID: "scale-test", ContactFrames: []int{0, 3},
		RootStart: model.Vec3{X: 1}, RootEnd: model.Vec3{Z: 1},
	}
	err := retarget.CheckScaleConsistency(exact, sampled, model.Tolerances{
		ScaleContactSlack: 0, ScaleRoot: 0.001,
	})
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.ErrScale {
		t.Fatalf("expected E_SCALE, got %v", err)
	}
	if !strings.Contains(appErr.Message, "contact_frame_delta=") || !strings.Contains(appErr.Message, "root_start_delta=") {
		t.Fatalf("E_SCALE must include measured metrics: %v", appErr)
	}
}

func checkAnimationBounds(t *testing.T, clip model.Clip, animation *model.Animation) {
	t.Helper()
	tol := clip.Truth.Tolerances
	for _, frame := range animation.Frames {
		if frame.Stats.Residual > tol.ResidualNormalized {
			t.Fatalf("frame %d residual %.9f exceeds tolerance", frame.Frame, frame.Stats.Residual)
		}
		if frame.Stats.AngleViolationDeg > tol.AngleDeg {
			t.Fatalf("frame %d angle violation %.9f exceeds tolerance", frame.Frame, frame.Stats.AngleViolationDeg)
		}
		if frame.Stats.ReachViolation > tol.Reach {
			t.Fatalf("frame %d reach violation %.9f exceeds tolerance", frame.Frame, frame.Stats.ReachViolation)
		}
		if frame.Stats.ContactDrift > tol.ContactDrift {
			t.Fatalf("frame %d contact drift %.9f exceeds tolerance", frame.Frame, frame.Stats.ContactDrift)
		}
	}
}

func assertCodeField(t *testing.T, err error, code model.ErrorCode, field string) {
	t.Helper()
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != code || appErr.Field != field {
		t.Fatalf("expected %s field %s, got %v", code, field, err)
	}
}

func fixture(t *testing.T) (model.Clip, map[string]model.TargetSkeleton) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	clip, err := loader.LoadClip(filepath.Join(dir, "clips", "clip_short.json"))
	if err != nil {
		t.Fatal(err)
	}
	targets := make(map[string]model.TargetSkeleton)
	for key, name := range map[string]string{
		"compact": "target_compact_17.json",
		"tall":    "target_tall_29.json",
		"wide":    "target_wide_21.json",
	} {
		target, err := loader.LoadTarget(filepath.Join(dir, "targets", name))
		if err != nil {
			t.Fatal(err)
		}
		targets[key] = target
	}
	return clip, targets
}

func loadClip(t *testing.T, name string) model.Clip {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	clip, err := loader.LoadClip(filepath.Join(dir, "clips", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return clip
}

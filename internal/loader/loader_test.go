package loader_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
)

func TestValidationErrorCodes(t *testing.T) {
	clip, _ := fixture(t)

	t.Run("missing schema", func(t *testing.T) {
		bad := clip
		bad.SchemaVersion = ""
		assertCode(t, loader.ValidateClip(bad), model.ErrSchema, "schema_version")
	})
	t.Run("parent cycle", func(t *testing.T) {
		bad := clip
		bad.Skeleton.Joints = append([]model.Joint(nil), clip.Skeleton.Joints...)
		bad.Skeleton.Joints[1].Parent = "head"
		assertCode(t, loader.ValidateClip(bad), model.ErrCycle, "parent")
	})
	t.Run("truth frame count", func(t *testing.T) {
		bad := clip
		bad.Truth.FrameCount++
		assertCode(t, loader.ValidateClip(bad), model.ErrFrame, "truth.frame_count")
	})
	t.Run("rotation count", func(t *testing.T) {
		bad := clip
		bad.Frames = append([]model.Frame(nil), clip.Frames...)
		bad.Frames[7].Rotations = bad.Frames[7].Rotations[:len(bad.Frames[7].Rotations)-1]
		assertCode(t, loader.ValidateClip(bad), model.ErrFrame, "rotations")
	})
	t.Run("contact truth contradiction", func(t *testing.T) {
		bad := clip
		bad.Frames = append([]model.Frame(nil), clip.Frames...)
		bad.Frames[0].Contacts = map[string]bool{"foot_l": false, "foot_r": bad.Frames[0].Contacts["foot_r"]}
		assertCode(t, loader.ValidateClip(bad), model.ErrMark, "foot_l")
	})
}

func TestTargetCycleAndMapping(t *testing.T) {
	_, target := fixture(t)
	cyclic := target
	cyclic.Joints = append([]model.Joint(nil), target.Joints...)
	cyclic.Joints[1].Parent = cyclic.Joints[4].Name
	assertCode(t, loader.ValidateTarget(cyclic, fixtureSourceSkeleton(t)), model.ErrCycle, "parent")
}

func TestMissingFrameFieldIsSchemaError(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "clips", "clip_short.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	frames := raw["frames"].([]any)
	frame := frames[2].(map[string]any)
	delete(frame, "root_translation")
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = loader.LoadClip(path)
	assertCode(t, err, model.ErrSchema, "root_translation")
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Frame != 2 {
		t.Fatalf("expected frame 2 in missing-field error, got %v", err)
	}
}

func fixture(t *testing.T) (model.Clip, model.TargetSkeleton) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	clip, err := loader.LoadClip(filepath.Join(dir, "clips", "clip_short.json"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := loader.LoadTarget(filepath.Join(dir, "targets", "target_tall_29.json"))
	if err != nil {
		t.Fatal(err)
	}
	return clip, target
}

func fixtureSourceSkeleton(t *testing.T) model.Skeleton {
	clip, _ := fixture(t)
	return clip.Skeleton
}

func assertCode(t *testing.T, err error, code model.ErrorCode, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var appErr *model.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, appErr.Code, err)
	}
	if field != "" && !strings.Contains(appErr.Field, field) && !strings.Contains(appErr.Joint, field) {
		t.Fatalf("expected field %q, got field=%q joint=%q: %v", field, appErr.Field, appErr.Joint, err)
	}
}

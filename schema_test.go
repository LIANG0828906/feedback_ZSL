package silhouette

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestGeneratorIsFixedSeedDeterministic(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		a, err := Generate(DefaultSeed, version)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Generate(DefaultSeed, version)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.Schema, b.Schema) {
			t.Fatalf("v%d schema bytes differ for fixed seed", version)
		}
		if len(a.Truth.Segments) != len(b.Truth.Segments) || a.Truth.SceneID != b.Truth.SceneID {
			t.Fatalf("v%d truth differs for fixed seed", version)
		}
	}
	other, err := Generate(DefaultSeed+1, 3)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := Generate(DefaultSeed, 3)
	if bytes.Equal(base.Schema, other.Schema) {
		t.Fatal("different seeds produced identical scenes")
	}
}

func TestThreeSchemaVersionsLoadCanonicalScene(t *testing.T) {
	canonical := GenerateScene(DefaultSeed)
	truth := GenerateTruth(canonical, VersionCurrent)
	for _, version := range []int{1, 2, 3} {
		t.Run("v"+string(rune('0'+version)), func(t *testing.T) {
			generated, err := Generate(DefaultSeed, version)
			if err != nil {
				t.Fatal(err)
			}
			scene, err := Load(generated.Schema)
			if err != nil {
				t.Fatal(err)
			}
			if scene.SourceVersion != version {
				t.Fatalf("source version=%d want %d", scene.SourceVersion, version)
			}
			if !vec3SlicesEqual(scene.Surface.Controls, canonical.Surface.Controls) {
				t.Fatal("surface controls differ")
			}
			if !segmentCurvesEqual(scene.Segments, canonical.Segments) {
				t.Fatal("curves differ")
			}
			if !tilesEqual(scene.Tiles, canonical.Tiles) {
				t.Fatal("tiles differ")
			}
			if scene.Tolerance.Value != canonical.Tolerance.Value {
				t.Fatalf("tolerance=%d want %d", scene.Tolerance.Value, canonical.Tolerance.Value)
			}
			if scene.Budget.MaxVertices != canonical.Budget.MaxVertices {
				t.Fatalf("budget=%d want %d", scene.Budget.MaxVertices, canonical.Budget.MaxVertices)
			}
			if !truthEndpointsMatch(scene, truth) {
				t.Fatal("analytic truth endpoints do not match loaded scene")
			}
			if len(scene.Traces) == 0 {
				t.Fatal("expected field conversion traces")
			}
			if version < 3 {
				defaults := 0
				for _, trace := range scene.Traces {
					if trace.Action == "default_fill" {
						defaults++
						if trace.Path == "" || trace.OldValue == "" || trace.NewValue == "" || trace.DefaultSource == "" {
							t.Fatalf("incomplete default trace: %+v", trace)
						}
					}
				}
				if defaults == 0 {
					t.Fatal("expected low-version default-fill traces")
				}
			}
		})
	}
}

func TestSchemaErrorsIncludePath(t *testing.T) {
	tests := []struct {
		name string
		data string
		path string
	}{
		{"unknown", `{"schema_version":99}`, "$.schema_version"},
		{"unknown-format", `{"format":"surface-schema/9"}`, "$.format"},
		{"missing-top", `{"schema_version":3,"scene_id":"x"}`, "$.surface"},
		{"missing-nested", `{"schema_version":3,"scene_id":"x","surface":{"name":"s","rows":4,"cols":4},"segments":[],"tiles":[],"sampling":{}}`, "$.surface.controls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.data))
			var de *DomainError
			if !asDomainError(err, &de) {
				t.Fatalf("want DomainError, got %v", err)
			}
			if de.Code != CodeSchema {
				t.Fatalf("code=%s want %s", de.Code, CodeSchema)
			}
			if de.Path != tt.path {
				t.Fatalf("path=%s want %s", de.Path, tt.path)
			}
		})
	}
}

func TestExportToLowerVersionIsLoadableAndLossy(t *testing.T) {
	high := GenerateScene(DefaultSeed)
	for _, target := range []int{1, 2} {
		t.Run("target-"+string(rune('0'+target)), func(t *testing.T) {
			exported, err := Export(high, target)
			if err != nil {
				t.Fatal(err)
			}
			if len(exported.Lossy) == 0 {
				t.Fatal("expected E_LOSSY issues for dropped explicit segment metadata")
			}
			for _, issue := range exported.Lossy {
				if issue.Code != CodeLossy || issue.Path == "" {
					t.Fatalf("invalid lossy issue: %+v", issue)
				}
			}
			low, err := Load(exported.Data)
			if err != nil {
				t.Fatal(err)
			}
			if low.SourceVersion != target {
				t.Fatalf("low source=%d want %d", low.SourceVersion, target)
			}
			back, err := Export(low, 3)
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := Load(back.Data)
			if err != nil {
				t.Fatal(err)
			}
			if !vec3SlicesEqual(reloaded.Surface.Controls, high.Surface.Controls) {
				t.Fatal("surface controls lost in roundtrip")
			}
			if !segmentCurvesEqual(reloaded.Segments, high.Segments) {
				t.Fatal("curve geometry lost in roundtrip")
			}
			if reloaded.Tolerance.Value != high.Tolerance.Value {
				t.Fatalf("tolerance=%d want %d", reloaded.Tolerance.Value, high.Tolerance.Value)
			}
			if !tilesEqual(reloaded.Tiles, high.Tiles) {
				t.Fatal("tiles lost in roundtrip")
			}
			if len(exported.Traces) == 0 {
				t.Fatal("expected export conversion traces")
			}
		})
	}
}

func TestNegativeCoordinateTruncationIsRecorded(t *testing.T) {
	op, err := ScaleInt(-1234567, 1, 1000, RoundFloor, "$.control.y")
	if err != nil {
		t.Fatal(err)
	}
	if op.Result != -1235 || op.Remainder != -567 || !op.Truncated {
		t.Fatalf("unexpected floor conversion: %+v", op)
	}
	zero, err := ScaleInt(-1234567, 1, 1000, RoundTowardZero, "$.control.y")
	if err != nil {
		t.Fatal(err)
	}
	if zero.Result != -1234 || zero.Remainder != -567 {
		t.Fatalf("unexpected toward-zero conversion: %+v", zero)
	}
	negativeDenominator, err := ScaleInt(5, -2, -1, RoundTowardZero, "$.scale")
	if err != nil {
		t.Fatal(err)
	}
	if negativeDenominator.Result != 10 {
		t.Fatalf("negative denominator conversion=%+v", negativeDenominator)
	}
	_, err = ScaleInt(1, 1, math.MinInt64, RoundTowardZero, "$.scale")
	var de *DomainError
	if !asDomainError(err, &de) || de.Code != CodeOverflow {
		t.Fatalf("negative denominator overflow error=%v", err)
	}
	pixel, err := LogicalToPixel(17, "$.pixel")
	if err != nil || pixel.Result != 17 || pixel.Truncated {
		t.Fatalf("logical-to-pixel conversion: %+v %v", pixel, err)
	}
	logical, err := PixelToLogical(17, "$.logical")
	if err != nil || logical.Result != 17 {
		t.Fatalf("pixel-to-logical conversion: %+v %v", logical, err)
	}
}

func TestRequiredFieldPathIsExact(t *testing.T) {
	var raw map[string]any
	data, _ := json.Marshal(GenerateScene(DefaultSeed))
	_ = json.Unmarshal(data, &raw)
	_, err := Load([]byte(`{"schema_version":3,"scene_id":"x","segments":[],"tiles":[],"sampling":{}}`))
	if err == nil || !strings.Contains(err.Error(), "$.surface") {
		t.Fatalf("missing surface error path: %v", err)
	}
}

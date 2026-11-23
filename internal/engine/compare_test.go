package engine

import (
	"errors"
	"testing"

	"textureworkbench/internal/tex"
)

func TestThreePathsMatchAcrossDepthAndChannelDirection(t *testing.T) {
	cases := []struct {
		sample     string
		descriptor string
	}{
		{"rgb8", "rgb8-to-bgra8-zero"},
		{"rgba8", "rgba8-to-rgb8-drop"},
		{"rgb16", "rgb16-to-rgba16-repeat"},
		{"bgra16", "bgra16-to-rgb8-drop"},
	}
	for _, tc := range cases {
		t.Run(tc.descriptor, func(t *testing.T) {
			fixture := makeFixture(t, tc.sample, tc.descriptor)
			cfg := testConfig(t, fixture)
			comparison, err := CompareThreePaths(fixture.image, fixture.desc, cfg)
			if err != nil {
				t.Fatalf("CompareThreePaths: %v", err)
			}
			if !comparison.Match || comparison.FirstDiff != nil {
				t.Fatalf("comparison mismatch: %+v", comparison)
			}
			if len(comparison.Paths) != 3 {
				t.Fatalf("paths=%d, want 3", len(comparison.Paths))
			}
			for _, path := range comparison.Paths {
				if path.BlockRecomputes == 0 {
					t.Fatalf("path %s did not report recompute counts", path.Path)
				}
			}
		})
	}
}

func TestDiffLocatesFirstPixelBlockAndValue(t *testing.T) {
	left := &tex.Converted{
		Width:    4,
		Height:   4,
		BitDepth: 8,
		Layout:   tex.LayoutRGB,
		Pixels:   make([]uint16, 4*4*3),
		Errors:   make([]int32, 4*4*3),
		Mapping:  tex.Mapping{SourceLayout: tex.LayoutRGB, TargetLayout: tex.LayoutRGB},
	}
	right := *left
	right.Pixels = append([]uint16(nil), left.Pixels...)
	right.Errors = append([]int32(nil), left.Errors...)
	right.Histogram = tex.BuildHistogram(left.Pixels, 3, tex.LayoutRGB, 8)
	left.Histogram = right.Histogram
	right.Pixels[(3*4+3)*3+1] = 77
	err := CompareConvertedOrError(left, &right, 2, 2)
	texErr := requireCode(t, err, tex.CodeDiff)
	diff, ok := texErr.Details.(map[string]any)["diff"].(*PixelDiff)
	if !ok {
		t.Fatalf("E_DIFF details missing PixelDiff: %#v", texErr.Details)
	}
	if diff.X != 3 || diff.Y != 3 || diff.Channel != 1 || diff.BlockIndex != 3 {
		t.Fatalf("wrong diff location: %+v", diff)
	}
	if diff.Stage != "pixel_quantization" || diff.Left != uint16(0) || diff.Right != uint16(77) {
		t.Fatalf("wrong diff detail: %+v", diff)
	}
}

func TestDecodeErrorCode(t *testing.T) {
	_, err := tex.DecodeImageFile("/definitely/missing/image.twimg")
	var texErr *tex.Error
	if !errors.As(err, &texErr) || texErr.Code != tex.CodeDecode {
		t.Fatalf("missing image should return E_DECODE, got %v", err)
	}
}

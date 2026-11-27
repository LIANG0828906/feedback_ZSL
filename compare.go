package volumefog

import (
	"fmt"
	"math"
)

const (
	PreviewColorTolerance         = 0.02
	PreviewTransmittanceTolerance = 0.01
	PixelIdentityTolerance        = 1e-12
)

type PixelMismatch struct {
	Chunk     ChunkCoord `json:"chunk"`
	PixelX    int        `json:"pixel_x"`
	PixelY    int        `json:"pixel_y"`
	Field     string     `json:"field"`
	Expected  float64    `json:"expected"`
	Actual    float64    `json:"actual"`
	Deviation float64    `json:"deviation"`
}

func frameChunkMap(frame *Frame) map[ChunkCoord]ChunkResult {
	out := make(map[ChunkCoord]ChunkResult, len(frame.Chunks))
	for _, ch := range frame.Chunks {
		out[ChunkCoord{X: ch.ChunkX, Y: ch.ChunkY}] = ch
	}
	return out
}

func firstFrameChunk(frame *Frame) ChunkCoord {
	if frame != nil && len(frame.Chunks) > 0 {
		return ChunkCoord{X: frame.Chunks[0].ChunkX, Y: frame.Chunks[0].ChunkY}
	}
	return ChunkCoord{}
}

// ComparePreviewExact enforces the preview/exact consistency contract.
func ComparePreviewExact(preview, exact *Frame) error {
	if preview == nil || exact == nil {
		return &FogError{Code: E_DIVERGE, Message: "missing preview or exact frame"}
	}
	if preview.SceneVersion != exact.SceneVersion || preview.MediaVersion != exact.MediaVersion ||
		preview.CameraVersion != exact.CameraVersion || preview.RecipeVersion != exact.RecipeVersion {
		c := firstFrameChunk(exact)
		return &FogError{Code: E_DIVERGE, Message: "preview and exact frames use different snapshot versions",
			Chunk: &c}
	}
	em := frameChunkMap(exact)
	for _, p := range preview.Chunks {
		c := ChunkCoord{X: p.ChunkX, Y: p.ChunkY}
		e, ok := em[c]
		if !ok {
			cc := c
			return &FogError{Code: E_DIVERGE, Message: "exact frame is missing a preview chunk", Chunk: &cc}
		}
		dt := absFloat(p.CenterTransmittance - e.CenterTransmittance)
		if dt > PreviewTransmittanceTolerance {
			cc := c
			return &FogError{Code: E_DIVERGE, Message: "preview/exact center transmittance diverged",
				Chunk: &cc, Field: "transmittance", Deviation: floatPtr(dt),
				Tolerance: floatPtr(PreviewTransmittanceTolerance),
				Actual:    floatPtr(p.CenterTransmittance), Expected: floatPtr(e.CenterTransmittance)}
		}
		for k := 0; k < 3; k++ {
			dc := absFloat(p.CenterColor[k] - e.CenterColor[k])
			if dc > PreviewColorTolerance {
				cc := c
				return &FogError{Code: E_DIVERGE, Message: "preview/exact center color diverged",
					Chunk: &cc, Field: fmt.Sprintf("color[%d]", k), Deviation: floatPtr(dc),
					Tolerance: floatPtr(PreviewColorTolerance),
					Actual:    floatPtr(p.CenterColor[k]), Expected: floatPtr(e.CenterColor[k])}
			}
		}
	}
	return nil
}

// CompareFrames locates the first pixel difference between two frames.
func CompareFrames(a, b *Frame) error {
	if a == nil || b == nil {
		return &FogError{Code: E_DIVERGE, Message: "nil frame in comparison"}
	}
	if len(a.Chunks) != len(b.Chunks) {
		return &FogError{Code: E_DIVERGE, Message: "frame chunk counts differ", Tolerance: floatPtr(0)}
	}
	for i := range a.Chunks {
		ac, bc := a.Chunks[i], b.Chunks[i]
		if ac.ChunkX != bc.ChunkX || ac.ChunkY != bc.ChunkY {
			return &FogError{Code: E_DIVERGE, Message: "frame chunk ordering differs",
				Chunk: &ChunkCoord{X: ac.ChunkX, Y: ac.ChunkY}}
		}
		if len(ac.Pixels) != len(bc.Pixels) {
			return &FogError{Code: E_DIVERGE, Message: "chunk pixel count differs",
				Chunk: &ChunkCoord{X: ac.ChunkX, Y: ac.ChunkY}}
		}
		for j := range ac.Pixels {
			ap, bp := ac.Pixels[j], bc.Pixels[j]
			if ap.X != bp.X || ap.Y != bp.Y {
				return &FogError{Code: E_DIVERGE, Message: "chunk pixel ordering differs",
					Chunk: &ChunkCoord{X: ac.ChunkX, Y: ac.ChunkY}}
			}
			type pair struct {
				name string
				a, b float64
			}
			vals := []pair{
				{"transmittance", ap.Transmittance, bp.Transmittance},
				{"scattering", ap.Scattering, bp.Scattering},
				{"color[0]", ap.Color[0], bp.Color[0]},
				{"color[1]", ap.Color[1], bp.Color[1]},
				{"color[2]", ap.Color[2], bp.Color[2]},
			}
			for _, v := range vals {
				d := math.Abs(v.a - v.b)
				if d > PixelIdentityTolerance {
					c := ChunkCoord{X: ac.ChunkX, Y: ac.ChunkY}
					return &FogError{Code: E_DIVERGE, Message: "pixel results differ", Chunk: &c,
						Field:  fmt.Sprintf("pixel(%d,%d).%s", ap.X, ap.Y, v.name),
						Actual: floatPtr(v.a), Expected: floatPtr(v.b), Deviation: floatPtr(d),
						Tolerance: floatPtr(PixelIdentityTolerance)}
				}
			}
		}
	}
	return nil
}

func CompareCommitSequences(a, b []int64) error {
	if len(a) != len(b) {
		return &FogError{Code: E_DIVERGE, Message: "commit sequence lengths differ"}
	}
	for i := range a {
		if a[i] != b[i] {
			return &FogError{Code: E_DIVERGE, Message: "commit sequence differs",
				Actual: floatPtr(float64(a[i])), Expected: floatPtr(float64(b[i]))}
		}
	}
	return nil
}

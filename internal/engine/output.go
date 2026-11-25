package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"uvenhance/internal/apperr"
	"uvenhance/internal/yuv"
)

type HeatmapStats struct {
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	NormMin  float64 `json:"norm_min"`
	NormMax  float64 `json:"norm_max"`
	Digest   string  `json:"digest"`
	MeanHeat float64 `json:"mean_heat_value_0_255"`
	MaxHeat  float64 `json:"max_heat_value_0_255"`
	NonZero  int     `json:"non_zero_pixels"`
}

func ValidateOutput(sample *yuv.Sample, img *yuv.Image) error {
	if img == nil {
		return apperr.New(apperr.CodeInvariant, "enhanced image is nil")
	}
	if img.Width != sample.Metadata.Width || img.Height != sample.Metadata.Height {
		return apperr.Plane("YUV", fmt.Sprintf("%dx%d", sample.Metadata.Width, sample.Metadata.Height),
			fmt.Sprintf("%dx%d", img.Width, img.Height), "final output dimensions differ from truth")
	}
	if img.BitDepth != sample.Metadata.BitDepth {
		return apperr.Plane("YUV", fmt.Sprintf("bit_depth=%d", sample.Metadata.BitDepth),
			fmt.Sprintf("bit_depth=%d", img.BitDepth), "final output bit depth differs from truth")
	}
	if img.QuantMin != sample.Metadata.QuantMin || img.QuantMax != sample.Metadata.QuantMax {
		return apperr.Plane("YUV", "declared quant range", "different quant range", "final output quant range differs")
	}
	return nil
}

func WriteYUV444(path string, img *yuv.Image) error {
	bps := 1
	if img.BitDepth > 8 {
		bps = 2
	}
	raw := make([]byte, len(img.Pixels)*3*bps)
	pos := 0
	for _, p := range img.Pixels {
		for _, v := range []uint16{p.Y, p.U, p.V} {
			if bps == 1 {
				raw[pos] = byte(v)
				pos++
			} else {
				binary.LittleEndian.PutUint16(raw[pos:pos+2], v)
				pos += 2
			}
		}
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return apperr.IO("write yuv444 output", err)
	}
	return nil
}

func WriteHeatmap(ctx context.Context, path string, original, enhanced *yuv.Image, normMin, normMax float64) (*HeatmapStats, error) {
	if original.Width != enhanced.Width || original.Height != enhanced.Height {
		return nil, apperr.Diff("heatmap", 0, 0, "original/enhanced dimensions differ")
	}
	if normMin < 0 || normMax <= normMin || normMax > 1 {
		return nil, apperr.Para("heat_norm", "normalization must satisfy 0 <= min < max <= 1")
	}
	width, height := enhanced.Width, enhanced.Height
	out := make([]byte, width*height)
	fullRange := int64(enhanced.QuantMax - enhanced.QuantMin)
	if fullRange <= 0 {
		return nil, apperr.Meta("quant_min/quant_max", "invalid quant range")
	}
	minQ := int64(normMin * 1_000_000)
	maxQ := int64(normMax * 1_000_000)
	var sum, peak int64
	nonZero := 0
	for i := 0; i < width*height; i++ {
		if i%(width*32) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, apperr.Canceled(err)
			}
		}
		a, b := original.Pixels[i], enhanced.Pixels[i]
		diff := abs64(int64(a.Y)-int64(b.Y)) + abs64(int64(a.U)-int64(b.U)) + abs64(int64(a.V)-int64(b.V))
		normQ := diff * 1_000_000 / (3 * fullRange)
		var heat int64
		if normQ <= minQ {
			heat = 0
		} else if normQ >= maxQ {
			heat = 255
		} else {
			heat = (normQ - minQ) * 255 / (maxQ - minQ)
		}
		out[i] = byte(heat)
		sum += heat
		if heat > peak {
			peak = heat
		}
		if heat != 0 {
			nonZero++
		}
	}
	header := fmt.Sprintf("P5\n%d %d\n255\n", width, height)
	if err := os.WriteFile(path, append([]byte(header), out...), 0o644); err != nil {
		return nil, apperr.IO("write heatmap", err)
	}
	return &HeatmapStats{
		Width: width, Height: height, NormMin: normMin, NormMax: normMax,
		Digest: yuv.HashBytes(out), MeanHeat: float64(sum) / float64(len(out)),
		MaxHeat: float64(peak), NonZero: nonZero,
	}, nil
}

func WriteArtifacts(ctx context.Context, outDir string, sample *yuv.Sample, result *RunResult,
	normMin, normMax float64) (*HeatmapStats, error) {
	if err := ValidateOutput(sample, result.Final); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, apperr.IO("create output directory", err)
	}
	enhancedPath := filepath.Join(outDir, "enhanced.yuv444p")
	if err := WriteYUV444(enhancedPath, result.Final); err != nil {
		return nil, err
	}
	original, err := yuv.BuildOriginalImage(ctx, sample)
	if err != nil {
		return nil, err
	}
	if err := WriteYUV444(filepath.Join(outDir, "original.yuv444p"), original); err != nil {
		return nil, err
	}
	stats, err := WriteHeatmap(ctx, filepath.Join(outDir, "difference.pgm"), original, result.Final, normMin, normMax)
	if err != nil {
		return nil, err
	}
	return stats, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

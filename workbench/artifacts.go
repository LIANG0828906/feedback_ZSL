package workbench

import (
	"image"
	"os"
	"path/filepath"

	"yuvench/yuv"
)

func WriteArtifacts(dir string, result *RunResult, heatmap *image.RGBA) error {
	if result == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if result.Original != nil {
		if err := yuv.WritePNG(filepath.Join(dir, "original.png"), yuv.CompositePNG(result.Original)); err != nil {
			return err
		}
	}
	if result.Final != nil {
		if err := yuv.WritePNG(filepath.Join(dir, "enhanced.png"), yuv.CompositePNG(result.Final)); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "enhanced_yuv444p16le.raw"), result.Final.EncodeRaw16LE(), 0o644); err != nil {
			return err
		}
	}
	if heatmap != nil {
		if err := yuv.WritePNG(filepath.Join(dir, "heatmap.png"), heatmap); err != nil {
			return err
		}
	}
	return nil
}

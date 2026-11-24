package report

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"audioeval/internal/model"
)

func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	return writeAtomic(path, data)
}

func WriteHeatmap(path string, report model.QualityReport) error {
	heightCells := 1
	for _, frame := range report.Frames {
		if len(frame.Blocks) > heightCells {
			heightCells = len(frame.Blocks)
		}
	}
	widthCells := max(1, len(report.Frames))
	cell := 24
	img := image.NewRGBA(image.Rect(0, 0, widthCells*cell, heightCells*cell))
	maxRMSE := 1.0
	values := make([][]float64, len(report.Frames))
	for frameIndex, frame := range report.Frames {
		values[frameIndex] = make([]float64, len(frame.Blocks))
		for blockIndex, block := range frame.Blocks {
			count := block.CoreEnd - block.CoreStart
			if count <= 0 {
				continue
			}
			rmse := math.Sqrt(float64(block.SampleErrorSSE) / float64(count))
			values[frameIndex][blockIndex] = rmse
			if rmse > maxRMSE {
				maxRMSE = rmse
			}
		}
	}
	for x := range values {
		for y := range values[x] {
			intensity := values[x][y] / maxRMSE
			if intensity < 0 {
				intensity = 0
			}
			if intensity > 1 {
				intensity = 1
			}
			c := heatColor(intensity)
			for py := y * cell; py < (y+1)*cell; py++ {
				for px := x * cell; px < (x+1)*cell; px++ {
					img.SetRGBA(px, py, c)
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create heatmap directory: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create heatmap: %w", err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		return fmt.Errorf("encode heatmap: %w", err)
	}
	return nil
}

func heatColor(value float64) color.RGBA {
	// Blue (0) -> cyan -> yellow -> red (1).
	r := clampByte(255 * math.Min(1, value*2))
	g := clampByte(255 * math.Min(1, math.Max(0, 2-value*2)))
	b := clampByte(255 * math.Max(0, 1-value*2))
	return color.RGBA{R: r, G: g, B: b, A: 255}
}

func clampByte(value float64) uint8 {
	if value <= 0 {
		return 0
	}
	if value >= 255 {
		return 255
	}
	return uint8(value)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return fmt.Errorf("create temporary report: %w", err)
	}
	tempName := temp.Name()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return fmt.Errorf("write temporary report: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return fmt.Errorf("sync temporary report: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("close temporary report: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tempName)
		return fmt.Errorf("remove previous report: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}

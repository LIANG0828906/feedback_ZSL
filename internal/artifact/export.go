package artifact

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"terrainpipeline/internal/model"
)

func ExportBlockArtifacts(root string, result *model.BlockResult) error {
	normalImg := image.NewRGBA(image.Rect(0, 0, result.NX, result.NZ))
	slopeImg := image.NewRGBA(image.Rect(0, 0, result.NX, result.NZ))
	for _, pixel := range result.Pixels {
		normalImg.SetRGBA(pixel.I, pixel.J, color.RGBA{R: pixel.RGB[0], G: pixel.RGB[1], B: pixel.RGB[2], A: 255})
		slopeImg.SetRGBA(pixel.I, pixel.J, slopeColor(pixel.SlopeDeg, 45))
	}
	if err := writePNG(filepath.Join(root, "normal", result.BlockID+".png"), normalImg); err != nil {
		return err
	}
	if err := writePNG(filepath.Join(root, "slope", result.BlockID+".png"), slopeImg); err != nil {
		return err
	}
	return writeJSON(filepath.Join(root, "data", result.BlockID+".json"), result)
}

func ExportSeams(root string, seams []*model.SeamReport) error {
	return writeJSON(filepath.Join(root, "seams.json"), seams)
}

func ExportJSON(path string, value any) error {
	return writeJSON(path, value)
}

func writePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func slopeColor(slope, maxSlope float64) color.RGBA {
	t := slope / maxSlope
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	stops := [][3]uint8{
		{28, 32, 120},
		{34, 120, 180},
		{80, 200, 180},
		{240, 230, 90},
		{225, 90, 50},
		{120, 20, 40},
	}
	scaled := t * float64(len(stops)-1)
	idx := int(scaled)
	if idx >= len(stops)-1 {
		c := stops[len(stops)-1]
		return color.RGBA{R: c[0], G: c[1], B: c[2], A: 255}
	}
	frac := scaled - float64(idx)
	a := stops[idx]
	b := stops[idx+1]
	return color.RGBA{
		R: uint8(float64(a[0]) + frac*float64(b[0]-a[0])),
		G: uint8(float64(a[1]) + frac*float64(b[1]-a[1])),
		B: uint8(float64(a[2]) + frac*float64(b[2]-a[2])),
		A: 255,
	}
}

package exportpng

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"

	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/model"
)

func Write(project *model.Project, snapshot engine.Snapshot, dir string) (map[string]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.ErrIO, -1, "exportDir", err.Error())
	}
	maxX, maxY := 0, 0
	for _, block := range project.Blocks {
		if block.OriginX+block.Width > maxX {
			maxX = block.OriginX + block.Width
		}
		if block.OriginY+block.Height > maxY {
			maxY = block.OriginY + block.Height
		}
	}
	normalImage := image.NewNRGBA(image.Rect(0, 0, maxX, maxY))
	slopeImage := image.NewNRGBA(image.Rect(0, 0, maxX, maxY))
	ids := make([]int, 0, len(snapshot.Blocks))
	for id := range snapshot.Blocks {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		result := snapshot.Blocks[id]
		for y := 0; y < result.Height; y++ {
			for x := 0; x < result.Width; x++ {
				pixel := y*result.Width + x
				offset := pixel * 3
				normalImage.SetNRGBA(result.OriginX+x, result.OriginY+y, color.NRGBA{
					R: result.Colors[offset],
					G: result.Colors[offset+1],
					B: result.Colors[offset+2],
					A: 255,
				})
				slopeImage.SetNRGBA(result.OriginX+x, result.OriginY+y, slopeColor(result.Slopes[pixel]))
			}
		}
	}
	paths := map[string]string{
		"normalPng": filepath.Join(dir, "normal-map.png"),
		"slopePng":  filepath.Join(dir, "slope-heatmap.png"),
	}
	if err := writePNG(paths["normalPng"], normalImage); err != nil {
		return nil, err
	}
	if err := writePNG(paths["slopePng"], slopeImage); err != nil {
		return nil, err
	}
	return paths, nil
}

func slopeColor(slope float64) color.NRGBA {
	t := math.Max(0, math.Min(1, slope/60))
	if t < 0.5 {
		u := t * 2
		return color.NRGBA{
			R: uint8(30 + 120*u),
			G: uint8(90 + 120*u),
			B: uint8(220 - 150*u),
			A: 255,
		}
	}
	u := (t - 0.5) * 2
	return color.NRGBA{
		R: uint8(150 + 105*u),
		G: uint8(210 - 170*u),
		B: uint8(70 - 50*u),
		A: 255,
	}
}

func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return model.NewError(model.ErrIO, -1, "export", err.Error())
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		return model.NewError(model.ErrIO, -1, "export", err.Error())
	}
	return nil
}

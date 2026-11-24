package engine

import (
	"math"

	"volumediag/internal/volume"
)

func (e *Engine) Image() ImageData {
	e.mu.RLock()
	defer e.mu.RUnlock()
	width := e.scene.Viewport.Width
	height := e.scene.Viewport.Height
	image := ImageData{
		CameraID:  e.scene.CameraPath[e.cameraIndex].ID,
		Width:     width,
		Height:    height,
		Volume:    make([]float64, width*height*3),
		Composite: make([]float64, width*height*3),
		TruthDiff: make([]float64, width*height*3),
		Base:      make([]float64, width*height*3),
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			base := volume.GeometryColor(e.scene, e.cameraIndex, x, y)
			off := (y*width + x) * 3
			image.Base[off] = base[0]
			image.Base[off+1] = base[1]
			image.Base[off+2] = base[2]
			key := volume.BlockKey{
				CameraIndex: e.cameraIndex,
				CameraID:    image.CameraID,
				X:           x / e.scene.BlockSize.Width,
				Y:           y / e.scene.BlockSize.Height,
			}
			block := e.cache[key]
			if block == nil {
				continue
			}
			for i := range block.Pixels {
				if block.Pixels[i].X != x || block.Pixels[i].Y != y {
					continue
				}
				p := block.Pixels[i]
				for c := 0; c < 3; c++ {
					image.Volume[off+c] = p.Volume[c]
					image.Composite[off+c] = block.Composite[i][c]
				}
				break
			}
		}
	}
	for _, p := range e.bundle.Truth.Pixels {
		if p.CameraID != image.CameraID {
			continue
		}
		if p.X < 0 || p.Y < 0 || p.X >= width || p.Y >= height {
			continue
		}
		off := (p.Y*width + p.X) * 3
		for c := 0; c < 3; c++ {
			image.TruthDiff[off+c] = math.Abs(image.Volume[off+c] - p.Volume[c])
		}
	}
	return image
}

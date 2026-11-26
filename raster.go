package maskqa

import (
	"image"
	"image/color"
	"math"
)

// RasterizeStroke renders one deterministic coverage component.
func RasterizeStroke(stroke Stroke, width, height int) (*image.Gray, error) {
	if err := stroke.ValidateParameters("stroke"); err != nil {
		return nil, err
	}
	out := image.NewGray(image.Rect(0, 0, width, height))
	bounds := StrokeBounds(stroke).Intersect(Rect{MaxX: width, MaxY: height})
	if bounds.Empty() {
		return out, nil
	}
	for y := bounds.MinY; y < bounds.MaxY; y++ {
		for x := bounds.MinX; x < bounds.MaxX; x++ {
			a := strokeCoverageAt(stroke, x, y)
			out.SetGray(x, y, color.Gray{Y: a})
		}
	}
	return out, nil
}

// RasterizeStrokeTile renders one stroke into a tile-local packed alpha buffer.
func RasterizeStrokeTile(stroke Stroke, tile Tile, buffer []byte) error {
	if err := stroke.ValidateParameters("stroke"); err != nil {
		return err
	}
	if len(buffer) < tile.PixelCount {
		return paraError("buffer.pixel_capacity", int64(tile.PixelCount), math.MaxInt32,
			"buffer length %d is smaller than tile pixels %d", len(buffer), tile.PixelCount)
	}
	for i := range buffer[:tile.PixelCount] {
		buffer[i] = 0
	}
	bounds := StrokeBounds(stroke).Intersect(tile.Rect)
	if bounds.Empty() {
		return nil
	}
	tileWidth := tile.Rect.Width()
	for y := bounds.MinY; y < bounds.MaxY; y++ {
		row := (y - tile.MinY) * tileWidth
		for x := bounds.MinX; x < bounds.MaxX; x++ {
			idx := row + (x - tile.MinX)
			buffer[idx] = strokeCoverageAt(stroke, x, y)
		}
	}
	return nil
}

func strokeCoverageAt(stroke Stroke, x, y int) byte {
	ax, ay := stroke.Start.X, stroke.Start.Y
	bx, by := stroke.End.X, stroke.End.Y
	dx, dy := bx-ax, by-ay
	px, py := x-ax, y-ay
	length2 := dx*dx + dy*dy
	dist2 := 0
	if length2 == 0 {
		dist2 = px*px + py*py
	} else {
		dot := px*dx + py*dy
		switch {
		case dot <= 0:
			dist2 = px*px + py*py
		case dot >= length2:
			ex, ey := x-bx, y-by
			dist2 = ex*ex + ey*ey
		default:
			cross := dx*py - dy*px
			dist2 = (cross * cross) / length2
		}
	}

	radius := stroke.WidthPx / 2
	if radius < 1 {
		radius = 1
	}
	outer2 := radius * radius
	if dist2 >= outer2 {
		return 0
	}
	maxAlpha := int(stroke.WeightPermyriad) * 255 / 10000
	if maxAlpha <= 0 {
		return 0
	}
	hardRadius := radius * stroke.HardnessPermyriad / 10000
	hard2 := hardRadius * hardRadius
	if dist2 <= hard2 {
		return byte(maxAlpha)
	}
	denom := outer2 - hard2
	if denom <= 0 {
		return byte(maxAlpha)
	}
	alpha := maxAlpha * (outer2 - dist2) / denom
	if alpha > maxAlpha {
		alpha = maxAlpha
	}
	if alpha < 0 {
		return 0
	}
	return byte(alpha)
}

// AlphaOver applies the deterministic rounded 8-bit source-over operator.
func AlphaOver(dst, src byte) byte {
	if src == 0 {
		return dst
	}
	if src == 255 {
		return 255
	}
	d := int(dst)
	s := int(src)
	value := s*255 + d*(255-s)
	return byte((value + 127) / 255)
}

// CompositeTileOver composites packed src alpha over packed dst in place.
func CompositeTileOver(dst []byte, src []byte) {
	for i := range dst {
		dst[i] = AlphaOver(dst[i], src[i])
	}
}

// GrayAt returns a safe gray value.
func GrayAt(img *image.Gray, x, y int) byte {
	if img == nil || !image.Pt(x, y).In(img.Bounds()) {
		return 0
	}
	return img.GrayAt(x, y).Y
}

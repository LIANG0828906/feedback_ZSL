package engine

import (
	"fmt"

	"maskqa/internal/model"
)

const MaxBrushWidth = 64

func ValidateStroke(s model.Stroke) *model.Error {
	if s.WeightBP < 0 || s.WeightBP > model.WeightScaleBP {
		return model.NewError(model.CodePara, "weight_bp", s.ID, "weight %d outside declared scale 0..%d", s.WeightBP, model.WeightScaleBP)
	}
	if s.HardnessBP < 0 || s.HardnessBP > model.HardnessScaleBP {
		return model.NewError(model.CodePara, "hardness_bp", s.ID, "hardness %d outside declared scale 0..%d", s.HardnessBP, model.HardnessScaleBP)
	}
	if s.WidthPx <= 0 || s.WidthPx > MaxBrushWidth {
		return model.NewError(model.CodePara, "width_px", s.ID, "brush width %d outside 1..%d", s.WidthPx, MaxBrushWidth)
	}
	return nil
}

func OrderedStrokes(strokes []model.Stroke) []model.Stroke {
	out := make([]model.Stroke, len(strokes))
	copy(out, strokes)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if strokeLess(out[j], out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func strokeLess(a, b model.Stroke) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if a.No != b.No {
		return a.No < b.No
	}
	return a.ID < b.ID
}

func StrokeFootprint(s model.Stroke) model.BBox {
	radius := BrushRadius(s.WidthPx)
	minX := minInt(s.From.X, s.To.X) - radius
	maxX := maxInt(s.From.X, s.To.X) + radius
	minY := minInt(s.From.Y, s.To.Y) - radius
	maxY := maxInt(s.From.Y, s.To.Y) + radius
	return model.BBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

func BrushRadius(widthPx int) int {
	if widthPx <= 1 {
		return 0
	}
	return widthPx / 2
}

func StrokeIntersectsChunk(s model.Stroke, c model.Chunk) bool {
	b := StrokeFootprint(s)
	return b.MinX < c.X1 && b.MaxX >= c.X0 && b.MinY < c.Y1 && b.MaxY >= c.Y0
}

func AffectedChunks(s model.Stroke, chunks []model.Chunk) []string {
	var ids []string
	for _, c := range chunks {
		if StrokeIntersectsChunk(s, c) {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// RasterizeStrokeChunk renders one stroke into chunk-local alpha coverage.
// All geometry and profiles use integer arithmetic; repeated calls are byte-identical.
func RasterizeStrokeChunk(s model.Stroke, c model.Chunk, pixelBufferCap int) ([]byte, error) {
	if verr := ValidateStroke(s); verr != nil {
		return nil, verr
	}
	chunkPixels := c.Width() * c.Height()
	if pixelBufferCap > 0 && chunkPixels > pixelBufferCap {
		return nil, model.NewError(model.CodePara, "pixel_buffer", s.ID, "chunk needs %d pixels, per-thread cap is %d", chunkPixels, pixelBufferCap)
	}
	scratch := make([]byte, 2*chunkPixels)
	out, err := RasterizeStrokeChunkBuffered(s, c, scratch)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), out...), nil
}

// RasterizeStrokeChunkBuffered uses scratch as two chunk-sized byte planes.
// The returned slice aliases the second half of scratch; callers must copy it
// before the next task reuses scratch.
func RasterizeStrokeChunkBuffered(s model.Stroke, c model.Chunk, scratch []byte) ([]byte, error) {
	if verr := ValidateStroke(s); verr != nil {
		return nil, verr
	}
	chunkPixels := c.Width() * c.Height()
	if len(scratch) < 2*chunkPixels {
		return nil, model.NewError(model.CodePara, "pixel_buffer", s.ID, "scratch has %d bytes, need %d", len(scratch), 2*chunkPixels)
	}
	profile := scratch[:chunkPixels]
	out := scratch[chunkPixels : 2*chunkPixels]
	for i := range profile {
		profile[i] = 0
	}
	for i := range out {
		out[i] = 0
	}
	radius := BrushRadius(s.WidthPx)
	innerRadius := radius * s.HardnessBP / model.HardnessScaleBP
	inner2 := innerRadius * innerRadius
	outer := radius + 1
	outer2 := outer * outer
	span := outer2 - inner2

	for _, p := range linePoints(s.From, s.To) {
		for dy := -radius; dy <= radius; dy++ {
			globalY := p.Y + dy
			if globalY < c.Y0 || globalY >= c.Y1 {
				continue
			}
			for dx := -radius; dx <= radius; dx++ {
				globalX := p.X + dx
				if globalX < c.X0 || globalX >= c.X1 {
					continue
				}
				d2 := dx*dx + dy*dy
				var v int
				switch {
				case d2 <= inner2:
					v = 255
				case d2 >= outer2:
					continue
				default:
					if span <= 0 {
						v = 0
					} else {
						v = 255 * (outer2 - d2) / span
					}
				}
				localIdx := (globalY-c.Y0)*c.Width() + (globalX - c.X0)
				if v > int(profile[localIdx]) {
					profile[localIdx] = byte(v)
				}
			}
		}
	}

	for i, p := range profile {
		if p == 0 {
			continue
		}
		alpha := int(p) * s.WeightBP / model.WeightScaleBP
		if alpha > 255 {
			alpha = 255
		}
		out[i] = byte(alpha)
	}
	return out, nil
}

func linePoints(a, b model.Point) []model.Point {
	x0, y0 := a.X, a.Y
	x1, y1 := b.X, b.Y
	dx := absInt(x1 - x0)
	dy := -absInt(y1 - y0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	var pts []model.Point
	for {
		pts = append(pts, model.Point{X: x0, Y: y0})
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
	return pts
}

func OverAlpha(dst, src byte) byte {
	s := int(src)
	d := int(dst)
	out := (s*255 + d*(255-s) + 127) / 255
	if out > 255 {
		out = 255
	}
	return byte(out)
}

func ApplyComponent(dst []byte, c model.Chunk, component []byte) error {
	if len(dst) != c.Width()*c.Height() || len(component) != len(dst) {
		return fmt.Errorf("chunk buffer length mismatch: dst=%d component=%d expected=%d", len(dst), len(component), c.Width()*c.Height())
	}
	for i, src := range component {
		dst[i] = OverAlpha(dst[i], src)
	}
	return nil
}

func ChunkCoverageFromMask(m model.Mask, c model.Chunk) []byte {
	out := make([]byte, c.Width()*c.Height())
	for y := c.Y0; y < c.Y1; y++ {
		for x := c.X0; x < c.X1; x++ {
			out[(y-c.Y0)*c.Width()+(x-c.X0)] = m.At(x, y)
		}
	}
	return out
}

func WriteChunkIntoMask(m *model.Mask, c model.Chunk, pix []byte) error {
	if len(pix) != c.Width()*c.Height() {
		return fmt.Errorf("component length %d does not match chunk %s size %d", len(pix), c.ID, c.Width()*c.Height())
	}
	for y := c.Y0; y < c.Y1; y++ {
		for x := c.X0; x < c.X1; x++ {
			m.Pix[m.Index(x, y)] = pix[(y-c.Y0)*c.Width()+(x-c.X0)]
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

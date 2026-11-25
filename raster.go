package maskqc

import (
	"fmt"
	"math"
)

// ValidateStrokeParameters checks declared rasterizer ranges.
func ValidateStrokeParameters(s Stroke) *Error {
	if s.WeightPermille < MinPermille || s.WeightPermille > MaxPermille {
		return paramError("strokes.weight_permille", MinPermille, MaxPermille, "coverage weight is outside declared permille range")
	}
	if s.HardnessPermille < MinPermille || s.HardnessPermille > MaxPermille {
		return paramError(fmt.Sprintf("strokes[%d].hardness_permille", s.Seq), MinPermille, MaxPermille, "brush hardness is outside declared permille range")
	}
	if s.RadiusTick < MinRadiusTick || s.RadiusTick > MaxRadiusTick {
		return paramError(fmt.Sprintf("strokes[%d].radius_tick", s.Seq), MinRadiusTick, MaxRadiusTick, "radius tick is outside declared range")
	}
	return nil
}

// StrokeBounds returns the integer half-open region touched by a stroke.
func StrokeBounds(s Stroke, width, height int) Rect {
	radius := float64(s.RadiusTick)
	minX := int(math.Floor(math.Min(float64(s.Start.X), float64(s.End.X))-radius)) - 1
	minY := int(math.Floor(math.Min(float64(s.Start.Y), float64(s.End.Y))-radius)) - 1
	maxX := int(math.Ceil(math.Max(float64(s.Start.X), float64(s.End.X))+radius)) + 1
	maxY := int(math.Ceil(math.Max(float64(s.Start.Y), float64(s.End.Y))+radius)) + 1
	if minX < 0 {
		minX = 0
	}
	if minY < 0 {
		minY = 0
	}
	if maxX > width {
		maxX = width
	}
	if maxY > height {
		maxY = height
	}
	return Rect{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

// RasterizeStroke renders a stroke into its tight deterministic component.
func RasterizeStroke(s Stroke, width, height int) (*CoverageComponent, *Error) {
	if err := ValidateStrokeParameters(s); err != nil {
		return nil, err
	}
	bounds := StrokeBounds(s, width, height)
	alpha, err := RasterizeStrokeRegion(s, bounds)
	if err != nil {
		return nil, err
	}
	return &CoverageComponent{
		StrokeSeq:   s.Seq,
		Bounds:      bounds,
		Alpha:       alpha,
		ContentHash: hashBytes([]byte(fmt.Sprintf("stroke-%d-%s", s.Seq, AlgorithmVersion)), alpha),
	}, nil
}

// RasterizeStrokeRegion renders coverage for an arbitrary pixel rectangle.
// The same stroke and region always return byte-identical output.
func RasterizeStrokeRegion(s Stroke, region Rect) ([]byte, *Error) {
	if err := ValidateStrokeParameters(s); err != nil {
		return nil, err
	}
	w := region.Width()
	h := region.Height()
	if w <= 0 || h <= 0 {
		return []byte{}, nil
	}
	if w > math.MaxInt/h {
		return nil, &Error{Code: CodePara, Field: "region", Message: "region area overflows integer allocation"}
	}
	out := make([]byte, w*h)
	if err := RasterizeStrokeInto(s, region, out); err != nil {
		return nil, err
	}
	return out, nil
}

// RasterizeStrokeInto is the buffer-backed deterministic raster core.
func RasterizeStrokeInto(s Stroke, region Rect, out []byte) *Error {
	if err := ValidateStrokeParameters(s); err != nil {
		return err
	}
	w := region.Width()
	h := region.Height()
	if w <= 0 || h <= 0 {
		return nil
	}
	if len(out) < w*h {
		return &Error{Code: CodePara, Field: "pixel_buffer", LegalRange: [2]int{w * h, w * h}, Message: "pixel buffer is smaller than region"}
	}
	for i := 0; i < w*h; i++ {
		out[i] = 0
	}
	radius := float64(s.RadiusTick)
	hardness := float64(s.HardnessPermille) / 1000.0
	coreRadius := radius * hardness
	weight := float64(s.WeightPermille) / 1000.0

	vx := float64(s.End.X - s.Start.X)
	vy := float64(s.End.Y - s.Start.Y)
	lengthSquared := vx*vx + vy*vy

	for y := region.MinY; y < region.MaxY; y++ {
		row := (y - region.MinY) * w
		for x := region.MinX; x < region.MaxX; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			distance := segmentDistance(px, py, float64(s.Start.X)+0.5, float64(s.Start.Y)+0.5, vx, vy, lengthSquared)
			profile := brushProfile(distance, radius, coreRadius)
			if profile <= 0 {
				continue
			}
			coverage := profile * weight
			idx := row + (x - region.MinX)
			out[idx] = byte(math.Round(255.0 * coverage))
		}
	}
	return nil
}

func segmentDistance(px, py, sx, sy, vx, vy, lengthSquared float64) float64 {
	if lengthSquared == 0 {
		return math.Hypot(px-sx, py-sy)
	}
	t := ((px-sx)*vx + (py-sy)*vy) / lengthSquared
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	qx := sx + t*vx
	qy := sy + t*vy
	return math.Hypot(px-qx, py-qy)
}

func brushProfile(distance, radius, coreRadius float64) float64 {
	if distance >= radius {
		return 0
	}
	if distance <= coreRadius {
		return 1
	}
	if coreRadius >= radius {
		return 1
	}
	return (radius - distance) / (radius - coreRadius)
}

// ComposeAlphaOver performs rounded integer source-over alpha composition.
func ComposeAlphaOver(dst, source []byte) []byte {
	for i := range dst {
		var src byte
		if i < len(source) {
			src = source[i]
		}
		d := int(dst[i])
		s := int(src)
		out := s + (d*(255-s)+127)/255
		if out > 255 {
			out = 255
		}
		dst[i] = byte(out)
	}
	return dst
}

// ComposeRegionOver composes a same-sized source region into dst.
func ComposeRegionOver(dst []byte, dstRect, sourceRect Rect, source []byte) {
	clip := dstRect.Intersect(sourceRect)
	if clip.Empty() {
		return
	}
	srcW := sourceRect.Width()
	dstW := dstRect.Width()
	for y := clip.MinY; y < clip.MaxY; y++ {
		for x := clip.MinX; x < clip.MaxX; x++ {
			si := (y-sourceRect.MinY)*srcW + (x - sourceRect.MinX)
			di := (y-dstRect.MinY)*dstW + (x - dstRect.MinX)
			s := int(source[si])
			d := int(dst[di])
			out := s + (d*(255-s)+127)/255
			if out > 255 {
				out = 255
			}
			dst[di] = byte(out)
		}
	}
}

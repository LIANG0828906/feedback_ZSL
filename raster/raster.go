package raster

import (
	"fmt"

	"maskqc/model"
)

type Result struct {
	Accumulated  []byte            `json:"-"`
	PerStroke    map[string][]byte `json:"-"`
	Order        []string          `json:"order"`
	OrderVersion string            `json:"orderVersion"`
	MaskHash     string            `json:"maskHash"`
}

func ValidateStroke(stroke model.Stroke) error {
	if stroke.Radius > 1<<16 {
		return &model.Error{
			Code: model.CodeParam, Path: "stroke.radius", ObjectID: stroke.ID,
			Field: "radius", Message: "brush radius exceeds the safe integer range", Allowed: "1..65536",
		}
	}
	coordinates := []struct {
		field string
		value int
	}{
		{field: "x0", value: stroke.X0},
		{field: "y0", value: stroke.Y0},
		{field: "x1", value: stroke.X1},
		{field: "y1", value: stroke.Y1},
	}
	for _, coordinate := range coordinates {
		if coordinate.value < -(1<<20) || coordinate.value > 1<<20 {
			return &model.Error{
				Code: model.CodeParam, Path: "stroke." + coordinate.field, ObjectID: stroke.ID,
				Field: coordinate.field, Message: "stroke coordinate exceeds the safe integer range", Allowed: "-1048576..1048576",
			}
		}
	}
	if stroke.Radius <= 0 {
		return &model.Error{
			Code: model.CodeParam, Path: "stroke.radius", ObjectID: stroke.ID,
			Field: "radius", Message: "brush radius must be positive", Allowed: "1..1048576",
		}
	}
	if stroke.Hardness < 0 || stroke.Hardness > model.MaxHardness {
		return &model.Error{
			Code: model.CodeParam, Path: "stroke.hardness", ObjectID: stroke.ID,
			Field: "hardness", Message: "brush hardness is outside the allowed range", Allowed: "0..100",
		}
	}
	if stroke.Weight < 0 || stroke.Weight > model.MaxCoverage {
		return &model.Error{
			Code: model.CodeParam, Path: "stroke.weight", ObjectID: stroke.ID,
			Field: "weight", Message: "coverage weight is outside the allowed scale", Allowed: "0..10000",
		}
	}
	return nil
}

func Coverage(stroke model.Stroke, width, height int) ([]byte, error) {
	if err := ValidateStroke(stroke); err != nil {
		return nil, err
	}
	return CoverageRect(stroke, model.Rect{MinX: 0, MinY: 0, MaxX: width - 1, MaxY: height - 1}, width, height)
}

func CoverageRect(stroke model.Stroke, rect model.Rect, width, height int) ([]byte, error) {
	if err := ValidateStroke(stroke); err != nil {
		return nil, err
	}
	if width <= 0 || height <= 0 {
		return nil, &model.Error{
			Code: model.CodeParam, Path: "canvas", ObjectID: stroke.ID,
			Field: "canvas", Message: "canvas dimensions must be positive", Allowed: "width>0,height>0",
		}
	}
	clipped := rect.Clip(width, height)
	if clipped.Empty() {
		return nil, nil
	}
	outputWidth := clipped.Width()
	outputHeight := clipped.Height()
	coverage := make([]byte, outputWidth*outputHeight)
	alpha := weightAlpha(stroke.Weight)
	radius := int64(stroke.Radius)
	radiusSquared := radius * radius
	inner := radius * int64(stroke.Hardness) / int64(model.MaxHardness)
	innerSquared := inner * inner
	falloffSpan := radius - inner
	for y := clipped.MinY; y <= clipped.MaxY; y++ {
		for x := clipped.MinX; x <= clipped.MaxX; x++ {
			distanceSquared := pointSegmentDistanceSquared(int64(x), int64(y), int64(stroke.X0), int64(stroke.Y0), int64(stroke.X1), int64(stroke.Y1))
			var pixelAlpha int64
			switch {
			case distanceSquared <= innerSquared:
				pixelAlpha = alpha
			case distanceSquared >= radiusSquared:
				pixelAlpha = 0
			case falloffSpan <= 0:
				pixelAlpha = alpha
			default:
				distance := integerSqrt(distanceSquared)
				fall := (radius - distance) * 256 / falloffSpan
				fall = max(0, min(256, fall))
				pixelAlpha = alpha * fall / 256
			}
			coverage[(y-clipped.MinY)*outputWidth+(x-clipped.MinX)] = byte(max(0, min(255, pixelAlpha)))
		}
	}
	return coverage, nil
}

func Compose(candidate []byte, width, height int, strokes []model.Stroke) (*Result, error) {
	if width <= 0 || height <= 0 || len(candidate) != width*height {
		return nil, &model.Error{
			Code: model.CodeParam, Path: "candidate", ObjectID: "",
			Field: "candidate", Message: "candidate mask size does not match canvas",
		}
	}
	accumulated := append([]byte(nil), candidate...)
	perStroke := make(map[string][]byte, len(strokes))
	order := model.OrderedStrokes(strokes)
	ids := make([]string, 0, len(order))
	for _, stroke := range order {
		if err := ValidateStroke(stroke); err != nil {
			return nil, err
		}
		component, err := Coverage(stroke, width, height)
		if err != nil {
			return nil, err
		}
		perStroke[stroke.ID] = component
		ids = append(ids, stroke.ID)
		alphaOverInPlace(accumulated, component)
	}
	return &Result{
		Accumulated: accumulated, PerStroke: perStroke, Order: ids,
		OrderVersion: model.OrderVersion(strokes), MaskHash: model.HashBytes(accumulated),
	}, nil
}

func ComposeOrdered(candidate []byte, width, height int, strokes []model.Stroke, order []string) ([]byte, map[string][]byte, error) {
	if len(order) != len(strokes) {
		return nil, nil, &model.Error{
			Code: model.CodeParam, Path: "order", Field: "order",
			Message: "composite order length does not match stroke count",
		}
	}
	byID := make(map[string]model.Stroke, len(strokes))
	for _, stroke := range strokes {
		byID[stroke.ID] = stroke
	}
	accumulated := append([]byte(nil), candidate...)
	components := make(map[string][]byte, len(order))
	for _, id := range order {
		stroke, ok := byID[id]
		if !ok {
			return nil, nil, &model.Error{
				Code: model.CodeParam, Path: "order", ObjectID: id,
				Field: "order", Message: "composite order contains unknown stroke",
			}
		}
		component, err := Coverage(stroke, width, height)
		if err != nil {
			return nil, nil, err
		}
		components[id] = component
		alphaOverInPlace(accumulated, component)
	}
	return accumulated, components, nil
}

func AlphaOver(base, source byte) byte {
	value := int64(source) + (int64(base)*(255-int64(source))+127)/255
	return byte(max(0, min(255, value)))
}

func alphaOverInPlace(base, source []byte) {
	for index := range base {
		base[index] = AlphaOver(base[index], source[index])
	}
}

func weightAlpha(weight int) int64 {
	return (int64(weight)*255 + model.CoverageScale/2) / model.CoverageScale
}

func pointSegmentDistanceSquared(px, py, x0, y0, x1, y1 int64) int64 {
	dx := x1 - x0
	dy := y1 - y0
	if dx == 0 && dy == 0 {
		return squared(px-x0) + squared(py-y0)
	}
	lengthSquared := dx*dx + dy*dy
	t := ((px-x0)*dx + (py-y0)*dy) * 1024 / lengthSquared
	t = max(0, min(1024, t))
	nearestX := x0 + dx*t/1024
	nearestY := y0 + dy*t/1024
	return squared(px-nearestX) + squared(py-nearestY)
}

func squared(value int64) int64 {
	return value * value
}

func integerSqrt(value int64) int64 {
	if value <= 0 {
		return 0
	}
	low, high := int64(0), int64(1)
	for high*high < value {
		high *= 2
	}
	for low+1 < high {
		mid := (low + high) / 2
		if mid*mid <= value {
			low = mid
		} else {
			high = mid
		}
	}
	return low
}

func Describe(stroke model.Stroke) string {
	return fmt.Sprintf("%s:%d,%d->%d,%d r=%d h=%d w=%d p=%d", stroke.ID, stroke.X0, stroke.Y0, stroke.X1, stroke.Y1, stroke.Radius, stroke.Hardness, stroke.Weight, stroke.Priority)
}

package mirror

import (
	"fmt"
	"strconv"
)

// MapErrors records independently observable integer truncation sources.
// ScaleRounding* is expressed in hundredths of a pixel (denominator 100),
// while crop fields are measured in output pixel edges.
type MapErrors struct {
	CoordinateResidualX int64 `json:"coordinateResidualX"`
	CoordinateResidualY int64 `json:"coordinateResidualY"`
	ScaleRoundingX      int64 `json:"scaleRoundingX"`
	ScaleRoundingY      int64 `json:"scaleRoundingY"`
	ScaleDenominator    int64 `json:"scaleDenominator"`
	CropLeft            int64 `json:"cropLeft"`
	CropRight           int64 `json:"cropRight"`
	CropTop             int64 `json:"cropTop"`
	CropBottom          int64 `json:"cropBottom"`
	CropTruncationX     int64 `json:"cropTruncationX"`
	CropTruncationY     int64 `json:"cropTruncationY"`
	TimebaseResidualMs  int64 `json:"timebaseResidualMs"`
}

func (e MapErrors) CropTruncation() int64 {
	return e.CropTruncationX + e.CropTruncationY
}

// Mapping is one screen's deterministic integer conversion result.
type Mapping struct {
	WindowID         string    `json:"windowId"`
	ScreenID         string    `json:"screenId"`
	Input            Region    `json:"input"`
	Local            Region    `json:"local"`
	Scaled           Region    `json:"scaled"`
	Output           Region    `json:"output"`
	ScreenOriginX    int64     `json:"screenOriginX"`
	ScreenOriginY    int64     `json:"screenOriginY"`
	Scale            Scale     `json:"scale"`
	Chroma           Chroma    `json:"chroma"`
	ChromaAlignX     int64     `json:"chromaAlignX"`
	ChromaAlignY     int64     `json:"chromaAlignY"`
	Errors           MapErrors `json:"errors"`
	MappingSignature string    `json:"mappingSignature"`
}

// ErrorBound is the cumulative upper bound derived only from observed
// truncation residues and their declared denominators.
type ErrorBound struct {
	CoordinateResidual int64 `json:"coordinateResidual"`
	ScaleRounding      int64 `json:"scaleRounding"`
	ScaleDenominator   int64 `json:"scaleDenominator"`
	CropTruncation     int64 `json:"cropTruncation"`
	TimebaseResidualMs int64 `json:"timebaseResidualMs"`
}

func (b *ErrorBound) Add(e MapErrors) {
	b.CoordinateResidual += abs64(e.CoordinateResidualX) + abs64(e.CoordinateResidualY)
	b.ScaleRounding += abs64(e.ScaleRoundingX) + abs64(e.ScaleRoundingY)
	if e.ScaleDenominator > 0 {
		b.ScaleDenominator = e.ScaleDenominator
	}
	b.CropTruncation += abs64(e.CropTruncationX) + abs64(e.CropTruncationY)
	b.TimebaseResidualMs += abs64(e.TimebaseResidualMs)
}

// MapScreen maps a window master region into one screen's physical pixel
// space and clips it to the screen rectangle. No floating point is used.
func MapScreen(screen Screen, window Window) (Mapping, error) {
	m := Mapping{
		WindowID: window.ID, ScreenID: screen.ID,
		Input: window.Region, ScreenOriginX: screen.OriginX, ScreenOriginY: screen.OriginY,
		Scale: window.Scale, Chroma: window.Chroma,
		Errors: MapErrors{ScaleDenominator: 100, TimebaseResidualMs: abs64(window.SourceTimeResidualMs)},
	}
	if !screen.SupportsScale(window.Scale) {
		return Mapping{}, capabilityError(screen, window, "scale")
	}
	if !screen.SupportsChroma(window.Chroma) {
		return Mapping{}, capabilityError(screen, window, "chroma")
	}
	localX, err := SubChecked("windows."+window.ID+".region.x", window.Region.X, screen.OriginX)
	if err != nil {
		return Mapping{}, err
	}
	localY, err := SubChecked("windows."+window.ID+".region.y", window.Region.Y, screen.OriginY)
	if err != nil {
		return Mapping{}, err
	}
	if _, err := AddChecked("windows."+window.ID+".region.x2", window.Region.X, window.Region.W); err != nil {
		return Mapping{}, err
	}
	if _, err := AddChecked("windows."+window.ID+".region.y2", window.Region.Y, window.Region.H); err != nil {
		return Mapping{}, err
	}
	localX2, err := SubChecked("windows."+window.ID+".region.x2", window.Region.X2(), screen.OriginX)
	if err != nil {
		return Mapping{}, err
	}
	localY2, err := SubChecked("windows."+window.ID+".region.y2", window.Region.Y2(), screen.OriginY)
	if err != nil {
		return Mapping{}, err
	}
	m.Local = Region{X: localX, Y: localY, W: localX2 - localX, H: localY2 - localY}

	scaledX0, remX0, err := MulDivChecked("windows."+window.ID+".region.x", localX, int64(window.Scale), 100, RoundFloor)
	if err != nil {
		return Mapping{}, err
	}
	scaledY0, remY0, err := MulDivChecked("windows."+window.ID+".region.y", localY, int64(window.Scale), 100, RoundFloor)
	if err != nil {
		return Mapping{}, err
	}
	scaledX1, remX1, err := MulDivChecked("windows."+window.ID+".region.x2", localX2, int64(window.Scale), 100, RoundFloor)
	if err != nil {
		return Mapping{}, err
	}
	scaledY1, remY1, err := MulDivChecked("windows."+window.ID+".region.y2", localY2, int64(window.Scale), 100, RoundFloor)
	if err != nil {
		return Mapping{}, err
	}
	// MulDivChecked's floor path leaves a non-negative remainder relative to
	// the returned quotient for negative operands as well.
	m.Errors.ScaleRoundingX = abs64(remX0) + abs64(remX1)
	m.Errors.ScaleRoundingY = abs64(remY0) + abs64(remY1)
	if scaledX1 < scaledX0 || scaledY1 < scaledY0 {
		return Mapping{}, &Error{Code: CodeCapacity, ScreenID: screen.ID, WindowID: window.ID,
			Field: "scale", Message: "scaled region inverted"}
	}
	m.Scaled = Region{X: scaledX0, Y: scaledY0, W: scaledX1 - scaledX0, H: scaledY1 - scaledY0}

	alignX, alignY := window.Chroma.Align()
	m.ChromaAlignX, m.ChromaAlignY = alignX, alignY
	alignedX0 := floorTo(scaledX0, alignX)
	alignedY0 := floorTo(scaledY0, alignY)
	alignedX1 := floorTo(scaledX1, alignX)
	alignedY1 := floorTo(scaledY1, alignY)
	m.Errors.CropLeft = scaledX0 - alignedX0
	m.Errors.CropRight = scaledX1 - alignedX1
	m.Errors.CropTop = scaledY0 - alignedY0
	m.Errors.CropBottom = scaledY1 - alignedY1

	clipLeft := max64(0, -alignedX0)
	clipTop := max64(0, -alignedY0)
	clipRight := max64(0, alignedX1-screen.Width)
	clipBottom := max64(0, alignedY1-screen.Height)
	x0 := max64(0, alignedX0)
	y0 := max64(0, alignedY0)
	x1 := min64(screen.Width, alignedX1)
	y1 := min64(screen.Height, alignedY1)
	m.Errors.CropLeft += clipLeft
	m.Errors.CropTop += clipTop
	m.Errors.CropRight += clipRight
	m.Errors.CropBottom += clipBottom
	if x1 > x0 && alignX > 1 {
		if rem := (x1 - x0) % alignX; rem != 0 {
			x1 -= rem
			m.Errors.CropRight += rem
		}
	}
	if y1 > y0 && alignY > 1 {
		if rem := (y1 - y0) % alignY; rem != 0 {
			y1 -= rem
			m.Errors.CropBottom += rem
		}
	}
	m.Errors.CropTruncationX = abs64(m.Errors.CropLeft) + abs64(m.Errors.CropRight)
	m.Errors.CropTruncationY = abs64(m.Errors.CropTop) + abs64(m.Errors.CropBottom)
	if x1 <= x0 || y1 <= y0 {
		return Mapping{}, capabilityError(screen, window, "crop")
	}
	m.Output = Region{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
	m.MappingSignature = HashJSON(struct {
		ScreenID string    `json:"screenId"`
		WindowID string    `json:"windowId"`
		Output   Region    `json:"output"`
		Local    Region    `json:"local"`
		Scaled   Region    `json:"scaled"`
		Scale    Scale     `json:"scale"`
		Chroma   Chroma    `json:"chroma"`
		Errors   MapErrors `json:"errors"`
	}{screen.ID, window.ID, m.Output, m.Local, m.Scaled, m.Scale, m.Chroma, m.Errors})
	return m, nil
}

func capabilityError(screen Screen, window Window, field string) *Error {
	supported := make([]string, 0, len(screen.ScaleCapabilities))
	requested := window.Scale.String()
	msg := fmt.Sprintf("requested scale %s is outside screen capability", requested)
	if field == "chroma" {
		for _, v := range screen.ChromaCapabilities {
			supported = append(supported, string(v))
		}
		requested = string(window.Chroma)
		msg = fmt.Sprintf("requested chroma %s is not supported by screen", requested)
	} else {
		for _, v := range screen.ScaleCapabilities {
			supported = append(supported, v.String())
		}
	}
	if field == "crop" {
		msg = "mapped region is empty after clipping"
	}
	return &Error{Code: CodeCapacity, Path: "windows." + window.ID + ".region",
		ScreenID: screen.ID, WindowID: window.ID, Field: field,
		Requested: requested, Supported: supported, Message: msg}
}

func floorTo(v, quantum int64) int64 {
	if quantum <= 1 {
		return v
	}
	return FloorDiv(v, quantum) * quantum
}

// ScaleCapabilityRange describes the declared scale set as a stable string.
func ScaleCapabilityRange(screen Screen) string {
	if len(screen.ScaleCapabilities) == 0 {
		return "[]"
	}
	out := "["
	for i, v := range screen.ScaleCapabilities {
		if i > 0 {
			out += ","
		}
		out += strconv.FormatInt(int64(v), 10) + "%"
	}
	return out + "]"
}

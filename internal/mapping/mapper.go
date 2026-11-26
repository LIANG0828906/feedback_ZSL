package mapping

import (
	"fmt"
	"sort"

	"screenrelay/internal/model"
	"screenrelay/internal/xconv"
)

const roundingDen = model.InternalCoordUnit * 100

type Request struct {
	Region      model.Rect
	Screen      model.Screen
	Requested   model.ScaleTier
	TimeQuantum int64
	PacketBytes int64
}

type PixelRect struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

type Params struct {
	ScreenID         string          `json:"screen_id"`
	SourceRevision   int64           `json:"source_revision"`
	Scale            model.ScaleTier `json:"scale"`
	ScaleNumerator   int64           `json:"scale_numerator"`
	ScaleDenominator int64           `json:"scale_denominator"`
	CoordUnit        int64           `json:"coord_unit"`
	Rounding         string          `json:"rounding"`
	TimeQuantum      int64           `json:"time_quantum"`
	PacketBytes      int64           `json:"packet_bytes"`
	Chroma           model.Chroma    `json:"chroma"`
}

type Bounds struct {
	CoordRemainderUnits       int64 `json:"coord_remainder_units"`
	ScaleRoundingNumerator    int64 `json:"scale_rounding_numerator"`
	CoordinateScaledNumerator int64 `json:"coordinate_scaled_numerator"`
	GeometryChainNumerator    int64 `json:"geometry_chain_numerator"`
	ClipTruncationPixels      int64 `json:"clip_truncation_pixels"`
	TimeRemainderTicks        int64 `json:"time_remainder_ticks"`
	MaxPackets                int64 `json:"max_packets"`
}

type ErrorReport struct {
	ScreenID               string `json:"screen_id"`
	CoordRemainderUnits    int64  `json:"coord_remainder_units"`
	ScaleRoundingNumerator int64  `json:"scale_rounding_numerator"`
	ClipLeft               int64  `json:"clip_left"`
	ClipTop                int64  `json:"clip_top"`
	ClipRight              int64  `json:"clip_right"`
	ClipBottom             int64  `json:"clip_bottom"`
	ClipTruncationPixels   int64  `json:"clip_truncation_pixels"`
	TimeRemainderTicks     int64  `json:"time_remainder_ticks"`
	Bounds                 Bounds `json:"bounds"`
}

type Result struct {
	ScreenID    string      `json:"screen_id"`
	Active      bool        `json:"active"`
	LocalRegion model.Rect  `json:"local_region"`
	Unclipped   PixelRect   `json:"unclipped"`
	PixelRegion PixelRect   `json:"pixel_region"`
	Params      Params      `json:"params"`
	Errors      ErrorReport `json:"errors"`
}

func ValidateCapabilities(screen model.Screen) error {
	path := "$.screens." + screen.ID
	if screen.ID == "" {
		return model.SchemaError(path+".id", "screen id is required")
	}
	if screen.PixelW <= 0 || screen.PixelH <= 0 {
		return model.NewError(model.ECap, "screen %s has invalid resolution %dx%d", screen.ID, screen.PixelW, screen.PixelH)
	}
	if len(screen.SupportedScales) == 0 {
		return model.NewError(model.ECap, "screen %s declares no scale capabilities", screen.ID)
	}
	if !containsScale(screen.SupportedScales, screen.Scale) {
		return model.NewError(model.ECap, "screen %s current scale %s is absent from capabilities %v", screen.ID, screen.Scale, screen.SupportedScales)
	}
	if len(screen.SupportedChromas) == 0 {
		return model.NewError(model.ECap, "screen %s declares no chroma capabilities", screen.ID)
	}
	if !containsChroma(screen.SupportedChromas, screen.Chroma) {
		return model.NewError(model.ECap, "screen %s current chroma %s is absent from capabilities %v", screen.ID, screen.Chroma, screen.SupportedChromas)
	}
	return nil
}

func containsScale(items []model.ScaleTier, target model.ScaleTier) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func containsChroma(items []model.Chroma, target model.Chroma) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func MapScreen(req Request, tick int64) (*Result, error) {
	screen := req.Screen
	scale := req.Requested
	if scale == "" {
		scale = screen.Scale
	}
	if !containsScale(screen.SupportedScales, scale) {
		return nil, &model.Error{
			Code: model.EPara, ScreenID: screen.ID, Field: "scale_tier",
			Path:    "$.screens." + screen.ID + ".scale_tier",
			Message: "requested scale is outside declared capabilities",
			Actual:  fmt.Sprintf("requested=%s supported=%v resolution=%dx%d chroma=%s", scale, screen.SupportedScales, screen.PixelW, screen.PixelH, screen.SupportedChromas),
			Details: map[string]any{
				"requested": string(scale), "supported_scales": screen.SupportedScales,
				"pixel_width": screen.PixelW, "pixel_height": screen.PixelH,
				"supported_chromas": screen.SupportedChromas,
			},
		}
	}
	num, rationalDen, ok := scale.Rational()
	if !ok {
		return nil, model.NewError(model.EPara, "screen %s has invalid scale %q", screen.ID, scale)
	}
	scaleDen := model.InternalCoordUnit * rationalDen

	localX, err := xconv.SubChecked(req.Region.X, screen.Origin.X, "local.x")
	if err != nil {
		return nil, err
	}
	localY, err := xconv.SubChecked(req.Region.Y, screen.Origin.Y, "local.y")
	if err != nil {
		return nil, err
	}
	right, err := req.Region.Right()
	if err != nil {
		return nil, err
	}
	bottom, err := req.Region.Bottom()
	if err != nil {
		return nil, err
	}
	localRight, err := xconv.SubChecked(right, screen.Origin.X, "local.right")
	if err != nil {
		return nil, err
	}
	localBottom, err := xconv.SubChecked(bottom, screen.Origin.Y, "local.bottom")
	if err != nil {
		return nil, err
	}
	local := model.Rect{X: localX, Y: localY, W: req.Region.W, H: req.Region.H}

	leftScale, err := xconv.Scale(localX, num, scaleDen, xconv.RoundFloor, "map.left")
	if err != nil {
		return nil, err
	}
	topScale, err := xconv.Scale(localY, num, scaleDen, xconv.RoundFloor, "map.top")
	if err != nil {
		return nil, err
	}
	rightScale, err := xconv.Scale(localRight, num, scaleDen, xconv.RoundCeil, "map.right")
	if err != nil {
		return nil, err
	}
	bottomScale, err := xconv.Scale(localBottom, num, scaleDen, xconv.RoundCeil, "map.bottom")
	if err != nil {
		return nil, err
	}

	unclipped := PixelRect{
		X: leftScale.Value, Y: topScale.Value,
		W: rightScale.Value - leftScale.Value, H: bottomScale.Value - topScale.Value,
	}
	clipped := clipPixelRect(unclipped, screen.PixelW, screen.PixelH)
	if clipped.W <= 0 || clipped.H <= 0 {
		return nil, &model.Error{
			Code: model.EPara, ScreenID: screen.ID, Field: "pixel_region",
			Path:    "$.windows[].mapping." + screen.ID,
			Message: "mapped region is empty after clipping",
			Actual:  fmt.Sprintf("unclipped=%+v screen=0,0,%d,%d scale=%s supported=%v", unclipped, screen.PixelW, screen.PixelH, scale, screen.SupportedScales),
			Details: map[string]any{"unclipped": unclipped, "pixel_width": screen.PixelW, "pixel_height": screen.PixelH},
		}
	}

	coordRemainder := coordResidual(localX, localY, localRight, localBottom)
	scaleError := leftScale.RoundingError + topScale.RoundingError + rightScale.RoundingError + bottomScale.RoundingError
	clipLeft := maxInt64(0, -unclipped.X)
	clipTop := maxInt64(0, -unclipped.Y)
	clipRight := maxInt64(0, unclipped.X+unclipped.W-screen.PixelW)
	clipBottom := maxInt64(0, unclipped.Y+unclipped.H-screen.PixelH)
	clipTotal := clipLeft + clipTop + clipRight + clipBottom

	quantum := req.TimeQuantum
	if quantum <= 0 {
		quantum = model.TicksPerSecond / 1000
	}
	_, timeRemainder := xconv.DivModFloor(tick, quantum)
	packetBytes := req.PacketBytes
	if packetBytes <= 0 {
		packetBytes = 1400
	}
	pixelCapacity := packetBytes / model.PixelBytes
	area := clipped.W * clipped.H
	maxPackets := int64(0)
	if pixelCapacity > 0 {
		maxPackets = (area + pixelCapacity - 1) / pixelCapacity
	}

	bounds := Bounds{
		CoordRemainderUnits:       4 * (model.InternalCoordUnit - 1),
		ScaleRoundingNumerator:    4 * (scaleDen - 1),
		CoordinateScaledNumerator: (4 * (model.InternalCoordUnit - 1)) * num,
		GeometryChainNumerator:    (4*(model.InternalCoordUnit-1))*num + 4*(scaleDen-1),
		ClipTruncationPixels:      clipTotal,
		TimeRemainderTicks:        quantum,
		MaxPackets:                maxPackets,
	}
	report := ErrorReport{
		ScreenID: screen.ID, CoordRemainderUnits: coordRemainder,
		ScaleRoundingNumerator: scaleError,
		ClipLeft:               clipLeft, ClipTop: clipTop, ClipRight: clipRight, ClipBottom: clipBottom,
		ClipTruncationPixels: clipTotal, TimeRemainderTicks: timeRemainder, Bounds: bounds,
	}
	if err := validateErrorBounds(report, quantum); err != nil {
		return nil, err
	}

	return &Result{
		ScreenID: screen.ID, Active: true, LocalRegion: local,
		Unclipped: unclipped, PixelRegion: clipped,
		Params: Params{
			ScreenID: screen.ID, SourceRevision: screen.Revision, Scale: scale,
			ScaleNumerator: num, ScaleDenominator: scaleDen, CoordUnit: model.InternalCoordUnit,
			Rounding: "floor_left_top_ceil_right_bottom", TimeQuantum: quantum,
			PacketBytes: packetBytes, Chroma: screen.Chroma,
		},
		Errors: report,
	}, nil
}

func validateErrorBounds(report ErrorReport, quantum int64) error {
	b := report.Bounds
	if report.CoordRemainderUnits > b.CoordRemainderUnits {
		return model.NewError(model.EInternal, "coordinate residual exceeds bound")
	}
	if report.ScaleRoundingNumerator > b.ScaleRoundingNumerator {
		return model.NewError(model.EInternal, "scale residual exceeds bound")
	}
	if report.ClipTruncationPixels > b.ClipTruncationPixels {
		return model.NewError(model.EInternal, "clip residual exceeds bound")
	}
	if quantum > 1 && report.TimeRemainderTicks >= quantum {
		return model.NewError(model.EInternal, "time residual exceeds bound")
	}
	if report.TimeRemainderTicks > b.TimeRemainderTicks {
		return model.NewError(model.EInternal, "time residual exceeds upper bound")
	}
	return nil
}

func coordResidual(values ...int64) int64 {
	total := int64(0)
	for _, value := range values {
		_, rem := xconv.DivModFloor(value, model.InternalCoordUnit)
		total += rem
	}
	return total
}

func clipPixelRect(r PixelRect, screenW, screenH int64) PixelRect {
	x1 := maxInt64(0, r.X)
	y1 := maxInt64(0, r.Y)
	x2 := minInt64(screenW, r.X+r.W)
	y2 := minInt64(screenH, r.Y+r.H)
	return PixelRect{X: x1, Y: y1, W: maxInt64(0, x2-x1), H: maxInt64(0, y2-y1)}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func SortedScreens(screens []model.Screen) []model.Screen {
	out := append([]model.Screen(nil), screens...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

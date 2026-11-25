package kernel

import (
	"fmt"

	"obsproj/internal/model"
)

const (
	scaleDenominator       int64 = 100
	microPerLogicalUnit    int64 = 1000
	maxLogicalMicroPerTick int64 = 1000
)

type edgeResult struct {
	pixel         int64
	idealMicro    int64
	scaleResidual int64
}

func mapEdge(field string, relative, scale int64) (edgeResult, *model.KernelError) {
	numerator, err := model.CheckedMul(field, relative, scale)
	if err != nil {
		return edgeResult{}, err
	}
	denominator := microPerLogicalUnit * scaleDenominator
	pixel := model.FloorDiv(numerator, denominator)
	quotient := model.FloorDiv(numerator, denominator)
	remainder := model.FloorMod(numerator, denominator)
	fractionMicro := model.FloorDiv(remainder*microPerLogicalUnit, denominator)
	idealMicro, err := model.CheckedMul(field+".ideal_micro", quotient, microPerLogicalUnit)
	if err != nil {
		return edgeResult{}, err
	}
	idealMicro, err = model.CheckedAdd(field+".ideal_micro", idealMicro, fractionMicro)
	if err != nil {
		return edgeResult{}, err
	}
	pixelMicro, err := model.CheckedMul(field+".pixel_micro", pixel, microPerLogicalUnit)
	if err != nil {
		return edgeResult{}, err
	}
	residual, err := model.CheckedSub(field+".scale_residual", idealMicro, pixelMicro)
	if err != nil {
		return edgeResult{}, err
	}
	if residual < 0 {
		residual = -residual
	}
	return edgeResult{pixel: pixel, idealMicro: idealMicro, scaleResidual: residual}, nil
}

func clampIdealMicro(field string, value, resolution int64, scale int64, relative int64) (int64, *model.KernelError) {
	numerator, err := model.CheckedMul(field+".ideal_clamp_num", relative, scale)
	if err != nil {
		return 0, err
	}
	denominator := microPerLogicalUnit * scaleDenominator
	if numerator < 0 {
		return 0, nil
	}
	limit, err := model.CheckedMul(field+".ideal_clamp_limit", resolution, denominator)
	if err != nil {
		return 0, err
	}
	if numerator > limit {
		resolutionMicro, err := model.CheckedMul(field+".ideal_clamp_micro", resolution, microPerLogicalUnit)
		if err != nil {
			return 0, err
		}
		return resolutionMicro, nil
	}
	return value, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// MapRegion performs a deterministic, integer-only logical-to-pixel mapping and
// crop. timeResidual is measured in internal ticks and is included in the
// reported upper bound using the declared one-logical-pixel-per-tick motion
// envelope.
func MapRegion(screen model.Screen, source model.Rect, timeResidual int64) (model.MappedRegion, model.ErrorMetrics, *model.KernelError) {
	if source.W <= 0 || source.H <= 0 {
		return model.MappedRegion{}, model.ErrorMetrics{}, &model.KernelError{
			Code:            model.CodeParam,
			Message:         "source region is empty",
			ScreenID:        screen.ID,
			RequestedScale:  &screen.Scale,
			AllowedScales:   append([]model.Scale(nil), screen.Capability.Scales...),
			RequestedChroma: &screen.Chroma,
			AllowedChromas:  append([]model.Chroma(nil), screen.Capability.Chromas...),
		}
	}
	if screen.Resolution.W <= 0 || screen.Resolution.H <= 0 {
		return model.MappedRegion{}, model.ErrorMetrics{}, &model.KernelError{
			Code:           model.CodeParam,
			Message:        "screen resolution is empty",
			ScreenID:       screen.ID,
			RequestedScale: &screen.Scale,
			AllowedScales:  append([]model.Scale(nil), screen.Capability.Scales...),
		}
	}
	if !screen.AllowsScale(screen.Scale) {
		return model.MappedRegion{}, model.ErrorMetrics{}, &model.KernelError{
			Code:           model.CodeParam,
			Message:        "requested scale is not allowed by screen capability",
			ScreenID:       screen.ID,
			RequestedScale: &screen.Scale,
			AllowedScales:  append([]model.Scale(nil), screen.Capability.Scales...),
		}
	}
	if !screen.AllowsChroma(screen.Chroma) {
		return model.MappedRegion{}, model.ErrorMetrics{}, &model.KernelError{
			Code:            model.CodeParam,
			Message:         "requested chroma is not allowed by screen capability",
			ScreenID:        screen.ID,
			RequestedChroma: &screen.Chroma,
			AllowedChromas:  append([]model.Chroma(nil), screen.Capability.Chromas...),
		}
	}

	relX, err := model.CheckedSub("mapping."+screen.ID+".relative_x", source.X, screen.Origin.X)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	relY, err := model.CheckedSub("mapping."+screen.ID+".relative_y", source.Y, screen.Origin.Y)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	relRight, err := model.CheckedAdd("mapping."+screen.ID+".relative_right", relX, source.W)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	relBottom, err := model.CheckedAdd("mapping."+screen.ID+".relative_bottom", relY, source.H)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	scale := int64(screen.Scale)
	left, err := mapEdge("mapping."+screen.ID+".x0", relX, scale)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	right, err := mapEdge("mapping."+screen.ID+".x1", relRight, scale)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	top, err := mapEdge("mapping."+screen.ID+".y0", relY, scale)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	bottom, err := mapEdge("mapping."+screen.ID+".y1", relBottom, scale)
	if err != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, err
	}
	rawX0, rawY0 := left.pixel, top.pixel
	rawX1, rawY1 := right.pixel, bottom.pixel
	rawWidth, rawErr := model.CheckedSub("mapping."+screen.ID+".raw_width", rawX1, rawX0)
	if rawErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, rawErr
	}
	rawHeight, rawErr := model.CheckedSub("mapping."+screen.ID+".raw_height", rawY1, rawY0)
	if rawErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, rawErr
	}
	raw := model.PixelRect{X: rawX0, Y: rawY0, W: rawWidth, H: rawHeight}
	if raw.W <= 0 || raw.H <= 0 {
		return model.MappedRegion{}, model.ErrorMetrics{}, &model.KernelError{
			Code:           model.CodeParam,
			Message:        "scale produced an empty region",
			ScreenID:       screen.ID,
			RequestedScale: &screen.Scale,
			AllowedScales:  append([]model.Scale(nil), screen.Capability.Scales...),
		}
	}
	cropLeft := int64(0)
	if rawX0 < 0 {
		value, cropErr := model.CheckedSub("mapping."+screen.ID+".crop_left", 0, rawX0)
		if cropErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, cropErr
		}
		cropLeft = value
	}
	cropTop := int64(0)
	if rawY0 < 0 {
		value, cropErr := model.CheckedSub("mapping."+screen.ID+".crop_top", 0, rawY0)
		if cropErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, cropErr
		}
		cropTop = value
	}
	cropRight := int64(0)
	if rawX1 > screen.Resolution.W {
		value, cropErr := model.CheckedSub("mapping."+screen.ID+".crop_right", rawX1, screen.Resolution.W)
		if cropErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, cropErr
		}
		cropRight = value
	}
	cropBottom := int64(0)
	if rawY1 > screen.Resolution.H {
		value, cropErr := model.CheckedSub("mapping."+screen.ID+".crop_bottom", rawY1, screen.Resolution.H)
		if cropErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, cropErr
		}
		cropBottom = value
	}
	clipX0 := max64(0, rawX0)
	clipY0 := max64(0, rawY0)
	clipX1 := min64(screen.Resolution.W, rawX1)
	clipY1 := min64(screen.Resolution.H, rawY1)
	if clipX1 <= clipX0 || clipY1 <= clipY0 {
		return model.MappedRegion{}, model.ErrorMetrics{}, &model.KernelError{
			Code:           model.CodeParam,
			Message:        "crop produced an empty region",
			ScreenID:       screen.ID,
			RequestedScale: &screen.Scale,
			AllowedScales:  append([]model.Scale(nil), screen.Capability.Scales...),
		}
	}
	pixels := model.PixelRect{X: clipX0, Y: clipY0, W: clipX1 - clipX0, H: clipY1 - clipY0}

	scaleBound, addErr := model.CheckedAdd("mapping."+screen.ID+".scale_bound", left.scaleResidual, right.scaleResidual)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	scaleBound, addErr = model.CheckedAdd("mapping."+screen.ID+".scale_bound", scaleBound, top.scaleResidual)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	scaleBound, addErr = model.CheckedAdd("mapping."+screen.ID+".scale_bound", scaleBound, bottom.scaleResidual)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	cropPixels, addErr := model.CheckedAdd("mapping."+screen.ID+".crop_pixels", cropLeft, cropTop)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	cropPixels, addErr = model.CheckedAdd("mapping."+screen.ID+".crop_pixels", cropPixels, cropRight)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	cropPixels, addErr = model.CheckedAdd("mapping."+screen.ID+".crop_pixels", cropPixels, cropBottom)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	cropBound, mulErr := model.CheckedMul("mapping."+screen.ID+".crop_bound", cropPixels, microPerLogicalUnit)
	if mulErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, mulErr
	}
	if timeResidual < 0 {
		timeResidual = -timeResidual
	}
	timeShiftLogical, mulErr := model.CheckedMul("mapping."+screen.ID+".time_shift", timeResidual, maxLogicalMicroPerTick)
	if mulErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, mulErr
	}
	timeShiftPixels, mulErr := model.CheckedMul("mapping."+screen.ID+".time_shift_scale", timeShiftLogical, scale)
	if mulErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, mulErr
	}
	timeBound, addErr := model.CheckedMul("mapping."+screen.ID+".time_bound", timeShiftPixels/scaleDenominator, 4)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	coordinateBound := int64(0)
	totalBound, addErr := model.CheckedAdd("mapping."+screen.ID+".total_bound", coordinateBound, scaleBound)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	totalBound, addErr = model.CheckedAdd("mapping."+screen.ID+".total_bound", totalBound, cropBound)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}
	totalBound, addErr = model.CheckedAdd("mapping."+screen.ID+".total_bound", totalBound, timeBound)
	if addErr != nil {
		return model.MappedRegion{}, model.ErrorMetrics{}, addErr
	}

	actualDeviation := int64(0)
	edges := []struct {
		relative   int64
		raw        int64
		clipped    int64
		idealValue int64
		resolution int64
	}{
		{relX, rawX0, clipX0, left.idealMicro, screen.Resolution.W},
		{relRight, rawX1, clipX1, right.idealMicro, screen.Resolution.W},
		{relY, rawY0, clipY0, top.idealMicro, screen.Resolution.H},
		{relBottom, rawY1, clipY1, bottom.idealMicro, screen.Resolution.H},
	}
	for _, edge := range edges {
		idealClamped, clampErr := clampIdealMicro("mapping."+screen.ID+".actual", edge.idealValue, edge.resolution, scale, edge.relative)
		if clampErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, clampErr
		}
		actualMicro, microErr := model.CheckedMul("mapping."+screen.ID+".actual_micro", edge.clipped, microPerLogicalUnit)
		if microErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, microErr
		}
		deviation, subErr := model.CheckedSub("mapping."+screen.ID+".actual_deviation", actualMicro, idealClamped)
		if subErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, subErr
		}
		deviation = abs64(deviation)
		actualDeviation, addErr = model.CheckedAdd("mapping."+screen.ID+".actual_deviation", actualDeviation, deviation)
		if addErr != nil {
			return model.MappedRegion{}, model.ErrorMetrics{}, addErr
		}
	}
	metrics := model.ErrorMetrics{
		CoordinateResidual: 0,
		ScaleResidual:      scaleBound,
		CropError:          cropBound,
		TimeResidual:       timeResidual,
		CoordinateBound:    coordinateBound,
		ScaleBound:         scaleBound,
		CropBound:          cropBound,
		TimeBound:          timeBound,
		TotalBound:         totalBound,
		ActualDeviation:    actualDeviation,
		WithinBound:        actualDeviation <= totalBound,
	}
	if actualDeviation > totalBound {
		return model.MappedRegion{}, metrics, &model.KernelError{
			Code:     model.CodeBound,
			Message:  "measured mapping deviation exceeds derived upper bound",
			ScreenID: screen.ID,
			Actual:   &actualDeviation,
			Bound:    &totalBound,
			Field:    "actual_deviation",
		}
	}
	return model.MappedRegion{
		ScreenID:   screen.ID,
		Source:     source,
		RawPixels:  raw,
		Pixels:     pixels,
		CropLeft:   cropLeft,
		CropTop:    cropTop,
		CropRight:  cropRight,
		CropBottom: cropBottom,
		Params: model.MappingParams{
			ScreenID:         screen.ID,
			OriginX:          screen.Origin.X,
			OriginY:          screen.Origin.Y,
			ResolutionW:      screen.Resolution.W,
			ResolutionH:      screen.Resolution.H,
			Scale:            screen.Scale,
			ScaleNumerator:   scale,
			ScaleDenominator: scaleDenominator,
			Chroma:           screen.Chroma,
			MaxPacketSize:    screen.Capability.MaxPacketSize,
		},
	}, metrics, nil
}

// ValidateMetricBound exposes the same E_BOUND check for deterministic fault
// injection tests.
func ValidateMetricBound(screenID string, actual, bound int64) *model.KernelError {
	if actual <= bound {
		return nil
	}
	return &model.KernelError{
		Code:     model.CodeBound,
		Message:  fmt.Sprintf("mapping deviation %d exceeds bound %d", actual, bound),
		ScreenID: screenID,
		Actual:   &actual,
		Bound:    &bound,
		Field:    "actual_deviation",
	}
}

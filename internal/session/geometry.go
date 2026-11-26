package session

import (
	"sort"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/safemath"
)

func ValidateScale(scalePermyriad int64) error {
	if scalePermyriad == 0 {
		return bizerror.New(bizerror.EScaleZero, "scale rung is zero")
	}
	if scalePermyriad < 0 || scalePermyriad > ScaleBasePermyriad {
		return bizerror.Newf(bizerror.EScaleOutOfRange, "scale %d/10000 is outside (0,%d]", scalePermyriad, ScaleBasePermyriad).
			WithDetail("scale_permyriad", scalePermyriad)
	}
	return nil
}

func OnDeclaredLadder(scalePermyriad int64) bool {
	i := sort.Search(len(DeclaredScaleLadderPermyriad), func(i int) bool {
		return DeclaredScaleLadderPermyriad[i] <= scalePermyriad
	})
	return i < len(DeclaredScaleLadderPermyriad) && DeclaredScaleLadderPermyriad[i] == scalePermyriad
}

// NextLowerRung returns the highest declared rung strictly below scale.
func NextLowerRung(scale int64) (int64, bool) {
	// The ladder is declared from highest to lowest; the first strictly
	// smaller rung is therefore the adjacent legal downgrade landing point.
	for _, rung := range DeclaredScaleLadderPermyriad {
		if rung < scale {
			return rung, true
		}
	}
	return 0, false
}

// HighestRungAtMost returns the highest ladder rung <= requested and >= min,
// if one exists.
func HighestRungAtMost(requested, min int64) (int64, bool) {
	best := int64(-1)
	for _, rung := range DeclaredScaleLadderPermyriad {
		if rung <= requested && rung >= min && rung > best {
			best = rung
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

func ValidateResolution(width, height int64) error {
	if width < 0 || height < 0 {
		return bizerror.New(bizerror.EResolutionNegative, "resolution width and height must be non-negative").
			WithDetail("width", width).WithDetail("height", height)
	}
	if width == 0 || height == 0 {
		return bizerror.New(bizerror.EResolutionZero, "resolution width and height must be positive").
			WithDetail("width", width).WithDetail("height", height)
	}
	return nil
}

func ValidateMaster(master Rect) error {
	if master.Width <= 0 || master.Height <= 0 {
		return bizerror.New(bizerror.EResolutionZero, "master region width and height must be positive").
			WithPath("master").WithDetail("rect", master)
	}
	if master.Width < 0 || master.Height < 0 || master.X < 0 || master.Y < 0 {
		return bizerror.New(bizerror.EResolutionNegative, "master region coordinates and dimensions must be non-negative").
			WithPath("master").WithDetail("rect", master)
	}
	return nil
}

// MapRegion computes the destination mirror rectangle in controlled-screen
// coordinates. Rounding is round-half-up; centering offsets use floor
// ((screen-extent - output-extent)/2), leaving any extra odd pixel on the
// right/bottom edge.
func MapRegion(master Rect, screenWidth, screenHeight, scalePermyriad int64, chromaRaw string) (Mapping, error) {
	if err := ValidateMaster(master); err != nil {
		return Mapping{}, err
	}
	if err := ValidateResolution(screenWidth, screenHeight); err != nil {
		return Mapping{}, err
	}
	if err := ValidateScale(scalePermyriad); err != nil {
		return Mapping{}, err
	}
	format, info, err := ParseChroma(chromaRaw)
	if err != nil {
		return Mapping{}, err
	}

	boxWidth, err := safemath.MulDivRound(screenWidth, scalePermyriad, ScaleBasePermyriad)
	if err != nil {
		return Mapping{}, bizerror.New(bizerror.EOverflow, "width scale propagation overflow").WithDetail("screen_width", screenWidth).WithDetail("scale", scalePermyriad)
	}
	boxHeight, err := safemath.MulDivRound(screenHeight, scalePermyriad, ScaleBasePermyriad)
	if err != nil {
		return Mapping{}, bizerror.New(bizerror.EOverflow, "height scale propagation overflow").WithDetail("screen_height", screenHeight).WithDetail("scale", scalePermyriad)
	}

	masterW := master.Width
	masterH := master.Height
	wideNumerator, err := safemath.Mul(masterW, boxHeight)
	if err != nil {
		return Mapping{}, bizerror.New(bizerror.EOverflow, "aspect comparison overflow")
	}
	wideDenominator, err := safemath.Mul(boxWidth, masterH)
	if err != nil {
		return Mapping{}, bizerror.New(bizerror.EOverflow, "aspect comparison overflow")
	}

	outW, outH := int64(0), int64(0)
	if wideNumerator > wideDenominator {
		outW = boxWidth
		outH, err = safemath.MulDivRound(outW, masterH, masterW)
		if err != nil {
			return Mapping{}, bizerror.New(bizerror.EOverflow, "scaled height propagation overflow")
		}
	} else {
		outH = boxHeight
		outW, err = safemath.MulDivRound(outH, masterW, masterH)
		if err != nil {
			return Mapping{}, bizerror.New(bizerror.EOverflow, "scaled width propagation overflow")
		}
	}

	if outW < info.MinUnitWidth || outH < info.MinUnitHeight {
		return Mapping{}, bizerror.New(bizerror.EResolutionTooSmall, "scaled mirror region is smaller than one chroma sampling unit").
			WithScreen("").
			WithDetail("scaled_width", outW).
			WithDetail("scaled_height", outH).
			WithDetail("min_unit_width", info.MinUnitWidth).
			WithDetail("min_unit_height", info.MinUnitHeight).
			WithConstraint("C-CHROMA-UNIT")
	}

	planeW, err := safemath.CeilDiv(outW, info.WidthDivisor)
	if err != nil {
		return Mapping{}, bizerror.New(bizerror.EOverflow, "plane width overflow")
	}
	planeH, err := safemath.CeilDiv(outH, info.HeightDivisor)
	if err != nil {
		return Mapping{}, bizerror.New(bizerror.EOverflow, "plane height overflow")
	}

	x := (screenWidth - outW) / 2
	y := (screenHeight - outH) / 2

	frameBits, err := FrameBits(outW, outH, planeW, planeH, info.BitDepth)
	if err != nil {
		return Mapping{}, err
	}
	cost, err := BudgetFromFrameBits(frameBits, DeclaredFrameRate)
	if err != nil {
		return Mapping{}, err
	}

	return Mapping{
		Source:              master,
		Destination:         Rect{X: x, Y: y, Width: outW, Height: outH},
		ScaledWidth:         outW,
		ScaledHeight:        outH,
		PlaneWidth:          planeW,
		PlaneHeight:         planeH,
		Chroma:              format,
		ChromaBitsPerPixel:  info.ChromaBitsPerPixel,
		NominalBitsPerPixel: info.BitsPerPixel,
		FrameBits:           frameBits,
		BudgetCostKbps:      cost,
	}, nil
}

func FrameBits(width, height, planeWidth, planeHeight, bitDepth int64) (int64, error) {
	luma, err := safemath.Mul(width, height)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "luma sample count overflow")
	}
	chromaPlane, err := safemath.Mul(planeWidth, planeHeight)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "chroma sample count overflow")
	}
	chroma, err := safemath.Mul(2, chromaPlane)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "chroma sample count overflow")
	}
	samples, err := safemath.Add(luma, chroma)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "total sample count overflow")
	}
	bits, err := safemath.Mul(samples, bitDepth)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "frame bit count overflow")
	}
	return bits, nil
}

func BudgetFromFrameBits(frameBits, fps int64) (int64, error) {
	bitsPerSecond, err := safemath.Mul(frameBits, fps)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "bitrate propagation overflow")
	}
	cost := bitsPerSecond / 1000
	if bitsPerSecond%1000 != 0 {
		cost, err = safemath.Add(cost, 1)
		if err != nil {
			return 0, bizerror.New(bizerror.EOverflow, "budget rounding overflow")
		}
	}
	return cost, nil
}

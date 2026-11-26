package projectio

import (
	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/safemath"
)

// AreaKiloPixels returns W*H/1000 for representations whose equivalent area
// is an exact integral kilopixel value.
func AreaKiloPixels(width, height int64) (int64, error) {
	pixels, err := safemath.Mul(width, height)
	if err != nil {
		return 0, bizerror.New(bizerror.EOverflow, "resolution area overflow")
	}
	if pixels%1000 != 0 {
		return 0, bizerror.New(bizerror.EResolutionAreaInvalid, "resolution area is not an integral number of kilopixels").
			WithDetail("width", width).WithDetail("height", height).WithDetail("area_pixels", pixels)
	}
	return pixels / 1000, nil
}

// DimensionsFromArea derives exact pixel dimensions from area in kilopixels
// plus integer aspect ratio terms aw:ah.
func DimensionsFromArea(areaKiloPixels, aspectWidth, aspectHeight int64) (int64, int64, error) {
	if areaKiloPixels <= 0 {
		return 0, 0, bizerror.New(bizerror.EResolutionAreaInvalid, "area_kilo_pixels must be positive").
			WithDetail("value", areaKiloPixels)
	}
	if aspectWidth <= 0 || aspectHeight <= 0 {
		return 0, 0, bizerror.New(bizerror.EResolutionAreaInvalid, "aspect ratio terms must be positive").
			WithDetail("aspect_width", aspectWidth).WithDetail("aspect_height", aspectHeight)
	}
	areaPixels, err := safemath.Mul(areaKiloPixels, 1000)
	if err != nil {
		return 0, 0, bizerror.New(bizerror.EOverflow, "kilopixel to pixel area overflow")
	}
	aspectProduct, err := safemath.Mul(aspectWidth, aspectHeight)
	if err != nil {
		return 0, 0, bizerror.New(bizerror.EOverflow, "aspect product overflow")
	}
	if areaPixels%aspectProduct != 0 {
		return 0, 0, bizerror.New(bizerror.EResolutionAreaInvalid, "area is not representable by the declared aspect ratio").
			WithDetail("area_kilo_pixels", areaKiloPixels).
			WithDetail("aspect_width", aspectWidth).
			WithDetail("aspect_height", aspectHeight)
	}
	factorSquared := areaPixels / aspectProduct
	factor, ok := safemath.SqrtExact(factorSquared)
	if !ok {
		return 0, 0, bizerror.New(bizerror.EResolutionAreaInvalid, "area/aspect ratio does not yield integral pixel dimensions").
			WithDetail("factor_squared", factorSquared)
	}
	width, err := safemath.Mul(aspectWidth, factor)
	if err != nil {
		return 0, 0, bizerror.New(bizerror.EOverflow, "derived width overflow")
	}
	height, err := safemath.Mul(aspectHeight, factor)
	if err != nil {
		return 0, 0, bizerror.New(bizerror.EOverflow, "derived height overflow")
	}
	return width, height, nil
}

func permilleToPermyriad(permille int64) int64 { return permille * 10 }

func permyriadToPermilleExact(permyriad int64) (int64, error) {
	if permyriad%10 != 0 {
		return 0, bizerror.New(bizerror.ESchemaConflict, "per-ten-thousand scale is not exactly representable as per-mille").
			WithDetail("scale_permyriad", permyriad)
	}
	return permyriad / 10, nil
}

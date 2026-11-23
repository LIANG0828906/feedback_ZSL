package workbench

import (
	"fmt"
	"math"
)

// ValidateScene checks the loader contract before any processing occurs. Error
// codes are intentionally stable: missing structural metadata is E_META, band
// cardinality is E_BAND and non-increasing exposure times are E_ORDER.
func ValidateScene(scene *Scene) error {
	if scene == nil {
		return fieldErr(CodeMeta, "scene", nil, "non-nil scene", "scene metadata is missing")
	}
	m := &scene.Meta
	if m.Width <= 0 {
		return fieldErr(CodeMeta, "width", m.Width, ">0", "image width metadata is missing or invalid")
	}
	if m.Height <= 0 {
		return fieldErr(CodeMeta, "height", m.Height, ">0", "image height metadata is missing or invalid")
	}
	if m.Width*m.Height > 40_000_000 {
		return fieldErr(CodeMeta, "width*height", m.Width*m.Height, "<=40000000", "image dimensions exceed supported size")
	}
	if m.BitDepth < 1 || m.BitDepth > 16 {
		return fieldErr(CodeMeta, "bitDepth", m.BitDepth, "1..16", "quantization bit depth metadata is missing or invalid")
	}
	if m.PixelPitchMicrons <= 0 || !finite(m.PixelPitchMicrons) {
		return fieldErr(CodeMeta, "pixelPitchMicrons", m.PixelPitchMicrons, ">0", "pixel size metadata is missing")
	}
	if m.ResponseGamma < 0.5 || m.ResponseGamma > 4 {
		return fieldErr(CodeMeta, "responseGamma", m.ResponseGamma, "0.5..4", "inverse camera response metadata is missing or invalid")
	}
	if m.BandRadianceScale <= 0 || !finite(m.BandRadianceScale) {
		return fieldErr(CodeMeta, "bandRadianceScale", m.BandRadianceScale, ">0", "band radiance calibration metadata is missing")
	}
	if m.BandMetadataVersion == "" {
		return fieldErr(CodeMeta, "bandMetadataVersion", m.BandMetadataVersion, "non-empty", "band metadata version is missing")
	}
	if m.TargetGamut == "" {
		return fieldErr(CodeMeta, "targetGamut", m.TargetGamut, []string{"sRGB", "DisplayP3", "Rec2020"}, "target gamut metadata is missing")
	}
	if _, _, ok := gamutMatrices(m.TargetGamut); !ok {
		return fieldErr(CodeMeta, "targetGamut", m.TargetGamut, []string{"sRGB", "DisplayP3", "Rec2020"}, "target gamut metadata is unsupported")
	}
	if err := validateWhitePoint("whitePoint", m.WhitePoint); err != nil {
		return err
	}
	if err := validateWhitePoint("sourceWhitePoint", m.SourceWhitePoint); err != nil {
		return err
	}
	if err := validateWhitePoint("truth.whitePoint", scene.Truth.WhitePoint); err != nil {
		return err
	}
	if m.WhitePoint.Name != scene.Truth.WhitePoint.Name ||
		math.Abs(m.WhitePoint.X-scene.Truth.WhitePoint.X) > 1e-9 ||
		math.Abs(m.WhitePoint.Y-scene.Truth.WhitePoint.Y) > 1e-9 ||
		math.Abs(m.WhitePoint.Z-scene.Truth.WhitePoint.Z) > 1e-9 {
		return fieldErr(CodeMeta, "whitePoint", scene.Truth.WhitePoint, m.WhitePoint, "metadata and truth white point fields disagree")
	}
	if m.BandCount < 3 || m.BandCount > 8 {
		return fieldErr(CodeBand, "bandCount", m.BandCount, "3..8", "band count is outside the supported range")
	}
	if len(m.Bands) != m.BandCount {
		return fieldErr(CodeBand, "bands", len(m.Bands), m.BandCount, "band metadata count does not match bandCount")
	}
	if len(scene.Bands) != m.BandCount {
		return fieldErr(CodeBand, "bands", len(scene.Bands), m.BandCount, "loaded band images do not match metadata band count")
	}
	if len(scene.Truth.Registration) != m.BandCount {
		return fieldErr(CodeBand, "truth.registration", len(scene.Truth.Registration), m.BandCount, "registration truth count does not match band count")
	}
	if len(m.ExposureTimes) != m.ExposureCount {
		return fieldErr(CodeMeta, "exposureTimes", len(m.ExposureTimes), m.ExposureCount, "exposure time metadata is incomplete")
	}
	if m.ExposureCount < 3 {
		return fieldErr(CodeMeta, "exposureCount", m.ExposureCount, ">=3", "HDR stack requires at least three exposures")
	}
	if len(scene.Exposures) != m.ExposureCount {
		return fieldErr(CodeMeta, "exposures", len(scene.Exposures), m.ExposureCount, "loaded exposure frame count does not match metadata")
	}
	for i, t := range m.ExposureTimes {
		if t <= 0 || !finite(t) {
			return fieldErr(CodeMeta, fmt.Sprintf("exposureTimes[%d]", i), t, ">0", "exposure time is missing or invalid")
		}
		if i > 0 && t <= m.ExposureTimes[i-1] {
			return fieldErr(CodeOrder, fmt.Sprintf("exposureTimes[%d]", i), t, ">"+fmt.Sprint(m.ExposureTimes[i-1]), "exposure times are not strictly increasing")
		}
	}
	for b, meta := range m.Bands {
		if meta.CenterWavelength < 380 || meta.CenterWavelength > 780 {
			return fieldErr(CodeMeta, fmt.Sprintf("bands[%d].centerWavelengthNm", b), meta.CenterWavelength, "380..780", "band center wavelength is missing or invalid")
		}
		if meta.FWHM <= 0 || !finite(meta.FWHM) {
			return fieldErr(CodeMeta, fmt.Sprintf("bands[%d].fwhmNm", b), meta.FWHM, ">0", "band response width is missing or invalid")
		}
	}
	if len(scene.Detail) != m.Width*m.Height {
		return fieldErr(CodeMeta, "detail", len(scene.Detail), m.Width*m.Height, "detail layer pixel count does not match image dimensions")
	}
	if len(scene.Truth.Patches) == 0 {
		return fieldErr(CodeMeta, "truth.patches", 0, ">0", "truth color patch list is missing")
	}
	maxQ := maxCode(m.BitDepth)
	for b, img := range scene.Bands {
		if img.Width != m.Width || img.Height != m.Height {
			return fieldErr(CodeMeta, fmt.Sprintf("bands[%d].width/height", b), []int{img.Width, img.Height}, []int{m.Width, m.Height}, "band image dimensions disagree with metadata")
		}
		if img.BitDepth != m.BitDepth {
			return fieldErr(CodeMeta, fmt.Sprintf("bands[%d].bitDepth", b), img.BitDepth, m.BitDepth, "band quantization bit depth disagrees with metadata")
		}
		if len(img.Data) != m.Width*m.Height {
			return fieldErr(CodeMeta, fmt.Sprintf("bands[%d].data", b), len(img.Data), m.Width*m.Height, "band pixel data is incomplete")
		}
		if img.CenterWavelength != m.Bands[b].CenterWavelength {
			return fieldErr(CodeBand, fmt.Sprintf("bands[%d].centerWavelengthNm", b), img.CenterWavelength, m.Bands[b].CenterWavelength, "band image wavelength disagrees with metadata")
		}
		for i, value := range img.Data {
			if uint32(value) > maxQ {
				return fieldErr(CodeMeta, fmt.Sprintf("bands[%d].data[%d]", b, i), value, "0.."+fmt.Sprint(maxQ), "band sample exceeds declared quantization bit depth")
			}
		}
	}
	for e, frame := range scene.Exposures {
		if frame.Width != m.Width || frame.Height != m.Height {
			return fieldErr(CodeMeta, fmt.Sprintf("exposures[%d].width/height", e), []int{frame.Width, frame.Height}, []int{m.Width, m.Height}, "exposure frame dimensions disagree with metadata")
		}
		if frame.BitDepth != m.BitDepth {
			return fieldErr(CodeMeta, fmt.Sprintf("exposures[%d].bitDepth", e), frame.BitDepth, m.BitDepth, "exposure quantization bit depth disagrees with metadata")
		}
		if math.Abs(frame.ExposureTime-m.ExposureTimes[e]) > 1e-12 {
			return fieldErr(CodeMeta, fmt.Sprintf("exposures[%d].exposureTime", e), frame.ExposureTime, m.ExposureTimes[e], "exposure frame time disagrees with metadata")
		}
		if len(frame.Data) != m.Width*m.Height {
			return fieldErr(CodeMeta, fmt.Sprintf("exposures[%d].data", e), len(frame.Data), m.Width*m.Height, "exposure frame pixel data is incomplete")
		}
		for i, value := range frame.Data {
			if uint32(value) > maxQ {
				return fieldErr(CodeMeta, fmt.Sprintf("exposures[%d].data[%d]", e, i), value, "0.."+fmt.Sprint(maxQ), "exposure sample exceeds declared quantization bit depth")
			}
		}
	}
	for i, offset := range scene.Truth.Registration {
		if offset.FromBand != 0 || offset.ToBand != i {
			return fieldErr(CodeBand, fmt.Sprintf("truth.registration[%d]", i), offset.Name(), fmt.Sprintf("b0->b%d", i), "registration band pair is inconsistent with band index")
		}
		if !finite(offset.DX) || !finite(offset.DY) {
			return fieldErr(CodeMeta, fmt.Sprintf("truth.registration[%d]", i), []float64{offset.DX, offset.DY}, "finite", "registration offset is missing or invalid")
		}
	}
	for i, patch := range scene.Truth.Patches {
		if patch.Width <= 0 || patch.Height <= 0 ||
			patch.X < 0 || patch.Y < 0 ||
			patch.X+patch.Width > m.Width || patch.Y+patch.Height > m.Height {
			return fieldErr(CodeMeta, fmt.Sprintf("truth.patches[%d]", i), []int{patch.X, patch.Y, patch.Width, patch.Height}, "inside image", "truth patch bounds are invalid")
		}
		for c, v := range patch.LinearRGB {
			if !finite(v) || v < 0 || v > 1 {
				return fieldErr(CodeMeta, fmt.Sprintf("truth.patches[%d].linearRGB[%d]", i, c), v, "0..1", "truth patch is outside the target linear gamut")
			}
		}
	}
	if !finite(scene.Truth.PeakRadiance) || scene.Truth.PeakRadiance < 0 {
		return fieldErr(CodeMeta, "truth.peakRadiance", scene.Truth.PeakRadiance, ">=0", "peak radiance truth is missing or invalid")
	}
	return nil
}

func validateWhitePoint(field string, wp WhitePoint) error {
	if wp.Name == "" {
		return fieldErr(CodeMeta, field+".name", wp.Name, "non-empty", "white point name is missing")
	}
	if !finite(wp.X) || !finite(wp.Y) || !finite(wp.Z) || wp.Y <= 0 ||
		wp.X <= 0 || wp.Z < 0 || wp.X+wp.Y+wp.Z <= 0 {
		return fieldErr(CodeMeta, field, wp, "valid tristimulus", "white point tristimulus fields are invalid")
	}
	return nil
}

package spectralrender

import "math"

type TruthPixelCheck struct {
	X                 int     `json:"x"`
	Y                 int     `json:"y"`
	LinearRGBError    float64 `json:"linear_rgb_error"`
	Tolerance         float64 `json:"tolerance"`
	PeakRadianceError float64 `json:"peak_radiance_error"`
	PeakTolerance     float64 `json:"peak_tolerance"`
	ChromaticityError float64 `json:"chromaticity_error"`
	ChromaticityBound float64 `json:"chromaticity_bound"`
	Pass              bool    `json:"pass"`
}

type TruthCheckReport struct {
	SceneHash       string            `json:"scene_hash"`
	Pass            bool              `json:"pass"`
	MaxLinearRGB    float64           `json:"max_linear_rgb_error"`
	MaxPeak         float64           `json:"max_peak_radiance_error"`
	MaxChromaticity float64           `json:"max_chromaticity_error"`
	Pixels          []TruthPixelCheck `json:"pixels"`
}

func CheckTruthFrame(frame *Frame, truth *TruthManifest) (*TruthCheckReport, *Error) {
	if frame == nil || truth == nil {
		return nil, &Error{Code: ErrState, Message: "frame and truth are required"}
	}
	if frame.SceneHash != truth.SceneHash {
		return nil, &Error{Code: ErrSchema, Path: "truth.scene_hash",
			Message: "truth manifest does not belong to the rendered frame"}
	}
	r := &TruthCheckReport{SceneHash: truth.SceneHash, Pass: true}
	for _, px := range truth.Pixels {
		if px.X < 0 || px.X >= frame.Width || px.Y < 0 || px.Y >= frame.Height {
			return nil, schemaError("truth.pixels", "truth pixel is outside the frame")
		}
		idx := px.Y*frame.Width + px.X
		got := RGB{frame.LinearRGB[idx*3], frame.LinearRGB[idx*3+1], frame.LinearRGB[idx*3+2]}
		chk := TruthPixelCheck{
			X: px.X, Y: px.Y,
			LinearRGBError:    maxAbsDiff(got, px.LinearRGB),
			Tolerance:         px.Tolerance,
			PeakRadianceError: math.Abs(frame.PeakRadiance[idx] - px.PeakRadiance),
			PeakTolerance:     px.PeakTolerance,
			ChromaticityBound: px.ChromaticityBound,
		}
		xyz := RGB{frame.XYZ[idx*3], frame.XYZ[idx*3+1], frame.XYZ[idx*3+2]}
		xy := chromaticity([3]float64{xyz[0], xyz[1], xyz[2]})
		chk.ChromaticityError = math.Hypot(xy[0]-px.ChromaticityXY[0], xy[1]-px.ChromaticityXY[1])
		chk.Pass = chk.LinearRGBError <= chk.Tolerance &&
			chk.PeakRadianceError <= chk.PeakTolerance &&
			chk.ChromaticityError <= chk.ChromaticityBound
		if !chk.Pass {
			r.Pass = false
		}
		if chk.LinearRGBError > r.MaxLinearRGB {
			r.MaxLinearRGB = chk.LinearRGBError
		}
		if chk.PeakRadianceError > r.MaxPeak {
			r.MaxPeak = chk.PeakRadianceError
		}
		if chk.ChromaticityError > r.MaxChromaticity {
			r.MaxChromaticity = chk.ChromaticityError
		}
		r.Pixels = append(r.Pixels, chk)
	}
	return r, nil
}

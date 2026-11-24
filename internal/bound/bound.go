package bound

import (
	"fmt"
	"math"

	"audiocheck/internal/model"
)

type Input struct {
	Frame      int
	QuantStep  int64
	FrameSize  int
	WindowSize int
	Peak       int64
	FracBits   int
}

func Build(in Input) model.ErrorBound {
	bits := in.FracBits
	if bits <= 0 {
		bits = model.CoefficientFracBits
	}
	scale := model.MetricScale
	q := float64(in.QuantStep)
	coeffMax := q/2.0 + 0.5/float64(int64(1)<<bits)
	transformBound := math.Sqrt(float64(in.FrameSize)) * coeffMax
	roundBound := 0.5 + 0.5/float64(scale)
	sampleUpper := transformBound + roundBound
	rmsUpperFixed := int64(math.Ceil((sampleUpper + 0.5/float64(scale)) * float64(scale)))
	window := in.WindowSize
	if window <= 0 {
		window = in.FrameSize
	}
	peak := float64(in.Peak)
	if peak <= 0 {
		peak = 32767
	}

	snrMin := -1e9
	if sampleUpper > 0 {
		snrMin = 20.0 * math.Log10(peak/sampleUpper)
	}

	// Integer PCM has minimum nonzero squared error 1. A window contains at
	// most WindowSize sample positions per accumulated channel stream; the
	// conservative ceiling uses one stream and therefore remains valid when
	// channels are accumulated separately.
	snrMax := 10.0 * math.Log10(peak*peak*float64(window))
	snrMax += 0.5 / float64(scale)

	return model.ErrorBound{
		Frame:                       in.Frame,
		QuantStep:                   in.QuantStep,
		FrameSize:                   in.FrameSize,
		WindowSize:                  window,
		CoefficientFracBits:         bits,
		MetricScale:                 scale,
		CoeffErrorMax:               coeffMax,
		TransformSampleErrorUpper:   transformBound,
		ReconstructionRoundingUpper: roundBound,
		SampleErrorUpper:            sampleUpper,
		RMSErrorUpperFixed:          rmsUpperFixed,
		WindowEnergyUpper:           float64(window) * sampleUpper * sampleUpper,
		WindowL1Upper:               float64(window) * sampleUpper,
		SNRMinimumDBFixed:           int64(math.Floor(snrMin * float64(scale))),
		SNRUpperDBFixed:             int64(math.Ceil(snrMax * float64(scale))),
		Formula:                     fmt.Sprintf("|e_c|<=%d/2+0.5*2^-%d; |e_s|<sqrt(%d)*|e_c|+0.5; E_W<=%d*|e_s|^2; SNR_max<=10log10(peak^2*W)", in.QuantStep, bits, in.FrameSize, window),
	}
}

func CheckFrame(frame int, measuredSNRFixed int64, snrInfinite bool, measuredRMSEFixed int64, b model.ErrorBound) error {
	if measuredRMSEFixed > b.RMSErrorUpperFixed {
		err := model.NewError(model.CodeBound, "noise_floor_std_fixed",
			"frame %d RMSE %d exceeds error upper bound %d", frame, measuredRMSEFixed, b.RMSErrorUpperFixed)
		model.WithFrame(err, frame)
		err.Expected = b.RMSErrorUpperFixed
		err.Actual = measuredRMSEFixed
		return err
	}
	if snrInfinite {
		return nil
	}
	tolerance := int64(2 * model.MetricScale)
	if measuredSNRFixed > b.SNRUpperDBFixed+tolerance {
		err := model.NewError(model.CodeBound, "snr_db_fixed",
			"frame %d SNR %s dB exceeds upper bound %s dB", frame,
			formatFixed(measuredSNRFixed), formatFixed(b.SNRUpperDBFixed))
		model.WithFrame(err, frame)
		err.Expected = b.SNRUpperDBFixed
		err.Actual = measuredSNRFixed
		return err
	}
	if measuredSNRFixed < b.SNRMinimumDBFixed-tolerance {
		err := model.NewError(model.CodeBound, "snr_db_fixed",
			"frame %d SNR %s dB is below worst-case lower bound %s dB", frame,
			formatFixed(measuredSNRFixed), formatFixed(b.SNRMinimumDBFixed))
		model.WithFrame(err, frame)
		err.Expected = b.SNRMinimumDBFixed
		err.Actual = measuredSNRFixed
		return err
	}
	return nil
}

func formatFixed(v int64) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%06d", sign, v/model.MetricScale, v%model.MetricScale)
}

package verify

import (
	"fmt"
	"math"

	"spatialworkbench/internal/acoustics"
	"spatialworkbench/internal/session"
	"spatialworkbench/internal/werr"
)

type PathName string

const (
	PathRealtime  PathName = "realtime"
	PathResume    PathName = "resume"
	PathRecompute PathName = "persisted-recompute"
)

func Compare(leftName, rightName string, left, right session.RunOutput) *werr.Error {
	if len(left.CommitSequence) != len(right.CommitSequence) {
		frame := minInt(len(left.CommitSequence), len(right.CommitSequence))
		return diff(frame, "", "commitSequence",
			fmt.Sprintf("%s length %d", leftName, len(left.CommitSequence)),
			fmt.Sprintf("%s length %d", rightName, len(right.CommitSequence)))
	}
	for i := range left.CommitSequence {
		if left.CommitSequence[i] != right.CommitSequence[i] {
			return diff(i, "", "commitSequence",
				fmt.Sprintf("%s=%d", leftName, left.CommitSequence[i]),
				fmt.Sprintf("%s=%d", rightName, right.CommitSequence[i]))
		}
	}
	if len(left.Frames) != len(right.Frames) {
		return diff(minInt(len(left.Frames), len(right.Frames)), "", "frames",
			fmt.Sprintf("%s length %d", leftName, len(left.Frames)),
			fmt.Sprintf("%s length %d", rightName, len(right.Frames)))
	}
	for frameIndex := range left.Frames {
		a := left.Frames[frameIndex]
		b := right.Frames[frameIndex]
		if a.Frame != b.Frame {
			return diff(frameIndex, "", "frame", a.Frame, b.Frame)
		}
		if a.ParamsHash != b.ParamsHash {
			return diff(frameIndex, "", "paramsHash", a.ParamsHash, b.ParamsHash)
		}
		if a.InputHash != b.InputHash {
			return diff(frameIndex, "", "inputHash", a.InputHash, b.InputHash)
		}
		if a.Algorithm != b.Algorithm {
			return diff(frameIndex, "", "algorithmVersion", a.Algorithm, b.Algorithm)
		}
		frameErr := compareFrame(frameIndex, a, b)
		if frameErr != nil {
			return frameErr
		}
	}
	if len(left.Output) != len(right.Output) {
		return diff(0, "", "output.channels", len(left.Output), len(right.Output))
	}
	for channel := range left.Output {
		if len(left.Output[channel]) != len(right.Output[channel]) {
			return diff(0, fmt.Sprintf("ch%d", channel), "output.samples",
				len(left.Output[channel]), len(right.Output[channel]))
		}
		for sampleIndex := range left.Output[channel] {
			if math.Float64bits(left.Output[channel][sampleIndex]) != math.Float64bits(right.Output[channel][sampleIndex]) {
				return diff(0, fmt.Sprintf("ch%d", channel), fmt.Sprintf("output.sample[%d]", sampleIndex),
					left.Output[channel][sampleIndex], right.Output[channel][sampleIndex])
			}
		}
	}
	return nil
}

func compareFrame(frame int, a, b acoustics.FrameResult) *werr.Error {
	if len(a.Estimate.Channels) != len(b.Estimate.Channels) {
		return diff(frame, "", "estimate.channels", len(a.Estimate.Channels), len(b.Estimate.Channels))
	}
	for i := range a.Estimate.Channels {
		ca, cb := a.Estimate.Channels[i], b.Estimate.Channels[i]
		if ca.ID != cb.ID {
			return diff(frame, ca.ID, "estimate.channelId", ca.ID, cb.ID)
		}
		if ca.DirectArrival != cb.DirectArrival {
			return diff(frame, ca.ID, "estimate.directArrival", ca.DirectArrival, cb.DirectArrival)
		}
		if !floatEqual(ca.DirectEnergy, cb.DirectEnergy) {
			return diff(frame, ca.ID, "estimate.directEnergy", ca.DirectEnergy, cb.DirectEnergy)
		}
		if !floatEqual(ca.EarlyEnergy, cb.EarlyEnergy) {
			return diff(frame, ca.ID, "estimate.earlyEnergy", ca.EarlyEnergy, cb.EarlyEnergy)
		}
		if ca.IntegratedWidth != cb.IntegratedWidth {
			return diff(frame, ca.ID, "estimate.integratedWidth", ca.IntegratedWidth, cb.IntegratedWidth)
		}
	}
	if len(a.Reconstruction.Channels) != len(b.Reconstruction.Channels) {
		return diff(frame, "", "reconstruction.channels", len(a.Reconstruction.Channels), len(b.Reconstruction.Channels))
	}
	for i := range a.Reconstruction.Channels {
		ca, cb := a.Reconstruction.Channels[i], b.Reconstruction.Channels[i]
		if ca.ID != cb.ID {
			return diff(frame, ca.ID, "reconstruction.channelId", ca.ID, cb.ID)
		}
		if ca.DirectDelay != cb.DirectDelay {
			return diff(frame, ca.ID, "reconstruction.directDelay", ca.DirectDelay, cb.DirectDelay)
		}
		if !floatEqual(ca.EarlyEnergy, cb.EarlyEnergy) {
			return diff(frame, ca.ID, "reconstruction.earlyEnergy", ca.EarlyEnergy, cb.EarlyEnergy)
		}
		if ca.ITDSamples != cb.ITDSamples {
			return diff(frame, ca.ID, "reconstruction.itdSamples", ca.ITDSamples, cb.ITDSamples)
		}
		if !floatEqual(ca.DRRDB, cb.DRRDB) {
			return diff(frame, ca.ID, "reconstruction.drrDb", ca.DRRDB, cb.DRRDB)
		}
		if !floatEqual(ca.DirectionDegree, cb.DirectionDegree) {
			return diff(frame, ca.ID, "reconstruction.directionDegree", ca.DirectionDegree, cb.DirectionDegree)
		}
	}
	if err := compareBounds(frame, a.Reconstruction.Bounds, b.Reconstruction.Bounds); err != nil {
		return err
	}
	if !floatEqual(a.Reconstruction.State.SmoothedEarlyEnergy, b.Reconstruction.State.SmoothedEarlyEnergy) {
		return diff(frame, "", "state.smoothedEarlyEnergy", a.Reconstruction.State.SmoothedEarlyEnergy, b.Reconstruction.State.SmoothedEarlyEnergy)
	}
	if !floatEqual(a.Reconstruction.State.CumulativeDirect, b.Reconstruction.State.CumulativeDirect) {
		return diff(frame, "", "state.cumulativeDirectEnergy", a.Reconstruction.State.CumulativeDirect, b.Reconstruction.State.CumulativeDirect)
	}
	if !floatEqual(a.Reconstruction.State.CumulativeEarly, b.Reconstruction.State.CumulativeEarly) {
		return diff(frame, "", "state.cumulativeEarlyEnergy", a.Reconstruction.State.CumulativeEarly, b.Reconstruction.State.CumulativeEarly)
	}
	if len(a.Render) != len(b.Render) {
		return diff(frame, "", "render.channels", len(a.Render), len(b.Render))
	}
	for channel := range a.Render {
		if len(a.Render[channel]) != len(b.Render[channel]) {
			return diff(frame, fmt.Sprintf("ch%d", channel), "render.samples", len(a.Render[channel]), len(b.Render[channel]))
		}
		for n := range a.Render[channel] {
			if math.Float64bits(a.Render[channel][n]) != math.Float64bits(b.Render[channel][n]) {
				field := fmt.Sprintf("render.sample[%d]", n)
				return diff(frame, fmt.Sprintf("ch%d", channel), field, a.Render[channel][n], b.Render[channel][n])
			}
		}
	}
	return nil
}

func compareBounds(frame int, a, b acoustics.ErrorBounds) *werr.Error {
	values := []struct {
		field string
		left  float64
		right float64
	}{
		{"bounds.quantization.current", a.Quantization.Current, b.Quantization.Current},
		{"bounds.quantization.cumulative", a.Quantization.Cumulative, b.Quantization.Cumulative},
		{"bounds.energyTruncation.current", a.EnergyTruncation.Current, b.EnergyTruncation.Current},
		{"bounds.energyTruncation.cumulative", a.EnergyTruncation.Cumulative, b.EnergyTruncation.Cumulative},
		{"bounds.itd.current", a.ITD.Current, b.ITD.Current},
		{"bounds.itd.cumulative", a.ITD.Cumulative, b.ITD.Cumulative},
		{"bounds.convolutionInterp.current", a.ConvolutionInterp.Current, b.ConvolutionInterp.Current},
		{"bounds.convolutionInterp.cumulative", a.ConvolutionInterp.Cumulative, b.ConvolutionInterp.Cumulative},
	}
	for _, value := range values {
		if !floatEqual(value.left, value.right) {
			return diff(frame, "", value.field, value.left, value.right)
		}
	}
	return nil
}

func diff(frame int, channel, field string, left, right any) *werr.Error {
	e := werr.New(werr.CodeDiff, "first mismatch at frame %d field %s: left=%v right=%v", frame, field, left, right)
	e.Frame = frame
	e.Channel = channel
	e.Field = field
	e.Left = left
	e.Right = right
	e.Actual = left
	e.Expected = right
	return e
}

func floatEqual(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

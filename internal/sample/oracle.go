package sample

import (
	"channelpipe"
)

func attachTruth(truth *TruthFile, image *SourceImage, plan channelpipe.Plan, coordinates []struct {
	X, Y int
	Kind string
}) error {
	for _, coordinate := range coordinates {
		sourceBase := (coordinate.Y*image.Width + coordinate.X) * len(image.Channels)
		current := channelpipe.Buffer{
			Width:    1,
			Height:   1,
			Depth:    image.Depth,
			Channels: append([]channelpipe.Channel(nil), image.Channels...),
			Pixels:   append([]uint16(nil), image.Pixels[sourceBase:sourceBase+len(image.Channels)]...),
		}
		point := SamplePoint{ImageID: image.ID, X: coordinate.X, Y: coordinate.Y, Kind: coordinate.Kind}
		for stageIndex, stage := range plan.Stages {
			next, dropped := oracleStage(stage, current)
			stageTruth := StageTruth{
				StageIndex: stageIndex,
				Channels:   append([]channelpipe.Channel(nil), next.Channels...),
				Values:     append([]uint16(nil), next.Pixels...),
				Dropped:    dropped,
				Tolerance:  1,
			}
			if stageIndex == len(plan.Stages)-1 {
				if plan.SourceDepth == 16 {
					stageTruth.QuantizationErrorBound = 128
				} else {
					stageTruth.QuantizationErrorBound = 0
				}
			} else if stage.TargetDepth == 8 {
				stageTruth.QuantizationErrorBound = 1
			}
			point.Stages = append(point.Stages, stageTruth)
			current = next
		}
		truth.Samples = append(truth.Samples, point)
	}
	return nil
}

func oracleStage(stage channelpipe.Stage, input channelpipe.Buffer) (channelpipe.Buffer, []channelpipe.Channel) {
	output := channelpipe.Buffer{
		Width:    1,
		Height:   1,
		Depth:    stage.TargetDepth,
		Channels: append([]channelpipe.Channel(nil), stage.TargetChannels...),
		Pixels:   make([]uint16, len(stage.TargetChannels)),
	}
	used := map[int]bool{}
	for targetIndex, sourceIndex := range stage.Swizzle {
		if sourceIndex >= 0 {
			used[sourceIndex] = true
			output.Pixels[targetIndex] = oracleScale(input.Pixels[sourceIndex], input.Depth, stage.TargetDepth, stage.Rounding)
			continue
		}
		maxValue := uint16(channelpipe.DepthMax(stage.TargetDepth))
		switch stage.Fills[targetIndex].Kind {
		case channelpipe.FillZero:
			output.Pixels[targetIndex] = 0
		case channelpipe.FillOpaque:
			output.Pixels[targetIndex] = maxValue
		default:
			value := stage.Fills[targetIndex].Value
			if value > maxValue {
				value = maxValue
			}
			output.Pixels[targetIndex] = value
		}
	}
	dropped := []channelpipe.Channel{}
	for index, ch := range input.Channels {
		if !used[index] {
			dropped = append(dropped, ch)
		}
	}
	if stage.AlphaMode == channelpipe.AlphaOpaque {
		for index, ch := range output.Channels {
			if ch == channelpipe.ChannelA {
				output.Pixels[index] = uint16(channelpipe.DepthMax(output.Depth))
			}
		}
	}
	return output, dropped
}

func oracleScale(value uint16, fromDepth, toDepth int, rounding channelpipe.Rounding) uint16 {
	if fromDepth == toDepth {
		return value
	}
	fromMax := channelpipe.DepthMax(fromDepth)
	toMax := channelpipe.DepthMax(toDepth)
	numerator := uint64(value) * toMax
	var scaled uint64
	switch rounding {
	case channelpipe.RoundFloor:
		scaled = numerator / fromMax
	case channelpipe.RoundCeil:
		scaled = (numerator + fromMax - 1) / fromMax
	default:
		scaled = (numerator + fromMax/2) / fromMax
	}
	if scaled > toMax {
		scaled = toMax
	}
	return uint16(scaled)
}

func truthFingerprint(truth TruthFile) (string, error) {
	truth.Fingerprint = ""
	return channelpipe.Fingerprint(truth)
}

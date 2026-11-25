package channelpipe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

func DepthMax(depth int) uint64 {
	return uint64((1 << depth) - 1)
}

func QuantizeRatio(numerator, denominator uint64, rounding Rounding) uint64 {
	if denominator == 0 {
		return 0
	}
	switch rounding {
	case RoundFloor:
		return numerator / denominator
	case RoundCeil:
		return (numerator + denominator - 1) / denominator
	case RoundNearest, "":
		return (numerator + denominator/2) / denominator
	default:
		return (numerator + denominator/2) / denominator
	}
}

func ScaleValue(value uint16, fromDepth, toDepth int, rounding Rounding) uint16 {
	if fromDepth == toDepth {
		return value
	}
	fromMax := DepthMax(fromDepth)
	toMax := DepthMax(toDepth)
	scaled := QuantizeRatio(uint64(value)*toMax, fromMax, rounding)
	if scaled > toMax {
		scaled = toMax
	}
	return uint16(scaled)
}

func NormalizeRounding(rounding Rounding) Rounding {
	if rounding == "" {
		return RoundNearest
	}
	return rounding
}

func ValidatePlan(plan *Plan) error {
	return validatePlanAt(plan, "")
}

func validatePlanAt(plan *Plan, prefix string) error {
	if plan == nil {
		return FieldError(CodeSchema, "$", "plan is nil")
	}
	if plan.ID == "" {
		return FieldError(CodeSchema, prefix+"id", "plan id is required")
	}
	if plan.Width <= 0 {
		return FieldError(CodeSchema, prefix+"width", "width must be positive")
	}
	if plan.Height <= 0 {
		return FieldError(CodeSchema, prefix+"height", "height must be positive")
	}
	if plan.SourceDepth != 8 && plan.SourceDepth != 16 {
		return FieldError(CodeSchema, prefix+"source_depth", "source depth must be 8 or 16")
	}
	if len(plan.SourceLayout) == 0 {
		return FieldError(CodeSchema, prefix+"source_layout", "source layout is required")
	}
	inputLayout := append([]Channel(nil), plan.SourceLayout...)
	for stageIndex := range plan.Stages {
		stage := &plan.Stages[stageIndex]
		stage.Index = stageIndex
		if stage.ID == "" {
			stage.ID = fmt.Sprintf("stage-%d", stageIndex)
		}
		if stage.TargetDepth != 8 && stage.TargetDepth != 16 {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].target_depth", stageIndex), "target depth must be 8 or 16")
		}
		stage.Rounding = NormalizeRounding(stage.Rounding)
		if stage.Rounding != RoundNearest && stage.Rounding != RoundFloor && stage.Rounding != RoundCeil {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].rounding", stageIndex), "unsupported rounding %q", stage.Rounding)
		}
		if len(stage.TargetChannels) == 0 {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].target_channels", stageIndex), "target channels are required")
		}
		if len(stage.Swizzle) != len(stage.TargetChannels) {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].swizzle", stageIndex), "swizzle length must match target channels")
		}
		if len(stage.Fills) < len(stage.TargetChannels) {
			extended := make([]FillSpec, len(stage.TargetChannels))
			copy(extended, stage.Fills)
			stage.Fills = extended
		}
		for targetIndex, sourceIndex := range stage.Swizzle {
			path := prefix + fmt.Sprintf("stages[%d].swizzle[%d]", stageIndex, targetIndex)
			if sourceIndex < -1 || sourceIndex >= len(inputLayout) {
				return FieldError(CodeSchema, path, "source index %d is outside %d input channels", sourceIndex, len(inputLayout))
			}
			if sourceIndex == -1 {
				fill := stage.Fills[targetIndex]
				if fill.Kind == "" {
					return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].fills[%d]", stageIndex, targetIndex), "fill is required for added channel %s", stage.TargetChannels[targetIndex])
				}
				if fill.Kind != FillConstant && fill.Kind != FillZero && fill.Kind != FillOpaque {
					return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].fills[%d].kind", stageIndex, targetIndex), "unsupported fill kind %q", stage.TargetChannels[targetIndex])
				}
			}
		}
		if stage.DropPolicy == "" {
			stage.DropPolicy = DropRecord
		}
		if stage.DropPolicy != DropRecord && stage.DropPolicy != DropSilent {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].drop_policy", stageIndex), "unsupported drop policy %q", stage.DropPolicy)
		}
		if stage.AlphaMode == "" {
			stage.AlphaMode = AlphaPassthrough
		}
		if !validAlphaMode(stage.AlphaMode) {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].alpha_mode", stageIndex), "unsupported alpha mode %q", stage.AlphaMode)
		}
		if stage.AlphaMode == AlphaPremultiply && channelIndex(stage.TargetChannels, ChannelA) < 0 {
			return FieldError(CodeSchema, prefix+fmt.Sprintf("stages[%d].alpha_mode", stageIndex), "premultiply requires a target alpha channel")
		}
		inputLayout = append([]Channel(nil), stage.TargetChannels...)
	}
	return nil
}

func validAlphaMode(mode AlphaMode) bool {
	switch mode {
	case AlphaPassthrough, AlphaOpaque, AlphaPremultiply, AlphaDiscard:
		return true
	default:
		return false
	}
}

func channelIndex(channels []Channel, target Channel) int {
	for i, channel := range channels {
		if channel == target {
			return i
		}
	}
	return -1
}

func InputLayoutForStage(plan *Plan, stageIndex int) []Channel {
	if stageIndex == 0 {
		return plan.SourceLayout
	}
	return plan.Stages[stageIndex-1].TargetChannels
}

type StageResult struct {
	Buffer  Buffer
	Dropped []Channel
}

func ApplyStage(stage Stage, input Buffer) (StageResult, error) {
	if len(input.Pixels) != input.PixelCount()*input.ChannelCount() {
		return StageResult{}, NewError(CodeInvalid, "input pixel length %d does not match %dx%dx%d", len(input.Pixels), input.Width, input.Height, input.ChannelCount())
	}
	if len(stage.Swizzle) != len(stage.TargetChannels) {
		return StageResult{}, FieldError(CodeInvalid, "swizzle", "swizzle and target channel length differ")
	}
	output := Buffer{
		Width:    input.Width,
		Height:   input.Height,
		Depth:    stage.TargetDepth,
		Channels: append([]Channel(nil), stage.TargetChannels...),
		Pixels:   make([]uint16, input.PixelCount()*len(stage.TargetChannels)),
	}
	referenced := make(map[int]bool)
	targetMax := DepthMax(stage.TargetDepth)
	for pixel := 0; pixel < input.PixelCount(); pixel++ {
		inputBase := pixel * input.ChannelCount()
		outputBase := pixel * len(stage.TargetChannels)
		for targetChannel, sourceChannel := range stage.Swizzle {
			outputIndex := outputBase + targetChannel
			if sourceChannel >= 0 {
				referenced[sourceChannel] = true
				value := input.Pixels[inputBase+sourceChannel]
				output.Pixels[outputIndex] = ScaleValue(value, input.Depth, stage.TargetDepth, stage.Rounding)
				continue
			}
			fill := stage.Fills[targetChannel]
			switch fill.Kind {
			case FillZero:
				output.Pixels[outputIndex] = 0
			case FillOpaque:
				output.Pixels[outputIndex] = uint16(targetMax)
			case FillConstant:
				value := uint64(fill.Value)
				if value > targetMax {
					value = targetMax
				}
				output.Pixels[outputIndex] = uint16(value)
			default:
				return StageResult{}, FieldError(CodeSchema, "fills", "missing fill for target channel %d", targetChannel)
			}
		}
	}

	dropped := make([]Channel, 0)
	for sourceChannel := range input.Channels {
		if !referenced[sourceChannel] {
			dropped = append(dropped, input.Channels[sourceChannel])
		}
	}
	sortChannelsCanonical(dropped)

	if err := applyAlpha(&output, stage.AlphaMode, stage.Rounding); err != nil {
		return StageResult{}, err
	}
	return StageResult{Buffer: output, Dropped: dropped}, nil
}

func sortChannelsCanonical(channels []Channel) {
	for i := 0; i < len(channels); i++ {
		for j := i + 1; j < len(channels); j++ {
			if channels[j] < channels[i] {
				channels[i], channels[j] = channels[j], channels[i]
			}
		}
	}
}

func applyAlpha(buffer *Buffer, mode AlphaMode, rounding Rounding) error {
	alphaIndex := channelIndex(buffer.Channels, ChannelA)
	switch mode {
	case AlphaPassthrough, AlphaDiscard, "":
		return nil
	case AlphaOpaque:
		if alphaIndex < 0 {
			return FieldError(CodeInvalid, "alpha_mode", "opaque alpha requires A channel")
		}
		maxValue := DepthMax(buffer.Depth)
		for pixel := 0; pixel < buffer.PixelCount(); pixel++ {
			buffer.Pixels[pixel*buffer.ChannelCount()+alphaIndex] = uint16(maxValue)
		}
		return nil
	case AlphaPremultiply:
		if alphaIndex < 0 {
			return FieldError(CodeInvalid, "alpha_mode", "premultiply requires A channel")
		}
		maxValue := DepthMax(buffer.Depth)
		for pixel := 0; pixel < buffer.PixelCount(); pixel++ {
			base := pixel * buffer.ChannelCount()
			alpha := uint64(buffer.Pixels[base+alphaIndex])
			for channel := range buffer.Channels {
				if channel == alphaIndex || buffer.Channels[channel] == ChannelA {
					continue
				}
				value := uint64(buffer.Pixels[base+channel])
				scaled := QuantizeRatio(value*alpha, maxValue, rounding)
				if scaled > maxValue {
					scaled = maxValue
				}
				buffer.Pixels[base+channel] = uint16(scaled)
			}
		}
		return nil
	default:
		return FieldError(CodeInvalid, "alpha_mode", "unsupported alpha mode %q", mode)
	}
}

func EncodeBuffer(buffer *Buffer) ([]byte, error) {
	if buffer == nil {
		return nil, NewError(CodeInvalid, "cannot encode nil buffer")
	}
	channelCount := buffer.ChannelCount()
	samples := buffer.PixelCount() * channelCount
	switch buffer.Depth {
	case 8:
		data := make([]byte, samples)
		for i, value := range buffer.Pixels {
			if value > math.MaxUint8 {
				value = math.MaxUint8
			}
			data[i] = byte(value)
		}
		return data, nil
	case 16:
		data := make([]byte, samples*2)
		for i, value := range buffer.Pixels {
			binary.LittleEndian.PutUint16(data[i*2:i*2+2], value)
		}
		return data, nil
	default:
		return nil, FieldError(CodeInvalid, "depth", "unsupported encode depth %d", buffer.Depth)
	}
}

func DecodeBuffer(data []byte, width, height, channels, depth int) (*Buffer, error) {
	if width <= 0 || height <= 0 || channels <= 0 {
		return nil, NewError(CodeInvalid, "invalid buffer geometry")
	}
	pixelCount := width * height
	sampleCount := pixelCount * channels
	buffer := &Buffer{Width: width, Height: height, Depth: depth, Pixels: make([]uint16, sampleCount)}
	switch depth {
	case 8:
		if len(data) != sampleCount {
			return nil, NewError(CodeInvalid, "8-bit blob length %d, want %d", len(data), sampleCount)
		}
		for i, value := range data {
			buffer.Pixels[i] = uint16(value)
		}
	case 16:
		if len(data) != sampleCount*2 {
			return nil, NewError(CodeInvalid, "16-bit blob length %d, want %d", len(data), sampleCount*2)
		}
		for i := 0; i < sampleCount; i++ {
			buffer.Pixels[i] = binary.LittleEndian.Uint16(data[i*2 : i*2+2])
		}
	default:
		return nil, FieldError(CodeInvalid, "depth", "unsupported decode depth %d", depth)
	}
	return buffer, nil
}

func RegionHash(buffer *Buffer) (string, error) {
	if buffer == nil {
		return SHA256Hex([]byte("nil")), nil
	}
	blob, err := EncodeBuffer(buffer)
	if err != nil {
		return "", err
	}
	header, err := CanonicalJSON(struct {
		Width    int       `json:"width"`
		Height   int       `json:"height"`
		Depth    int       `json:"depth"`
		Channels []Channel `json:"channels"`
	}{buffer.Width, buffer.Height, buffer.Depth, buffer.Channels})
	if err != nil {
		return "", err
	}
	return SHA256Hex(header, blob), nil
}

func StageCacheKey(regionHash string, stageIndex int, stage Stage, upstreamKey string) (string, error) {
	semanticStage := stage
	semanticStage.Metadata = nil
	params, err := CanonicalJSON(semanticStage)
	if err != nil {
		return "", err
	}
	payload, err := CanonicalJSON(struct {
		CacheVersion     string `json:"cache_version"`
		RegionHash       string `json:"region_hash"`
		StageIndex       int    `json:"stage_index"`
		Stage            Stage  `json:"stage"`
		UpstreamKey      string `json:"upstream_key"`
		ParamVersion     string `json:"param_version"`
		AlgorithmVersion string `json:"algorithm_version"`
	}{CacheVersion, regionHash, stageIndex, semanticStage, upstreamKey, ParamVersion, AlgorithmVersion})
	if err != nil {
		return "", err
	}
	_ = params
	return SHA256Hex(payload), nil
}

func BufferBytesEqual(a, b *Buffer) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Width != b.Width || a.Height != b.Height || a.Depth != b.Depth || len(a.Channels) != len(b.Channels) || len(a.Pixels) != len(b.Pixels) {
		return false
	}
	if !bytes.Equal(channelBytes(a.Channels), channelBytes(b.Channels)) {
		return false
	}
	for i := range a.Pixels {
		if a.Pixels[i] != b.Pixels[i] {
			return false
		}
	}
	return true
}

func channelBytes(channels []Channel) []byte {
	result := make([]byte, len(channels))
	for i, channel := range channels {
		if len(channel) > 0 {
			result[i] = channel[0]
		}
	}
	return result
}

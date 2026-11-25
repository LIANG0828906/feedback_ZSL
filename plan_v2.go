package channelpipe

import "encoding/json"

type v2Envelope struct {
	SchemaVersion *int         `json:"schemaVersion"`
	PlanSetID     string       `json:"planSetId"`
	Pipelines     []v2Pipeline `json:"pipelines"`
}

type v2Source struct {
	Width       *int    `json:"width"`
	Height      *int    `json:"height"`
	BitDepth    *int    `json:"bitDepth"`
	PixelFormat *string `json:"pixelFormat"`
}

type v2Pipeline struct {
	ID         *string       `json:"id"`
	Seed       *int64        `json:"seed"`
	Source     *v2Source     `json:"source"`
	Operations []v2Operation `json:"operations"`
}

type v2Quantization struct {
	OutputDepth *int    `json:"outputDepth"`
	Rounding    *string `json:"rounding"`
}

type v2ChannelFlow struct {
	TargetFormat *string   `json:"targetFormat"`
	Sources      []*string `json:"sources"`
}

type v2FillSource struct {
	Strategy *string           `json:"strategy"`
	Samples  map[string]uint16 `json:"samples"`
}

type v2Operation struct {
	OpID            *string         `json:"opId"`
	Quantization    *v2Quantization `json:"quantization"`
	ChannelFlow     *v2ChannelFlow  `json:"channelFlow"`
	FillSource      *v2FillSource   `json:"fillSource"`
	Removal         *string         `json:"removal"`
	AlphaConvention *string         `json:"alphaConvention"`
}

func loadV2(data []byte, versionField string) (*LoadResult, error) {
	var raw v2Envelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, WrapError(CodeSchema, err, "$", "v2 plan cannot be parsed")
	}
	if raw.SchemaVersion == nil {
		return nil, FieldError(CodeSchema, versionField, "required field is missing")
	}
	if raw.Pipelines == nil {
		return nil, FieldError(CodeSchema, "pipelines", "required field is missing")
	}

	result := &LoadResult{SourceVersion: 2, VersionField: versionField}
	set := PlanSet{ID: raw.PlanSetID}
	for pipelineIndex, pipeline := range raw.Pipelines {
		prefix := "pipelines[" + itoa(pipelineIndex) + "]"
		planID, err := requirePointer(pipeline.ID, prefix+".id")
		if err != nil {
			return nil, err
		}
		if pipeline.Source == nil {
			return nil, FieldError(CodeSchema, prefix+".source", "required field is missing")
		}
		width, err := requirePointer(pipeline.Source.Width, prefix+".source.width")
		if err != nil {
			return nil, err
		}
		height, err := requirePointer(pipeline.Source.Height, prefix+".source.height")
		if err != nil {
			return nil, err
		}
		depth, err := requirePointer(pipeline.Source.BitDepth, prefix+".source.bitDepth")
		if err != nil {
			return nil, err
		}
		format, err := requirePointer(pipeline.Source.PixelFormat, prefix+".source.pixelFormat")
		if err != nil {
			return nil, err
		}
		layout, err := parseLayout(format, prefix+".source.pixelFormat")
		if err != nil {
			return nil, err
		}
		seed := int64Default(pipeline.Seed, DefaultSeed, planID, prefix+".seed", &result.UpgradeTraces)
		if pipelineIndex == 0 {
			set.Seed = seed
		}
		if set.ID == "" {
			set.ID = "v2-set"
		}

		plan := Plan{ID: planID, Seed: seed, Width: width, Height: height, SourceDepth: depth, SourceLayout: layout}
		inputLayout := append([]Channel(nil), layout...)
		for opIndex, op := range pipeline.Operations {
			opPrefix := prefix + ".operations[" + itoa(opIndex) + "]"
			if op.Quantization == nil {
				return nil, FieldError(CodeSchema, opPrefix+".quantization", "required field is missing")
			}
			targetDepth, err := requirePointer(op.Quantization.OutputDepth, opPrefix+".quantization.outputDepth")
			if err != nil {
				return nil, err
			}
			if op.ChannelFlow == nil {
				return nil, FieldError(CodeSchema, opPrefix+".channelFlow", "required field is missing")
			}
			targetText, err := requirePointer(op.ChannelFlow.TargetFormat, opPrefix+".channelFlow.targetFormat")
			if err != nil {
				return nil, err
			}
			targetChannels, err := parseLayout(targetText, opPrefix+".channelFlow.targetFormat")
			if err != nil {
				return nil, err
			}
			if op.ChannelFlow.Sources == nil {
				return nil, FieldError(CodeSchema, opPrefix+".channelFlow.sources", "required field is missing")
			}
			if len(op.ChannelFlow.Sources) != len(targetChannels) {
				return nil, FieldError(CodeSchema, opPrefix+".channelFlow.sources", "sources length must match targetFormat")
			}

			opID := stringDefault(op.OpID, "stage-"+itoa(opIndex), planID, opIndex, opPrefix+".opId", &result.UpgradeTraces)
			roundText := stringDefault(op.Quantization.Rounding, "ROUND_NEAREST", planID, opIndex, opPrefix+".quantization.rounding", &result.UpgradeTraces)
			rounding, err := v2Rounding(roundText, opPrefix+".quantization.rounding")
			if err != nil {
				return nil, err
			}
			removalText := stringDefault(op.Removal, "RECORD_DISCARDS", planID, opIndex, opPrefix+".removal", &result.UpgradeTraces)
			dropPolicy, err := v2Removal(removalText, opPrefix+".removal")
			if err != nil {
				return nil, err
			}
			alphaText := stringDefault(op.AlphaConvention, "ALPHA_PASSTHROUGH", planID, opIndex, opPrefix+".alphaConvention", &result.UpgradeTraces)
			alphaMode, err := v2Alpha(alphaText, opPrefix+".alphaConvention")
			if err != nil {
				return nil, err
			}

			stage := Stage{
				Index:          opIndex,
				ID:             opID,
				TargetDepth:    targetDepth,
				Rounding:       rounding,
				TargetChannels: targetChannels,
				DropPolicy:     dropPolicy,
				AlphaMode:      alphaMode,
			}
			stage.Swizzle = make([]int, len(targetChannels))
			stage.Fills = make([]FillSpec, len(targetChannels))
			for targetIndex, sourceRef := range op.ChannelFlow.Sources {
				refPath := opPrefix + ".channelFlow.sources[" + itoa(targetIndex) + "]"
				if sourceRef != nil && *sourceRef != "" {
					sourceChannel := Channel(*sourceRef)
					sourceIndex, err := resolveChannelIndex(inputLayout, sourceChannel, refPath)
					if err != nil {
						return nil, err
					}
					stage.Swizzle[targetIndex] = sourceIndex
					continue
				}
				stage.Swizzle[targetIndex] = -1
				fill, err := v2Fill(op.FillSource, targetChannels[targetIndex], targetDepth, opPrefix+".fillSource")
				if err != nil {
					return nil, err
				}
				stage.Fills[targetIndex] = fill
			}
			plan.Stages = append(plan.Stages, stage)
			inputLayout = append([]Channel(nil), targetChannels...)
		}
		set.Plans = append(set.Plans, plan)
	}
	if err := ValidateSet(&set); err != nil {
		return nil, err
	}
	result.Set = set
	return result, nil
}

func v2Rounding(value, path string) (Rounding, error) {
	switch value {
	case "ROUND_NEAREST", "nearest":
		return RoundNearest, nil
	case "TRUNCATE", "ROUND_FLOOR", "floor":
		return RoundFloor, nil
	case "CEIL", "ROUND_CEIL", "ceil":
		return RoundCeil, nil
	default:
		return "", FieldError(CodeSchema, path, "unknown rounding convention %q", value)
	}
}

func v2Removal(value, path string) (DropPolicy, error) {
	switch value {
	case "RECORD_DISCARDS", "record":
		return DropRecord, nil
	case "DISCARD_SILENT", "silent":
		return DropSilent, nil
	default:
		return "", FieldError(CodeSchema, path, "unknown removal convention %q", value)
	}
}

func v2Alpha(value, path string) (AlphaMode, error) {
	switch value {
	case "ALPHA_PASSTHROUGH", "passthrough":
		return AlphaPassthrough, nil
	case "ALPHA_OPAQUE", "opaque":
		return AlphaOpaque, nil
	case "ALPHA_PREMULTIPLY", "premultiply":
		return AlphaPremultiply, nil
	case "ALPHA_DISCARD", "discard":
		return AlphaDiscard, nil
	default:
		return "", FieldError(CodeSchema, path, "unknown alpha convention %q", value)
	}
}

func v2Fill(source *v2FillSource, channel Channel, depth int, path string) (FillSpec, error) {
	if source == nil || source.Strategy == nil {
		return FillSpec{}, FieldError(CodeSchema, path, "fillSource is required for a missing source channel")
	}
	switch *source.Strategy {
	case "ZERO":
		return FillSpec{Kind: FillZero}, nil
	case "OPAQUE":
		return FillSpec{Kind: FillOpaque}, nil
	case "SOLID":
		value, ok := source.Samples[string(channel)]
		if !ok {
			return FillSpec{}, FieldError(CodeSchema, path+".samples."+string(channel), "solid fill sample is missing")
		}
		if uint64(value) > DepthMax(depth) {
			return FillSpec{}, FieldError(CodeSchema, path+".samples."+string(channel), "fill value exceeds depth range")
		}
		return FillSpec{Kind: FillConstant, Value: value}, nil
	default:
		return FillSpec{}, FieldError(CodeSchema, path+".strategy", "unknown fill strategy %q", *source.Strategy)
	}
}

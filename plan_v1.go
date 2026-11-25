package channelpipe

import "encoding/json"

type v1Envelope struct {
	PlanVer *int    `json:"plan_ver"`
	Catalog string  `json:"catalog"`
	Jobs    []v1Job `json:"jobs"`
}

type v1Job struct {
	JobID      *string  `json:"job_id"`
	Seed       *int64   `json:"seed"`
	SrcWidth   *int     `json:"src_width"`
	SrcHeight  *int     `json:"src_height"`
	SrcBitness *int     `json:"src_bitness"`
	SrcLayout  *string  `json:"src_layout"`
	Tasks      []v1Task `json:"tasks"`
}

type v1Pad struct {
	Policy   string           `json:"policy"`
	Code     *uint16          `json:"code"`
	Channels map[string]v1Pad `json:"channels,omitempty"`
}

type v1Task struct {
	TaskID     *string           `json:"task_id"`
	DepthCode  *string           `json:"depth_code"`
	OutLayout  *string           `json:"out_layout"`
	IndexMap   []int             `json:"index_map"`
	Pad        *v1Pad            `json:"pad"`
	RoundMode  *string           `json:"round_mode"`
	DropPolicy *string           `json:"drop_policy"`
	AlphaRule  *int              `json:"alpha_rule"`
	Metadata   map[string]string `json:"metadata"`
}

func loadV1(data []byte, versionField string) (*LoadResult, error) {
	var raw v1Envelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, WrapError(CodeSchema, err, "$", "v1 plan cannot be parsed")
	}
	if raw.PlanVer == nil {
		return nil, FieldError(CodeSchema, versionField, "required field is missing")
	}
	if raw.Jobs == nil {
		return nil, FieldError(CodeSchema, "jobs", "required field is missing")
	}

	result := &LoadResult{SourceVersion: 1, VersionField: versionField}
	set := PlanSet{ID: raw.Catalog, Seed: DefaultSeed}
	for jobIndex, job := range raw.Jobs {
		prefix := "jobs[" + itoa(jobIndex) + "]"
		planID, err := requirePointer(job.JobID, prefix+".job_id")
		if err != nil {
			return nil, err
		}
		width, err := requirePointer(job.SrcWidth, prefix+".src_width")
		if err != nil {
			return nil, err
		}
		height, err := requirePointer(job.SrcHeight, prefix+".src_height")
		if err != nil {
			return nil, err
		}
		bitness, err := requirePointer(job.SrcBitness, prefix+".src_bitness")
		if err != nil {
			return nil, err
		}
		layoutText, err := requirePointer(job.SrcLayout, prefix+".src_layout")
		if err != nil {
			return nil, err
		}
		layout, err := parseLayout(layoutText, prefix+".src_layout")
		if err != nil {
			return nil, err
		}
		seed := int64Default(job.Seed, DefaultSeed, planID, prefix+".seed", &result.UpgradeTraces)
		if jobIndex == 0 {
			set.Seed = seed
		}
		if raw.Catalog == "" {
			set.ID = "v1-catalog"
		}

		plan := Plan{
			ID:           planID,
			Seed:         seed,
			Width:        width,
			Height:       height,
			SourceDepth:  bitness,
			SourceLayout: layout,
		}
		inputLayout := append([]Channel(nil), layout...)
		for taskIndex, task := range job.Tasks {
			taskPrefix := prefix + ".tasks[" + itoa(taskIndex) + "]"
			depthCode, err := requirePointer(task.DepthCode, taskPrefix+".depth_code")
			if err != nil {
				return nil, err
			}
			outLayoutText, err := requirePointer(task.OutLayout, taskPrefix+".out_layout")
			if err != nil {
				return nil, err
			}
			outLayout, err := parseLayout(outLayoutText, taskPrefix+".out_layout")
			if err != nil {
				return nil, err
			}
			targetDepth, err := v1Depth(depthCode, taskPrefix+".depth_code")
			if err != nil {
				return nil, err
			}
			if task.IndexMap == nil {
				return nil, FieldError(CodeSchema, taskPrefix+".index_map", "required field is missing")
			}
			if len(task.IndexMap) != len(outLayout) {
				return nil, FieldError(CodeSchema, taskPrefix+".index_map", "index_map length must equal out_layout length")
			}
			taskID := stringDefault(task.TaskID, "stage-"+itoa(taskIndex), planID, taskIndex, taskPrefix+".task_id", &result.UpgradeTraces)
			roundingText := stringDefault(task.RoundMode, "nearest", planID, taskIndex, taskPrefix+".round_mode", &result.UpgradeTraces)
			rounding, err := v1Rounding(roundingText, taskPrefix+".round_mode")
			if err != nil {
				return nil, err
			}
			dropText := stringDefault(task.DropPolicy, "report", planID, taskIndex, taskPrefix+".drop_policy", &result.UpgradeTraces)
			dropPolicy, err := v1Drop(dropText, taskPrefix+".drop_policy")
			if err != nil {
				return nil, err
			}
			alphaRule := intDefault(task.AlphaRule, 0, planID, taskIndex, taskPrefix+".alpha_rule", &result.UpgradeTraces)
			alphaMode, err := v1Alpha(alphaRule, taskPrefix+".alpha_rule")
			if err != nil {
				return nil, err
			}

			stage := Stage{
				Index:          taskIndex,
				ID:             taskID,
				TargetDepth:    targetDepth,
				Rounding:       rounding,
				TargetChannels: outLayout,
				Swizzle:        append([]int(nil), task.IndexMap...),
				DropPolicy:     dropPolicy,
				AlphaMode:      alphaMode,
				Metadata:       task.Metadata,
			}
			stage.Fills = make([]FillSpec, len(outLayout))
			for targetIndex, sourceIndex := range stage.Swizzle {
				if sourceIndex != -1 {
					continue
				}
				if task.Pad == nil {
					return nil, FieldError(CodeSchema, taskPrefix+".pad", "pad policy is required for added channel %s", outLayout[targetIndex])
				}
				fill, err := v1PadFill(*task.Pad, targetDepth, outLayout[targetIndex], taskPrefix+".pad")
				if err != nil {
					return nil, err
				}
				stage.Fills[targetIndex] = fill
			}
			if err := validateSwizzleRange(stage.Swizzle, inputLayout, taskPrefix+".index_map"); err != nil {
				return nil, err
			}
			plan.Stages = append(plan.Stages, stage)
			inputLayout = append([]Channel(nil), outLayout...)
		}
		set.Plans = append(set.Plans, plan)
	}
	if err := ValidateSet(&set); err != nil {
		return nil, err
	}
	result.Set = set
	return result, nil
}

func validateSwizzleRange(swizzle []int, inputLayout []Channel, path string) error {
	for _, index := range swizzle {
		if index < -1 || index >= len(inputLayout) {
			return FieldError(CodeSchema, path, "source index %d is outside input layout length %d", index, len(inputLayout))
		}
	}
	return nil
}

func v1Depth(code, path string) (int, error) {
	switch code {
	case "D8", "8BIT", "8":
		return 8, nil
	case "D16", "16BIT", "16":
		return 16, nil
	default:
		return 0, FieldError(CodeSchema, path, "unknown depth_code %q", code)
	}
}

func v1Rounding(value, path string) (Rounding, error) {
	switch value {
	case "nearest", "N":
		return RoundNearest, nil
	case "down", "floor", "F":
		return RoundFloor, nil
	case "up", "ceil", "C":
		return RoundCeil, nil
	default:
		return "", FieldError(CodeSchema, path, "unknown round mode %q", value)
	}
}

func v1Drop(value, path string) (DropPolicy, error) {
	switch value {
	case "report", "REPORT", "record":
		return DropRecord, nil
	case "silent", "SILENT", "ignore":
		return DropSilent, nil
	default:
		return "", FieldError(CodeSchema, path, "unknown drop policy %q", value)
	}
}

func v1Alpha(rule int, path string) (AlphaMode, error) {
	switch rule {
	case 0:
		return AlphaPassthrough, nil
	case 1:
		return AlphaOpaque, nil
	case 2:
		return AlphaPremultiply, nil
	case 3:
		return AlphaDiscard, nil
	default:
		return "", FieldError(CodeSchema, path, "unknown alpha_rule %d", rule)
	}
}

func v1PadFill(pad v1Pad, targetDepth int, channel Channel, path string) (FillSpec, error) {
	if child, ok := pad.Channels[string(channel)]; ok {
		return v1PadFill(child, targetDepth, channel, path+".channels."+string(channel))
	}
	switch pad.Policy {
	case "PAD_ZERO", "zero":
		return FillSpec{Kind: FillZero}, nil
	case "PAD_OPAQUE", "opaque":
		return FillSpec{Kind: FillOpaque}, nil
	case "PAD_CONSTANT", "constant":
		if pad.Code == nil {
			return FillSpec{}, FieldError(CodeSchema, path+".code", "constant pad requires code")
		}
		maxValue := DepthMax(targetDepth)
		if uint64(*pad.Code) > maxValue {
			return FillSpec{}, FieldError(CodeSchema, path+".code", "pad code %d exceeds %d-bit max %d", *pad.Code, targetDepth, maxValue)
		}
		return FillSpec{Kind: FillConstant, Value: *pad.Code}, nil
	default:
		return FillSpec{}, FieldError(CodeSchema, path+".policy", "unknown pad policy %q", pad.Policy)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		buffer[position] = '-'
	}
	return string(buffer[position:])
}

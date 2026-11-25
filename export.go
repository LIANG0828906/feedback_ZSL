package channelpipe

import (
	"encoding/json"
	"fmt"
)

func ExportSetAs(set PlanSet, version int) ([]byte, *LossReport, error) {
	if err := ValidateSet(&set); err != nil {
		return nil, nil, err
	}
	report := &LossReport{TargetVersion: version}
	switch version {
	case 1:
		return exportV1(set, report)
	case 2:
		return exportV2(set, report)
	case 3:
		return exportV3(set, report)
	default:
		return nil, nil, FieldError(CodeVersion, "version", "unsupported export version %d", version)
	}
}

func ExportSetAsStrict(set PlanSet, version int) ([]byte, *LossReport, error) {
	data, report, err := ExportSetAs(set, version)
	if err != nil {
		return data, report, err
	}
	if report.HasLoss() {
		lossyErr := NewError(CodeLossy, "export to version %d contains %d non-semantic lossy item(s)", version, len(report.Items)).
			withDetails(map[string]any{"loss_report": report})
		return data, report, lossyErr
	}
	return data, report, nil
}

func ExportPlanAs(plan Plan, version int) ([]byte, *LossReport, error) {
	set := PlanSet{ID: plan.ID, Seed: plan.Seed, Plans: []Plan{plan}}
	return ExportSetAs(set, version)
}

func exportV3(set PlanSet, report *LossReport) ([]byte, *LossReport, error) {
	data, err := json.MarshalIndent(v3Envelope{Version: intPtr(3), ID: set.ID, Seed: set.Seed, Plans: set.Plans}, "", "  ")
	if err != nil {
		return nil, report, WrapError(CodeIO, err, "$", "cannot encode v3 plan")
	}
	return data, report, nil
}

func exportV2(set PlanSet, report *LossReport) ([]byte, *LossReport, error) {
	envelope := v2Envelope{SchemaVersion: intPtr(2), PlanSetID: set.ID}
	for planIndex, plan := range set.Plans {
		pipeline := v2Pipeline{
			ID:   stringPtr(plan.ID),
			Seed: int64Ptr(plan.Seed),
			Source: &v2Source{
				Width:       intPtr(plan.Width),
				Height:      intPtr(plan.Height),
				BitDepth:    intPtr(plan.SourceDepth),
				PixelFormat: stringPtr(layoutString(plan.SourceLayout)),
			},
		}
		for stageIndex, stage := range plan.Stages {
			recordMetadataLoss(report, plan.ID, stageIndex, stage.Metadata, "v2 does not declare arbitrary stage metadata; metadata is discarded while pixel semantics are retained")
			op := v2Operation{
				OpID:            stringPtr(stage.ID),
				Quantization:    &v2Quantization{OutputDepth: intPtr(stage.TargetDepth), Rounding: stringPtr(v2RoundingValue(stage.Rounding))},
				Removal:         stringPtr(v2DropValue(stage.DropPolicy)),
				AlphaConvention: stringPtr(v2AlphaValue(stage.AlphaMode)),
				ChannelFlow: &v2ChannelFlow{
					TargetFormat: stringPtr(layoutString(stage.TargetChannels)),
					Sources:      make([]*string, len(stage.Swizzle)),
				},
			}
			samples := map[string]uint16{}
			hasFill := false
			inputLayout := InputLayoutForStage(&plan, stageIndex)
			for targetIndex, sourceIndex := range stage.Swizzle {
				if sourceIndex >= 0 {
					channel := string(inputLayout[sourceIndex])
					op.ChannelFlow.Sources[targetIndex] = &channel
					continue
				}
				op.ChannelFlow.Sources[targetIndex] = nil
				value := normalizedFillValue(stage.Fills[targetIndex], stage.TargetDepth)
				samples[string(stage.TargetChannels[targetIndex])] = value
				hasFill = true
			}
			if hasFill {
				op.FillSource = &v2FillSource{Strategy: stringPtr("SOLID"), Samples: samples}
			}
			if NormalizeRounding(stage.Rounding) == RoundNearest && op.Quantization != nil {
				op.Quantization.Rounding = nil
			}
			if stage.DropPolicy == DropRecord || stage.DropPolicy == "" {
				op.Removal = nil
			}
			if stage.AlphaMode == AlphaPassthrough || stage.AlphaMode == "" {
				op.AlphaConvention = nil
			}
			pipeline.Operations = append(pipeline.Operations, op)
		}
		envelope.Pipelines = append(envelope.Pipelines, pipeline)
		_ = planIndex
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, report, WrapError(CodeIO, err, "$", "cannot encode v2 plan")
	}
	return data, report, nil
}

func exportV1(set PlanSet, report *LossReport) ([]byte, *LossReport, error) {
	envelope := v1Envelope{PlanVer: intPtr(1), Catalog: set.ID}
	for _, plan := range set.Plans {
		job := v1Job{
			JobID:      stringPtr(plan.ID),
			Seed:       int64Ptr(plan.Seed),
			SrcWidth:   intPtr(plan.Width),
			SrcHeight:  intPtr(plan.Height),
			SrcBitness: intPtr(plan.SourceDepth),
			SrcLayout:  stringPtr(layoutString(plan.SourceLayout)),
		}
		for stageIndex, stage := range plan.Stages {
			recordMetadataLoss(report, plan.ID, stageIndex, stage.Metadata, "v1 has no arbitrary metadata extension; metadata is discarded while conversion semantics are retained")
			task := v1Task{
				TaskID:     stringPtr(stage.ID),
				DepthCode:  stringPtr(fmt.Sprintf("D%d", stage.TargetDepth)),
				OutLayout:  stringPtr(layoutString(stage.TargetChannels)),
				IndexMap:   append([]int(nil), stage.Swizzle...),
				RoundMode:  stringPtr(v1RoundingValue(stage.Rounding)),
				DropPolicy: stringPtr(v1DropValue(stage.DropPolicy)),
				AlphaRule:  intPtr(v1AlphaValue(stage.AlphaMode)),
			}
			if NormalizeRounding(stage.Rounding) == RoundNearest {
				task.RoundMode = nil
			}
			if stage.DropPolicy == DropRecord || stage.DropPolicy == "" {
				task.DropPolicy = nil
			}
			if stage.AlphaMode == AlphaPassthrough || stage.AlphaMode == "" {
				task.AlphaRule = nil
			}
			pad, err := buildV1Pad(stage)
			if err != nil {
				return nil, report, err
			}
			task.Pad = pad
			job.Tasks = append(job.Tasks, task)
		}
		envelope.Jobs = append(envelope.Jobs, job)
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, report, WrapError(CodeIO, err, "$", "cannot encode v1 plan")
	}
	return data, report, nil
}

func buildV1Pad(stage Stage) (*v1Pad, error) {
	values := make(map[Channel]uint16)
	first := uint16(0)
	haveFirst := false
	allSame := true
	for targetIndex, sourceIndex := range stage.Swizzle {
		if sourceIndex >= 0 {
			continue
		}
		value := normalizedFillValue(stage.Fills[targetIndex], stage.TargetDepth)
		channel := stage.TargetChannels[targetIndex]
		values[channel] = value
		if !haveFirst {
			first = value
			haveFirst = true
		} else if value != first {
			allSame = false
		}
	}
	if !haveFirst {
		return nil, nil
	}
	if allSame {
		return &v1Pad{Policy: "PAD_CONSTANT", Code: &first}, nil
	}
	pad := &v1Pad{Policy: "PAD_PER_CHANNEL", Channels: map[string]v1Pad{}}
	for channel, value := range values {
		valueCopy := value
		pad.Channels[string(channel)] = v1Pad{Policy: "PAD_CONSTANT", Code: &valueCopy}
	}
	return pad, nil
}

func normalizedFillValue(fill FillSpec, depth int) uint16 {
	maxValue := uint16(DepthMax(depth))
	switch fill.Kind {
	case FillZero:
		return 0
	case FillOpaque:
		return maxValue
	case FillConstant:
		if fill.Value > maxValue {
			return maxValue
		}
		return fill.Value
	default:
		return 0
	}
}

func recordMetadataLoss(report *LossReport, planID string, stageIndex int, metadata map[string]string, disposition string) {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	for _, key := range keys {
		report.Items = append(report.Items, LossyItem{
			Code:        CodeLossy,
			PlanID:      planID,
			StageIndex:  stageIndex,
			FieldPath:   fmt.Sprintf("stages[%d].metadata.%s", stageIndex, key),
			Feature:     "arbitrary-stage-metadata",
			Message:     fmt.Sprintf("metadata key %q cannot be represented by target version", key),
			Disposition: disposition,
		})
	}
}

func v1RoundingValue(rounding Rounding) string {
	switch NormalizeRounding(rounding) {
	case RoundFloor:
		return "down"
	case RoundCeil:
		return "up"
	default:
		return "nearest"
	}
}

func v2RoundingValue(rounding Rounding) string {
	switch NormalizeRounding(rounding) {
	case RoundFloor:
		return "TRUNCATE"
	case RoundCeil:
		return "CEIL"
	default:
		return "ROUND_NEAREST"
	}
}

func v1DropValue(policy DropPolicy) string {
	if policy == DropSilent {
		return "silent"
	}
	return "report"
}

func v2DropValue(policy DropPolicy) string {
	if policy == DropSilent {
		return "DISCARD_SILENT"
	}
	return "RECORD_DISCARDS"
}

func v1AlphaValue(mode AlphaMode) int {
	switch mode {
	case AlphaOpaque:
		return 1
	case AlphaPremultiply:
		return 2
	case AlphaDiscard:
		return 3
	default:
		return 0
	}
}

func v2AlphaValue(mode AlphaMode) string {
	switch mode {
	case AlphaOpaque:
		return "ALPHA_OPAQUE"
	case AlphaPremultiply:
		return "ALPHA_PREMULTIPLY"
	case AlphaDiscard:
		return "ALPHA_DISCARD"
	default:
		return "ALPHA_PASSTHROUGH"
	}
}

func intPtr(value int) *int          { return &value }
func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }

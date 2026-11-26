package pipeline

import (
	"encoding/json"

	"iconflow/errs"
	"iconflow/model"
)

type EffectiveParams struct {
	ThresholdPermyriad int
	ChannelOrder       []string
	Quant              model.QuantSpec
	OperatorVersion    string
}

func CloneProject(project *model.Project) (*model.Project, *errs.Error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, errs.New(errs.EInternal, "$.project", "clone encode: %v", err)
	}
	var clone model.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, errs.New(errs.EInternal, "$.project", "clone decode: %v", err)
	}
	return &clone, nil
}

func Effective(project *model.Project, key string) EffectiveParams {
	params := EffectiveParams{
		ThresholdPermyriad: project.ThresholdPermyriad,
		ChannelOrder:       append([]string(nil), project.ChannelOrder...),
		Quant:              project.Quant,
		OperatorVersion:    project.Rules.OperatorVersion,
	}
	if project.Overrides != nil {
		if override, ok := project.Overrides[key]; ok {
			if override.ThresholdPermyriad != nil {
				params.ThresholdPermyriad = *override.ThresholdPermyriad
			}
			if len(override.ChannelOrder) > 0 {
				params.ChannelOrder = append([]string(nil), override.ChannelOrder...)
			}
			if override.QuantStep != nil {
				params.Quant.Step = *override.QuantStep
			}
			if override.OperatorVersion != "" {
				params.OperatorVersion = override.OperatorVersion
			}
		}
	}
	return params
}

type Edit struct {
	Field    string `json:"field"`
	Value    any    `json:"value"`
	BlockKey string `json:"block_key,omitempty"`
}

func ApplyEdit(project *model.Project, edit Edit) *errs.Error {
	switch edit.Field {
	case "threshold_scale", "threshold_permyriad":
		value, ok := edit.Value.(int)
		if !ok {
			return errs.Schema("$.edit.value", "threshold edit requires int, got %T", edit.Value)
		}
		if edit.BlockKey == "" {
			project.ThresholdPermyriad = value
			project.Rules.QuantilePermyriad = value
		} else {
			override := project.Overrides[edit.BlockKey]
			override.ThresholdPermyriad = &value
			if project.Overrides == nil {
				project.Overrides = map[string]model.BlockOverride{}
			}
			project.Overrides[edit.BlockKey] = override
		}
	case "channel_order":
		value, ok := edit.Value.([]string)
		if !ok || len(value) == 0 {
			return errs.Schema("$.edit.value", "channel_order edit requires non-empty []string")
		}
		if edit.BlockKey == "" {
			project.ChannelOrder = append([]string(nil), value...)
		} else {
			override := project.Overrides[edit.BlockKey]
			override.ChannelOrder = append([]string(nil), value...)
			if project.Overrides == nil {
				project.Overrides = map[string]model.BlockOverride{}
			}
			project.Overrides[edit.BlockKey] = override
		}
	case "quant_step":
		value, ok := edit.Value.(int)
		if !ok || value <= 0 {
			return errs.Schema("$.edit.value", "quant_step edit requires positive int")
		}
		if edit.BlockKey == "" {
			project.Quant.Step = value
		} else {
			override := project.Overrides[edit.BlockKey]
			override.QuantStep = &value
			if project.Overrides == nil {
				project.Overrides = map[string]model.BlockOverride{}
			}
			project.Overrides[edit.BlockKey] = override
		}
	case "operator_version":
		value, ok := edit.Value.(string)
		if !ok || value == "" {
			return errs.Schema("$.edit.value", "operator_version edit requires string")
		}
		project.Rules.OperatorVersion = value
	default:
		return errs.Schema("$.edit.field", "unsupported edit field %q", edit.Field)
	}
	return nil
}

package format

import (
	"economycore/fixed"
	"economycore/model"
)

func versionRank(version string) int {
	switch version {
	case model.SchemaV1:
		return 1
	case model.SchemaV2:
		return 2
	case model.SchemaV3:
		return 3
	default:
		return 0
	}
}

// Upgrade applies the explicit v1 -> v2 -> v3 conversion chain and returns a
// deep copy plus every inserted default. Calling the same upgrade after a
// lossless downgrade/reload produces the same trace sequence.
func Upgrade(m *model.Model, target string) (*model.Model, []model.ConversionTrace, error) {
	if err := validateTarget(target); err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, model.NewError(model.ESchema, "", "nil model")
	}
	clone := m.Clone()
	clone.Normalize()
	if err := clone.Validate(); err != nil {
		return nil, nil, err
	}
	targetRank := versionRank(target)
	sourceRank := versionRank(clone.SourceVersion)
	if sourceRank == 0 {
		return nil, nil, model.NewError(model.EVer, "/version", "unknown source version "+clone.SourceVersion)
	}
	if targetRank <= sourceRank {
		return clone, nil, nil
	}
	var traces []model.ConversionTrace
	ruleVersion := clone.ConversionVersion
	if ruleVersion == "" {
		ruleVersion = model.DefaultConversionVersion
	}
	if targetRank >= 2 && !clone.Presence.WorkersMaximum {
		defaultValue := model.DefaultWorkersMaximum
		if clone.WorkersTotal > defaultValue {
			defaultValue = clone.WorkersTotal
		}
		clone.WorkersMaximum = defaultValue
		clone.Presence.WorkersMaximum = true
		traces = append(traces, model.ConversionTrace{
			Path:          "/workforce/maximum",
			OldValue:      nil,
			DefaultValue:  defaultValue,
			DefaultSource: "v2-schema-default",
			RuleVersion:   ruleVersion,
			FromVersion:   model.SchemaV1,
			ToVersion:     model.SchemaV2,
		})
	}
	if targetRank >= 3 {
		if !clone.Presence.OverflowPolicy {
			clone.OverflowPolicy = model.DefaultOverflowPolicy
			clone.Presence.OverflowPolicy = true
			traces = append(traces, model.ConversionTrace{
				Path:          "/overflowPolicy",
				OldValue:      nil,
				DefaultValue:  model.DefaultOverflowPolicy,
				DefaultSource: "v3-schema-default",
				RuleVersion:   ruleVersion,
				FromVersion:   model.SchemaV2,
				ToVersion:     model.SchemaV3,
			})
		}
		if !clone.Presence.UpgradeDiscount {
			clone.UpgradeDiscountBasisPoints = model.DefaultUpgradeDiscountBasisPoints
			clone.Presence.UpgradeDiscount = true
			traces = append(traces, model.ConversionTrace{
				Path:          "/upgradeDiscountBasisPoints",
				OldValue:      nil,
				DefaultValue:  model.DefaultUpgradeDiscountBasisPoints,
				DefaultSource: "v3-schema-default",
				RuleVersion:   ruleVersion,
				FromVersion:   model.SchemaV2,
				ToVersion:     model.SchemaV3,
			})
		}
	}
	clone.SourceVersion = target
	return clone, traces, nil
}

// Analyzer exposes loss analysis without encoding, useful for planning a
// downgrade before writing anything.
func Analyzer(m *model.Model, target string) ([]model.Loss, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	mode, err := fixed.ParseRoundMode(m.RoundMode)
	if err != nil {
		return nil, model.NewError(model.ESchema, "/rounding", err.Error())
	}
	return analyzeLosses(m, target, mode), nil
}

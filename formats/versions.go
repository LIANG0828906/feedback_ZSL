package formats

import (
	"encoding/json"

	"lipsync/domain"
	"lipsync/units"
)

func loadV1(top object) (*domain.Project, error) {
	sampleRate, frameRate, err := parseFlatFraming(top)
	if err != nil {
		return nil, err
	}
	timeBase, err := requiredString(top, "time_base", "time_base")
	if err != nil {
		return nil, err
	}
	if timeBase != domain.TimeBaseSamples {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: "time_base", Message: "v1 only accepts samples time base"}
	}
	budgetScale, err := requiredString(top, "budget_scale", "budget_scale")
	if err != nil {
		return nil, err
	}
	if budgetScale != domain.BudgetScalePermille {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: "budget_scale", Message: "v1 only accepts permille budget scale"}
	}
	rounding, err := parseRounding(top, "rounding")
	if err != nil {
		return nil, err
	}
	name, err := requiredString(top, "name", "name")
	if err != nil {
		return nil, err
	}
	seed, err := requiredInt(top, "seed", "seed")
	if err != nil {
		return nil, err
	}
	channels, truncations, err := parseChannelsV1(top, sampleRate, frameRate, rounding)
	if err != nil {
		return nil, err
	}
	order, err := parseOrder(top, channels)
	if err != nil {
		return nil, err
	}
	dialogues, dialogueTruncations, err := parseDialogues(top, "v1", sampleRate, frameRate, rounding, channels)
	if err != nil {
		return nil, err
	}
	truncations = append(truncations, dialogueTruncations...)
	recovery, fills := recoveryDefaultsV1()
	return &domain.Project{
		SchemaVersion:     "v1",
		Seed:              seed,
		Name:              name,
		SampleRate:        sampleRate,
		FrameRate:         frameRate,
		SourceTimeBase:    timeBase,
		SourceBudgetScale: budgetScale,
		Rounding:          string(rounding),
		Channels:          channels,
		ArbitrationOrder:  order,
		Recovery:          recovery,
		Dialogues:         dialogues,
		DefaultFills:      fills,
		Truncations:       truncations,
	}, nil
}

func loadV2(top object) (*domain.Project, error) {
	sampleRate, frameRate, err := parseCamelFraming(top)
	if err != nil {
		return nil, err
	}
	timeBase, err := requiredString(top, "timeBase", "timeBase")
	if err != nil {
		return nil, err
	}
	if timeBase != domain.TimeBaseMillis {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: "timeBase", Message: "v2 only accepts millis time base"}
	}
	budgetScale, err := requiredString(top, "budgetScale", "budgetScale")
	if err != nil {
		return nil, err
	}
	if budgetScale != domain.BudgetScaleBasisPoint {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: "budgetScale", Message: "v2 only accepts basis_points budget scale"}
	}
	roundingValue, err := optionalString(top, "rounding", "rounding", "floor")
	if err != nil {
		return nil, err
	}
	rounding, err := units.ParseRounding(roundingValue)
	if err != nil {
		return nil, err
	}
	name, err := requiredString(top, "name", "name")
	if err != nil {
		return nil, err
	}
	seed, err := requiredInt(top, "seed", "seed")
	if err != nil {
		return nil, err
	}
	channels, err := parseChannelsV2(top)
	if err != nil {
		return nil, err
	}
	order, err := parseOrder(top, channels)
	if err != nil {
		return nil, err
	}
	dialogues, truncations, err := parseDialogues(top, "v2", sampleRate, frameRate, rounding, channels)
	if err != nil {
		return nil, err
	}
	recovery, fills, err := parseRecoveryV2(top)
	if err != nil {
		return nil, err
	}
	return &domain.Project{
		SchemaVersion:     "v2",
		Seed:              seed,
		Name:              name,
		SampleRate:        sampleRate,
		FrameRate:         frameRate,
		SourceTimeBase:    timeBase,
		SourceBudgetScale: budgetScale,
		Rounding:          string(rounding),
		Channels:          channels,
		ArbitrationOrder:  order,
		Recovery:          recovery,
		Dialogues:         dialogues,
		DefaultFills:      fills,
		Truncations:       truncations,
	}, nil
}

func loadV3(top object) (*domain.Project, error) {
	sampleRate, frameRate, err := parseFlatFraming(top)
	if err != nil {
		return nil, err
	}
	timeBase, err := requiredString(top, "time_base", "time_base")
	if err != nil {
		return nil, err
	}
	if timeBase != domain.TimeBaseSamples {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: "time_base", Message: "v3 only accepts samples time base"}
	}
	budgetScale, err := requiredString(top, "budget_scale", "budget_scale")
	if err != nil {
		return nil, err
	}
	if budgetScale != domain.BudgetScaleBasisPoint {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: "budget_scale", Message: "v3 only accepts basis_points budget scale"}
	}
	rounding, err := parseRounding(top, "rounding")
	if err != nil {
		return nil, err
	}
	name, err := requiredString(top, "name", "name")
	if err != nil {
		return nil, err
	}
	seed, err := requiredInt(top, "seed", "seed")
	if err != nil {
		return nil, err
	}
	channels, err := parseChannelsV3(top)
	if err != nil {
		return nil, err
	}
	order, err := parseOrder(top, channels)
	if err != nil {
		return nil, err
	}
	dialogues, truncations, err := parseDialogues(top, "v3", sampleRate, frameRate, rounding, channels)
	if err != nil {
		return nil, err
	}
	recovery, err := parseRecoveryV3(top)
	if err != nil {
		return nil, err
	}
	return &domain.Project{
		SchemaVersion:     "v3",
		Seed:              seed,
		Name:              name,
		SampleRate:        sampleRate,
		FrameRate:         frameRate,
		SourceTimeBase:    timeBase,
		SourceBudgetScale: budgetScale,
		Rounding:          string(rounding),
		Channels:          channels,
		ArbitrationOrder:  order,
		Recovery:          recovery,
		Dialogues:         dialogues,
		Truncations:       truncations,
	}, nil
}

func parseFlatFraming(top object) (int64, int64, error) {
	sampleRate, err := requiredInt(top, "sample_rate", "sample_rate")
	if err != nil {
		return 0, 0, err
	}
	frameRate, err := requiredInt(top, "frame_rate", "frame_rate")
	if err != nil {
		return 0, 0, err
	}
	if err := requirePositive(sampleRate, "sample_rate", "", false); err != nil {
		return 0, 0, err
	}
	if err := requirePositive(frameRate, "frame_rate", "", false); err != nil {
		return 0, 0, err
	}
	return sampleRate, frameRate, nil
}

// DecodeAndLoad returns a decode error rather than a schema error for malformed
// JSON. It is used by the diagnostic fault path.
func DecodeAndLoad(data []byte) (*domain.Project, error) {
	if !json.Valid(data) {
		return nil, &domain.Error{Code: domain.CodeDecode, Path: "$", Message: "project JSON is malformed"}
	}
	project, err := Load(data)
	if err != nil {
		if err.Error() == "" {
			return nil, err
		}
		return nil, err
	}
	return project, nil
}

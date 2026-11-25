package channelpipe

import "encoding/json"

type v3Envelope struct {
	Version *int   `json:"version"`
	ID      string `json:"id"`
	Seed    int64  `json:"seed"`
	Plans   []Plan `json:"plans"`
}

func loadV3(data []byte, versionField string) (*LoadResult, error) {
	var raw v3Envelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, WrapError(CodeSchema, err, "$", "v3 plan cannot be parsed")
	}
	if raw.Version == nil {
		return nil, FieldError(CodeSchema, versionField, "required field is missing")
	}
	if raw.Plans == nil {
		return nil, FieldError(CodeSchema, "plans", "required field is missing")
	}
	if len(raw.Plans) == 0 {
		return nil, FieldError(CodeSchema, "plans", "at least one plan is required")
	}
	set := PlanSet{ID: raw.ID, Seed: raw.Seed, Plans: raw.Plans}
	if set.Seed == 0 {
		set.Seed = DefaultSeed
	}
	if set.ID == "" {
		set.ID = "v3-set"
	}
	for planIndex := range set.Plans {
		if set.Plans[planIndex].Seed == 0 {
			set.Plans[planIndex].Seed = set.Seed
		}
		if err := validatePlanAt(&set.Plans[planIndex], "plans["+itoa(planIndex)+"]."); err != nil {
			return nil, err
		}
	}
	return &LoadResult{Set: set, SourceVersion: 3, VersionField: versionField}, nil
}

func ValidateSet(set *PlanSet) error {
	if set == nil {
		return FieldError(CodeSchema, "$", "plan set is nil")
	}
	for planIndex := range set.Plans {
		prefix := "plans[" + itoa(planIndex) + "]."
		if err := validatePlanAt(&set.Plans[planIndex], prefix); err != nil {
			return err
		}
	}
	return nil
}

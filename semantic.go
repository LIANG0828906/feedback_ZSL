package channelpipe

type semanticStage struct {
	Index          int        `json:"index"`
	ID             string     `json:"id"`
	TargetDepth    int        `json:"target_depth"`
	Rounding       Rounding   `json:"rounding"`
	TargetChannels []Channel  `json:"target_channels"`
	Swizzle        []int      `json:"swizzle"`
	Fills          []FillSpec `json:"fills"`
	DropPolicy     DropPolicy `json:"drop_policy"`
	AlphaMode      AlphaMode  `json:"alpha_mode"`
}

type semanticPlan struct {
	ID           string          `json:"id"`
	Seed         int64           `json:"seed"`
	Width        int             `json:"width"`
	Height       int             `json:"height"`
	SourceDepth  int             `json:"source_depth"`
	SourceLayout []Channel       `json:"source_layout"`
	Stages       []semanticStage `json:"stages"`
}

func SemanticFingerprintPlan(plan Plan) (string, error) {
	return Fingerprint(toSemanticPlan(plan))
}

func SemanticEquivalentPlan(a, b Plan) (bool, string, error) {
	hashA, err := SemanticFingerprintPlan(a)
	if err != nil {
		return false, "", err
	}
	hashB, err := SemanticFingerprintPlan(b)
	if err != nil {
		return false, "", err
	}
	if hashA == hashB {
		return true, "", nil
	}
	diff := firstSemanticDiff(a, b)
	return false, diff, nil
}

func toSemanticPlan(plan Plan) semanticPlan {
	result := semanticPlan{
		ID:           plan.ID,
		Seed:         plan.Seed,
		Width:        plan.Width,
		Height:       plan.Height,
		SourceDepth:  plan.SourceDepth,
		SourceLayout: append([]Channel(nil), plan.SourceLayout...),
		Stages:       make([]semanticStage, len(plan.Stages)),
	}
	for i, stage := range plan.Stages {
		fills := make([]FillSpec, len(stage.TargetChannels))
		for targetIndex := range stage.TargetChannels {
			if targetIndex < len(stage.Fills) {
				fill := stage.Fills[targetIndex]
				fill.Value = normalizedFillValue(fill, stage.TargetDepth)
				if fill.Kind == FillZero || fill.Kind == FillOpaque {
					fill.Kind = FillConstant
				}
				fills[targetIndex] = fill
			}
		}
		result.Stages[i] = semanticStage{
			Index:          stage.Index,
			ID:             stage.ID,
			TargetDepth:    stage.TargetDepth,
			Rounding:       NormalizeRounding(stage.Rounding),
			TargetChannels: append([]Channel(nil), stage.TargetChannels...),
			Swizzle:        append([]int(nil), stage.Swizzle...),
			Fills:          fills,
			DropPolicy:     stage.DropPolicy,
			AlphaMode:      stage.AlphaMode,
		}
	}
	return result
}

func firstSemanticDiff(a, b Plan) string {
	if a.ID != b.ID {
		return "id"
	}
	if a.Width != b.Width || a.Height != b.Height {
		return "dimensions"
	}
	if a.SourceDepth != b.SourceDepth {
		return "source_depth"
	}
	if len(a.SourceLayout) != len(b.SourceLayout) {
		return "source_layout"
	}
	for i := range a.SourceLayout {
		if a.SourceLayout[i] != b.SourceLayout[i] {
			return "source_layout[" + itoa(i) + "]"
		}
	}
	if len(a.Stages) != len(b.Stages) {
		return "stages.length"
	}
	for i := range a.Stages {
		left := toSemanticPlan(Plan{Stages: []Stage{a.Stages[i]}}).Stages[0]
		right := toSemanticPlan(Plan{Stages: []Stage{b.Stages[i]}}).Stages[0]
		if left.ID != right.ID {
			return "stages[" + itoa(i) + "].id"
		}
		if left.TargetDepth != right.TargetDepth {
			return "stages[" + itoa(i) + "].target_depth"
		}
		if left.Rounding != right.Rounding {
			return "stages[" + itoa(i) + "].rounding"
		}
		if !channelsEqual(left.TargetChannels, right.TargetChannels) {
			return "stages[" + itoa(i) + "].target_channels"
		}
		if !intsEqual(left.Swizzle, right.Swizzle) {
			return "stages[" + itoa(i) + "].swizzle"
		}
		if len(left.Fills) != len(right.Fills) {
			return "stages[" + itoa(i) + "].fills"
		}
		for j := range left.Fills {
			if left.Fills[j] != right.Fills[j] {
				return "stages[" + itoa(i) + "].fills[" + itoa(j) + "]"
			}
		}
		if left.DropPolicy != right.DropPolicy {
			return "stages[" + itoa(i) + "].drop_policy"
		}
		if left.AlphaMode != right.AlphaMode {
			return "stages[" + itoa(i) + "].alpha_mode"
		}
	}
	return "semantic-fingerprint"
}

func channelsEqual(a, b []Channel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

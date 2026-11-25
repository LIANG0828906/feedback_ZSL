package model

type ExemptionHit struct {
	ExemptionID   string `json:"exemption_id"`
	Source        string `json:"source"`
	InstructionID string `json:"instruction_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type EffectDecision struct {
	Dimension    string   `json:"dimension"`
	WinnerID     string   `json:"winner_id,omitempty"`
	CandidateIDs []string `json:"candidate_ids"`
	Rule         string   `json:"rule"`
}

type Adjudication struct {
	RankedInstructionIDs []string         `json:"ranked_instruction_ids"`
	Decisions            []EffectDecision `json:"decisions"`
	Rule                 string           `json:"rule"`
}

type HopResult struct {
	Index                 int            `json:"index"`
	EdgeID                string         `json:"edge_id"`
	From                  string         `json:"from"`
	To                    string         `json:"to"`
	StartMS               int64          `json:"start_ms"`
	EndMS                 int64          `json:"end_ms"`
	BaseMS                int64          `json:"base_ms"`
	NaturalMS             int64          `json:"natural_ms"`
	AppliedDelayMS        int64          `json:"applied_delay_ms"`
	AppliedRepeatMS       int64          `json:"applied_repeat_ms"`
	TimeoutMS             int64          `json:"timeout_ms,omitempty"`
	CorrectionMS          int64          `json:"correction_ms"`
	Status                string         `json:"status"`
	Code                  string         `json:"code,omitempty"`
	Source                string         `json:"source,omitempty"`
	MatchedInstructionIDs []string       `json:"matched_instruction_ids"`
	AppliedInstructionIDs []string       `json:"applied_instruction_ids"`
	ExemptionHits         []ExemptionHit `json:"exemption_hits,omitempty"`
	Adjudication          Adjudication   `json:"adjudication"`
	ReorderDepth          int            `json:"reorder_depth,omitempty"`
	RepeatCount           int            `json:"repeat_count,omitempty"`
}

type RequestResult struct {
	RequestID               string      `json:"request_id"`
	Sequence                int         `json:"sequence"`
	Tick                    int         `json:"tick"`
	Status                  string      `json:"status"`
	Code                    string      `json:"code,omitempty"`
	TotalMS                 int64       `json:"total_ms"`
	BudgetMS                int64       `json:"budget_ms"`
	CompletedHops           int         `json:"completed_hops"`
	Hops                    []HopResult `json:"hops"`
	HitInstructionIDs       []string    `json:"hit_instruction_ids"`
	AppliedInstructionIDs   []string    `json:"applied_instruction_ids"`
	ExemptionInstructionIDs []string    `json:"exemption_instruction_ids"`
}

type Deviation struct {
	InstructionID string `json:"instruction_id,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Field         string `json:"field"`
	AmountMS      int64  `json:"amount_ms,omitempty"`
	AmountCount   int    `json:"amount_count,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

type Verdict struct {
	RequestID               string      `json:"request_id"`
	Classification          string      `json:"classification"`
	Consistent              bool        `json:"consistent"`
	ExpectedStatus          string      `json:"expected_status"`
	ActualStatus            string      `json:"actual_status"`
	ExpectedCode            string      `json:"expected_code,omitempty"`
	ActualCode              string      `json:"actual_code,omitempty"`
	DeviationMS             int64       `json:"deviation_ms"`
	DeviationInstructionIDs []string    `json:"deviation_instruction_ids,omitempty"`
	Deviations              []Deviation `json:"deviations,omitempty"`
	Reason                  string      `json:"reason"`
}

type QueueItem struct {
	InstructionID       string   `json:"instruction_id"`
	Sequence            int      `json:"sequence"`
	Position            int      `json:"position"`
	SinceTick           int      `json:"since_tick"`
	ConstraintID        string   `json:"constraint_id"`
	ViolatedConstraints []string `json:"violated_constraints"`
	Reason              string   `json:"reason"`
}

type TickSchedule struct {
	Tick      int         `json:"tick"`
	Submitted []string    `json:"submitted"`
	Admitted  []string    `json:"admitted"`
	Queued    []QueueItem `json:"queued"`
	Active    []string    `json:"active"`
	Released  []string    `json:"released"`
}

type Report struct {
	ProjectID      string          `json:"project_id"`
	ProjectVersion string          `json:"project_version"`
	Seed           int64           `json:"seed"`
	Ticks          []int           `json:"ticks,omitempty"`
	Schedules      []TickSchedule  `json:"schedules,omitempty"`
	Results        []RequestResult `json:"results"`
	Verdicts       []Verdict       `json:"verdicts,omitempty"`
}

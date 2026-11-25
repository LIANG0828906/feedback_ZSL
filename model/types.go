package model

const (
	SchemaV1 = "chaosdiag.io/v1"
	SchemaV2 = "chaosdiag.io/v2"
	SchemaV3 = "chaosdiag.io/v3"

	KindDelay     = "delay"
	KindTimeout   = "timeout"
	KindFailure   = "failure"
	KindDuplicate = "duplicate"
	KindReorder   = "reorder"

	TargetService = "service"
	TargetEdge    = "edge"

	StatusCompleted = "completed"
	StatusTimeout   = "timeout"
	StatusFailure   = "failure"

	VerdictMatch              = "MATCH"
	VerdictInjection          = "INJECTION_DEVIATION"
	VerdictServiceFailure     = "SERVICE_FAILURE"
	VerdictExpectationInvalid = "EXPECTATION_INVALID"

	MaxConcurrencyLimit = 64
)

type Project struct {
	Schema                   string        `json:"schema"`
	ProjectID                string        `json:"project_id"`
	ProjectVersion           string        `json:"project_version"`
	Seed                     int64         `json:"seed"`
	TickMS                   int64         `json:"tick_ms"`
	RequestTimeoutBudgetMS   int64         `json:"request_timeout_budget_ms"`
	AdmissionTimeoutBudgetMS int64         `json:"admission_timeout_budget_ms"`
	Limits                   Limits        `json:"limits"`
	Services                 []Service     `json:"services"`
	Edges                    []Edge        `json:"edges"`
	Selectors                []Selector    `json:"selectors,omitempty"`
	Instructions             []Instruction `json:"instructions"`
	Exemptions               []Exemption   `json:"exemptions,omitempty"`
	Requests                 []Request     `json:"requests"`
	ConstraintCatalog        []Constraint  `json:"constraint_catalog,omitempty"`
}

type Limits struct {
	GlobalConcurrent         int            `json:"global_concurrent"`
	ServiceConcurrentDefault int            `json:"service_concurrent_default"`
	ServiceConcurrent        map[string]int `json:"service_concurrent,omitempty"`
}

type Constraint struct {
	ID          string `json:"id"`
	Level       string `json:"level"`
	Description string `json:"description"`
}

type Service struct {
	Name             string `json:"name"`
	LatencyMS        int64  `json:"latency_ms"`
	SelfErrorCode    string `json:"self_error_code,omitempty"`
	SelfErrorMessage string `json:"self_error_message,omitempty"`
}

type Edge struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	LatencyMS int64  `json:"latency_ms"`
}

type Selector struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Service      string     `json:"service,omitempty"`
	EdgeID       string     `json:"edge_id,omitempty"`
	From         string     `json:"from,omitempty"`
	To           string     `json:"to,omitempty"`
	PathContains string     `json:"path_contains,omitempty"`
	TreePath     []string   `json:"tree_path,omitempty"`
	Tree         *MatchNode `json:"tree,omitempty"`
}

type MatchNode struct {
	Label    string      `json:"label"`
	Service  string      `json:"service,omitempty"`
	EdgeID   string      `json:"edge_id,omitempty"`
	From     string      `json:"from,omitempty"`
	To       string      `json:"to,omitempty"`
	Kind     string      `json:"kind,omitempty"`
	Operator string      `json:"operator,omitempty"`
	Children []MatchNode `json:"children,omitempty"`
}

type Instruction struct {
	ID              string   `json:"id"`
	Sequence        int      `json:"sequence"`
	SubmittedTick   int      `json:"submitted_tick"`
	Kind            string   `json:"kind"`
	Priority        int      `json:"priority"`
	SelectorID      string   `json:"selector_id,omitempty"`
	Selector        Selector `json:"selector"`
	StartTick       int      `json:"start_tick"`
	EndTick         int      `json:"end_tick"`
	DelayMS         int64    `json:"delay_ms,omitempty"`
	TimeoutMS       int64    `json:"timeout_ms,omitempty"`
	FailureCode     string   `json:"failure_code,omitempty"`
	RepeatCount     int      `json:"repeat_count,omitempty"`
	RepeatDelayMS   int64    `json:"repeat_delay_ms,omitempty"`
	ReorderDepth    int      `json:"reorder_depth,omitempty"`
	BudgetCostMS    int64    `json:"budget_cost_ms,omitempty"`
	SourceAmplitude string   `json:"source_amplitude,omitempty"`
	SourceWindow    string   `json:"source_window,omitempty"`
}

type Exemption struct {
	ID            string   `json:"id"`
	Source        string   `json:"source"`
	Reason        string   `json:"reason,omitempty"`
	InstructionID string   `json:"instruction_id,omitempty"`
	Selector      Selector `json:"selector"`
}

type Request struct {
	ID       string         `json:"id"`
	Sequence int            `json:"sequence"`
	Tick     int            `json:"tick"`
	Entry    string         `json:"entry"`
	Path     []string       `json:"path"`
	BudgetMS int64          `json:"budget_ms,omitempty"`
	Expected ExpectedResult `json:"expected"`
}

type ExpectedResult struct {
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	MaxTotalMS int64  `json:"max_total_ms,omitempty"`
	MaxRepeats int    `json:"max_repeats,omitempty"`
}

type Interval struct {
	StartTick int `json:"start_tick"`
	EndTick   int `json:"end_tick"`
}

func (p *Project) RequestBudget(r Request) int64 {
	if r.BudgetMS > 0 {
		return r.BudgetMS
	}
	return p.RequestTimeoutBudgetMS
}

func (p *Project) ServiceConcurrency(service string) int {
	if p.Limits.ServiceConcurrent != nil {
		if n, ok := p.Limits.ServiceConcurrent[service]; ok {
			return n
		}
	}
	return p.Limits.ServiceConcurrentDefault
}

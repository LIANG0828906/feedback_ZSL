package kernel

const DefaultDecisionBudgetMS int64 = 2

// Gate is the explicit admission/latency boundary. The fair multi-tenant
// scheduler lives in runSimulation; Gate contains the reusable deterministic
// fast-pre-deduction and budget policy.
type Gate struct {
	Mode    string
	Budgets Budgets
}

type GateRequest struct {
	TenantID      ID
	NodeID        ID
	WantBytes     int64
	UsedBytes     int64
	HeldBytes     int64
	HardCapBytes  int64
	NodeAvailable int64
	BudgetMS      int64
}

type GateDecision struct {
	Path         string `json:"path"`
	DelayMS      int64  `json:"delay_ms"`
	WithinBudget bool   `json:"within_budget"`
	PreHoldBytes int64  `json:"pre_hold_bytes"`
	GrantBytes   int64  `json:"grant_bytes"`
	Decision     string `json:"decision"`
	ReasonCode   string `json:"reason_code,omitempty"`
	ConstraintID string `json:"constraint_id,omitempty"`
}

func NewGate(mode string, budgets Budgets) *Gate {
	if mode == "" {
		mode = ModeAuto
	}
	return &Gate{Mode: mode, Budgets: budgets}
}

func (g *Gate) Latency(budgetMS int64) (path string, delayMS int64, within bool) {
	switch g.Mode {
	case PathExact:
		const exactCost = int64(3)
		return PathExact, exactCost, exactCost <= budgetMS
	case PathPreDeduct, PathParallel:
		const fastCost = int64(1)
		return PathPreDeduct, fastCost, fastCost <= budgetMS
	default:
		return latencyFor("", 0, budgetMS)
	}
}

// DecideSingle performs a conservative one-request gate decision. NodeAvailable
// must already subtract physical occupancy/holds and other tenants' protected
// guarantee remainder. The returned hold never exceeds tenant hard headroom.
func (g *Gate) DecideSingle(req GateRequest) GateDecision {
	budget := req.BudgetMS
	if budget <= 0 {
		budget = DefaultDecisionBudgetMS
	}
	path, delay, within := g.Latency(budget)
	d := GateDecision{Path: path, DelayMS: delay, WithinBudget: within}
	hardHeadroom := max64(0, req.HardCapBytes-req.UsedBytes-req.HeldBytes)
	tenantGrant := min64(req.WantBytes, hardHeadroom)
	d.PreHoldBytes = min64(tenantGrant, req.NodeAvailable)
	d.GrantBytes = d.PreHoldBytes
	switch {
	case req.WantBytes > hardHeadroom:
		d.Decision = DecisionReject
		d.ReasonCode = CodeHardCap
		d.ConstraintID = ConstraintHardCap
	case d.GrantBytes == 0:
		d.Decision = DecisionQueue
		d.ReasonCode = CodeNodeCapacity
		d.ConstraintID = ConstraintNodeCapacity
	case d.GrantBytes < req.WantBytes:
		d.Decision = DecisionQueue
		d.ReasonCode = CodeNodeCapacity
		d.ConstraintID = ConstraintNodeCapacity
	default:
		d.Decision = DecisionAdmit
	}
	return d
}

// PreDeductHardHeadroom is the invariant used by the batched scheduler before
// converting a hold into an exact ledger settlement.
func PreDeductHardHeadroom(used, held, hardCap, want int64) int64 {
	headroom := max64(0, hardCap-used-held)
	return min64(want, headroom)
}

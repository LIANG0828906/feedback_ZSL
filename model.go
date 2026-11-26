package cachebench

import "sort"

const (
	SchemaV1 = "cachebench.v1"
	SchemaV2 = "cachebench.v2"
	SchemaV3 = "cachebench.v3"

	CheckpointSchemaVersion = "checkpoint.v1"

	ScalePermille  int64 = 1000
	ScalePermyriad int64 = 10000
	PPMScale       int64 = 1_000_000
)

// Project is the schema-independent pressure engineering model. All time
// fields are normalized to integer milliseconds and all ratios to PPM.
type Project struct {
	SchemaVersion       string
	ProjectID           string
	Seed                int64
	TickMS              int64
	DurationMS          int64
	WindowMS            int64
	WeightSourceScale   int64
	CapacitySourceScale int64
	RoutingPolicy       RoutingPolicy
	StatTolerancePPM    int64
	Nodes               []Node
	Requests            []Request
	Observations        []Observation
	CriticalCases       []CriticalCase
	Overrides           []NodeOverride
}

// NodeOverride is a logical-time parameter change. Full recomputation and
// suffix incremental replay apply the same override timeline.
type NodeOverride struct {
	AtMS                 int64
	NodeID               string
	MaxInflight          int
	SetMaxInflight       bool
	WeightPermyriad      int64
	SetWeight            bool
	InflightThreshold    int
	SetInflightThreshold bool
}

type RoutingPolicy struct {
	Mode            string // weighted_priority
	HashBucketScale int64
	ZeroWeightDeny  bool
}

type Node struct {
	ID                        string
	Rack                      string
	DeclarationOrder          int
	Priority                  int
	WeightPermyriad           int64
	MaxInflight               int
	ServiceMS                 int64
	TimeoutMS                 int64
	ConstraintID              string
	CapacityLimitPPM          int64
	InflightThreshold         int
	RecoveryInflightThreshold int
	ErrorRateLimitPPM         int64
	ErrorRateRecoveryPPM      int64
	TriggerMS                 int64
	RecoveryMS                int64
	MinErrorSamples           int64
}

type Request struct {
	ID         string
	Ordinal    int
	AtMS       int64
	Candidates []string
	IsError    bool
	LatencyMS  int64
}

type Observation struct {
	WindowIndex           int
	AtMS                  int64
	Requests              int64
	Errors                int64
	TotalLatencyMS        int64
	ThroughputFixed       int64
	ErrorRateSource       int64
	LatencySourceFixed    int64
	Overloaded            bool
	OverloadMarkerPresent bool
}

type CriticalCase struct {
	ID             string
	Kind           string
	WidthBits      int
	FromValue      int64
	FromScale      int64
	ToScale        int64
	TickMS         int64
	TolerancePPM   int64
	ExpectedValue  int64
	ShouldSaturate bool
}

type Overflow struct {
	RequestID    string
	NodeID       string
	ConstraintID string
	TickMS       int64
	ReasonCode   ErrorCode
	BasisID      string
}

type RoutingDecision struct {
	RequestID          string
	TickMS             int64
	NodeID             string
	Bucket             int64
	TotalWeight        int64
	BasisID            string
	ArbitrationBasisID string
	Candidates         []string
}

type BreakerEvent struct {
	TickMS     int64
	NodeID     string
	Opened     bool
	Recovered  bool
	ReasonCode string
	BasisID    string
	RecoveryAt int64
}

type NodeTick struct {
	NodeID          string
	TickMS          int64
	Inflight        int
	Accepted        int
	Completed       int
	ErrorsCompleted int
	LatencyTotalMS  int64
	Overflow        int
	CircuitOpen     bool
	HighSinceMS     int64
	OpenSinceMS     int64
	RecoveryAtMS    int64
	ActualComputeNS int64
}

type WindowStat struct {
	WindowIndex     int
	StartMS         int64
	EndMS           int64
	Requests        int64
	Errors          int64
	TotalLatencyMS  int64
	ThroughputQ3    int64
	ErrorRatePPM    int64
	AvgLatencyQ3    int64
	WithinTolerance bool
	TolerancePPM    int64
	ConvertedValue  int64
	Saturated       bool
}

type TickResult struct {
	TickMS          int64
	Nodes           []NodeTick
	Overflow        []Overflow
	Decisions       []RoutingDecision
	BreakerEvents   []BreakerEvent
	TotalThroughput int64
	OpenCircuits    []string
}

type SimulationResult struct {
	ProjectID       string
	CheckpointVer   string
	Ticks           []TickResult
	Windows         []WindowStat
	FinalTick       int64
	NodeIDs         []string
	RecomputedCells int
	ReusedCells     int
}

func (p Project) NodeByID(id string) (Node, bool) {
	for _, n := range p.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (p Project) SortedNodes() []Node {
	out := append([]Node(nil), p.Nodes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].DeclarationOrder < out[j].DeclarationOrder
	})
	return out
}

func (r SimulationResult) TickAt(ms int64) (TickResult, bool) {
	for _, t := range r.Ticks {
		if t.TickMS == ms {
			return t, true
		}
	}
	return TickResult{}, false
}

func (r SimulationResult) NodeTick(ms int64, nodeID string) (NodeTick, bool) {
	t, ok := r.TickAt(ms)
	if !ok {
		return NodeTick{}, false
	}
	for _, n := range t.Nodes {
		if n.NodeID == nodeID {
			return n, true
		}
	}
	return NodeTick{}, false
}

// CanonicalFingerprint returns a deterministic comparable projection. Maps are
// intentionally avoided so tests can compare three loaded schema versions.
func (p Project) CanonicalFingerprint() CanonicalProject {
	cp := CanonicalProject{ProjectID: p.ProjectID, TickMS: p.TickMS, DurationMS: p.DurationMS, WindowMS: p.WindowMS}
	for _, n := range p.Nodes {
		cp.Nodes = append(cp.Nodes, CanonicalNode{ID: n.ID, Rack: n.Rack, Order: n.DeclarationOrder, Priority: n.Priority, WeightPermyriad: n.WeightPermyriad, MaxInflight: n.MaxInflight, ServiceMS: n.ServiceMS})
	}
	for _, r := range p.Requests {
		cp.Requests = append(cp.Requests, CanonicalRequest{ID: r.ID, AtMS: r.AtMS, Candidates: append([]string(nil), r.Candidates...), IsError: r.IsError})
	}
	for _, o := range p.Observations {
		cp.Observations = append(cp.Observations, CanonicalObservation{WindowIndex: o.WindowIndex, AtMS: o.AtMS, Requests: o.Requests, Errors: o.Errors, TotalLatencyMS: o.TotalLatencyMS})
	}
	return cp
}

type CanonicalProject struct {
	ProjectID    string
	TickMS       int64
	DurationMS   int64
	WindowMS     int64
	Nodes        []CanonicalNode
	Requests     []CanonicalRequest
	Observations []CanonicalObservation
}

type CanonicalNode struct {
	ID              string
	Rack            string
	Order           int
	Priority        int
	WeightPermyriad int64
	MaxInflight     int
	ServiceMS       int64
}

type CanonicalRequest struct {
	ID         string
	AtMS       int64
	Candidates []string
	IsError    bool
}

type CanonicalObservation struct {
	WindowIndex    int
	AtMS           int64
	Requests       int64
	Errors         int64
	TotalLatencyMS int64
}

package cachebench

import "fmt"

type Budget struct {
	Workers         int
	BufferPerWorker int
}

func ValidateBudget(b Budget) error {
	if b.Workers <= 0 || b.Workers > 4096 {
		return NewError(CodeWorkerBudget, "workers must be in [1,4096], got %d", b.Workers).
			WithField("workers").
			WithContext("value", fmt.Sprint(b.Workers))
	}
	if b.BufferPerWorker <= 0 || b.BufferPerWorker > 1_000_000 {
		return NewError(CodeBufferBudget, "buffer per worker must be in [1,1000000], got %d", b.BufferPerWorker).
			WithField("bufferPerWorker").
			WithContext("value", fmt.Sprint(b.BufferPerWorker))
	}
	return nil
}

// ValidateProject validates a normalized project. Validation takes a value
// receiver and never mutates the caller's source engineering data.
func ValidateProject(p Project) error {
	if p.Seed == 0 {
		p.Seed = DefaultSeed
	}
	if p.TickMS <= 0 {
		return NewError(CodeSchemaField, "tickMS must be positive").WithField("tickMS")
	}
	if p.DurationMS < 0 || p.DurationMS > 10_000_000 {
		return NewError(CodeSchemaField, "durationMS out of supported range").WithField("durationMS")
	}
	if p.WindowMS <= 0 {
		return NewError(CodeSchemaField, "windowMS must be positive").WithField("windowMS")
	}
	seen := map[string]bool{}
	var totalWeight int64
	for i, n := range p.Nodes {
		if n.ID == "" {
			return NewError(CodeSchemaField, "node id is empty").WithField(fmt.Sprintf("nodes[%d].id", i))
		}
		if seen[n.ID] {
			return NewError(CodeSchemaField, "duplicate node id").WithNode(n.ID).WithField("nodes.id")
		}
		seen[n.ID] = true
		if n.MaxInflight < 0 {
			return NewError(CodeNegativeCapacity, "max inflight/capacity is negative: %d", n.MaxInflight).
				WithField("max_inflight").WithNode(n.ID).WithConstraint(n.ConstraintID)
		}
		if n.ServiceMS < 0 || n.TimeoutMS < 0 {
			return NewError(CodeNegativeCapacity, "service/timeout is negative").
				WithField("service_ms").WithNode(n.ID)
		}
		if n.WeightPermyriad < 0 || n.WeightPermyriad > ScalePermyriad {
			return NewError(CodeWeightScale, "normalized weight must be [0,10000], got %d", n.WeightPermyriad).
				WithField("weight").WithNode(n.ID).
				WithContext("normalized", fmt.Sprint(n.WeightPermyriad))
		}
		totalWeight += n.WeightPermyriad
		if n.InflightThreshold < 0 || n.RecoveryInflightThreshold < 0 {
			return NewError(CodeNegativeCapacity, "breaker inflight threshold is negative").
				WithField("breaker_threshold").WithNode(n.ID)
		}
		if n.RecoveryInflightThreshold > n.InflightThreshold {
			return NewError(CodeBreakerOrder, "recovery inflight threshold %d is above open threshold %d", n.RecoveryInflightThreshold, n.InflightThreshold).
				WithField("recovery_inflight_threshold").WithNode(n.ID).
				WithContext("open", fmt.Sprint(n.InflightThreshold)).
				WithContext("recovery", fmt.Sprint(n.RecoveryInflightThreshold))
		}
		if n.ErrorRateRecoveryPPM > n.ErrorRateLimitPPM {
			return NewError(CodeBreakerOrder, "error recovery limit exceeds open limit").
				WithField("error_rate_recovery_ppm").WithNode(n.ID).
				WithContext("open_ppm", fmt.Sprint(n.ErrorRateLimitPPM)).
				WithContext("recovery_ppm", fmt.Sprint(n.ErrorRateRecoveryPPM))
		}
		if n.ConstraintID == "" {
			return NewError(CodeSchemaField, "constraint id missing").WithNode(n.ID)
		}
	}
	for _, o := range p.Overrides {
		n, ok := p.NodeByID(o.NodeID)
		if !ok {
			return NewError(CodeNodeNotFound, "override points to missing node").
				WithNode(o.NodeID).WithField("overrides.node_id").WithTick(o.AtMS)
		}
		if o.SetMaxInflight && o.MaxInflight < 0 {
			return NewError(CodeNegativeCapacity, "override max inflight is negative: %d", o.MaxInflight).
				WithNode(o.NodeID).WithTick(o.AtMS).WithField("max_inflight")
		}
		if o.SetWeight && (o.WeightPermyriad < 0 || o.WeightPermyriad > ScalePermyriad) {
			return NewError(CodeWeightScale, "override weight out of range: %d", o.WeightPermyriad).
				WithNode(o.NodeID).WithTick(o.AtMS).WithField("weight")
		}
		if o.SetInflightThreshold {
			if o.InflightThreshold < 0 {
				return NewError(CodeNegativeCapacity, "override threshold is negative").
					WithNode(o.NodeID).WithTick(o.AtMS).WithField("inflight_threshold")
			}
			if n.RecoveryInflightThreshold > o.InflightThreshold {
				return NewError(CodeBreakerOrder, "override open threshold %d is below recovery threshold %d", o.InflightThreshold, n.RecoveryInflightThreshold).
					WithNode(o.NodeID).WithTick(o.AtMS).WithField("inflight_threshold")
			}
		}
	}
	if totalWeight <= 0 {
		return NewError(CodeWeightScale, "at least one positive node weight is required").WithField("nodes.weight")
	}
	for _, r := range p.Requests {
		for _, id := range r.Candidates {
			if !seen[id] {
				return NewError(CodeNodeNotFound, "routing allocation points to missing node %q", id).
					WithField("candidates").
					WithNode(id).
					WithContext("request_id", r.ID)
			}
		}
	}
	return nil
}

// Injection is a named fault used by the exception-injection test/demo path.
type Injection string

const (
	InjectNegativeCapacity Injection = "negative_capacity"
	InjectWeightOutOfRange Injection = "weight_out_of_range"
	InjectBreakerOrder     Injection = "breaker_threshold_reversed"
	InjectMissingNode      Injection = "missing_node_allocation"
	InjectWorkerBudget     Injection = "worker_budget_out_of_range"
	InjectBufferBudget     Injection = "buffer_budget_out_of_range"
	InjectTruncatedCP      Injection = "truncated_checkpoint"
	InjectVersionMismatch  Injection = "version_mismatch_snapshot"
)

// InjectProjectFault returns an invalid copy. The input project is deliberately
// passed by value: slices share backing arrays until a write, and every injected
// write replaces the affected slice, preserving the source engineering data.
func InjectProjectFault(p Project, kind Injection) (Project, error) {
	q := p
	q.Nodes = append([]Node(nil), p.Nodes...)
	q.Requests = append([]Request(nil), p.Requests...)
	switch kind {
	case InjectNegativeCapacity:
		if len(q.Nodes) == 0 {
			return q, NewError(CodeInvalidArgument, "project has no nodes")
		}
		q.Nodes[0].MaxInflight = -1
		return q, ValidateProject(q)
	case InjectWeightOutOfRange:
		if len(q.Nodes) == 0 {
			return q, NewError(CodeInvalidArgument, "project has no nodes")
		}
		q.Nodes[0].WeightPermyriad = ScalePermyriad + 1
		return q, ValidateProject(q)
	case InjectBreakerOrder:
		if len(q.Nodes) == 0 {
			return q, NewError(CodeInvalidArgument, "project has no nodes")
		}
		q.Nodes[0].RecoveryInflightThreshold = q.Nodes[0].InflightThreshold + 1
		return q, ValidateProject(q)
	case InjectMissingNode:
		if len(q.Requests) == 0 {
			q.Requests = []Request{{ID: "req-injected-missing", AtMS: 0, Candidates: []string{"node-missing"}}}
		} else {
			q.Requests[0].Candidates = []string{"node-missing"}
		}
		return q, ValidateProject(q)
	case InjectWorkerBudget:
		return q, ValidateBudget(Budget{Workers: 0, BufferPerWorker: 16})
	case InjectBufferBudget:
		return q, ValidateBudget(Budget{Workers: 2, BufferPerWorker: -1})
	default:
		return q, NewError(CodeInvalidArgument, "unknown injection %q", kind)
	}
}

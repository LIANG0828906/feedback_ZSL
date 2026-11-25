package hckernel

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

const (
	SchemaV1 = "hcs/v1"
	SchemaV2 = "hcs/v2"
	SchemaV3 = "hcs/v3"
)

type LoadResult struct {
	Project      *Project `json:"project"`
	SourceSchema string   `json:"source_schema"`
	Conversions  []string `json:"conversions"`
}

// LoadDocument detects one of the three supported wire schemas and maps it to
// the canonical model. It does not retain or mutate the input byte slice.
func LoadDocument(data []byte) (*LoadResult, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, newError(ErrSchemaDecodeFailed, "invalid JSON document", withCause(err))
	}
	version := detectSchemaVersion(head)
	switch version {
	case SchemaV1:
		return loadV1(data)
	case SchemaV2:
		return loadV2(data)
	case SchemaV3:
		return loadV3(data)
	default:
		return nil, newError(ErrUnknownSchemaVersion, "unrecognized schema version: "+version, withPath("schema"))
	}
}

func detectSchemaVersion(head map[string]json.RawMessage) string {
	for _, key := range []string{"schema", "schemaVersion", "schema_version"} {
		raw, ok := head[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil && value != "" {
			return value
		}
	}
	return "missing"
}

func canonicalize(p *Project, conversions []string) (*LoadResult, error) {
	sort.Slice(p.Racks, func(i, j int) bool { return p.Racks[i].ID < p.Racks[j].ID })
	sort.Slice(p.Instances, func(i, j int) bool { return p.Instances[i].ID < p.Instances[j].ID })
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return &LoadResult{Project: p, Conversions: conversions}, nil
}

func secondsToMs(value float64, path string) (int64, error) {
	scaled := value * 1000
	rounded := int64(math.Round(scaled))
	if math.Abs(scaled-float64(rounded)) > 1e-6 {
		return 0, newError(ErrSchemaPayloadInvalid, "second value must have integer millisecond precision", withPath(path))
	}
	return rounded, nil
}

func normalizeState(value string) HealthState {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "starting", "initial", "init":
		return StateInitial
	case "healthy", "ok", "up":
		return StateHealthy
	case "unhealthy", "critical", "down":
		return StateUnhealthy
	case "half_open", "half-open", "halfopen":
		return StateHalfOpen
	default:
		return HealthState(value)
	}
}

func boolValue(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

type v1Document struct {
	Schema         string `json:"schema"`
	ProjectName    string `json:"project_name"`
	ProjectVersion string `json:"project_version"`
	Seed           int64  `json:"seed"`
	Rounds         int    `json:"rounds"`
	TickMs         int64  `json:"tick_ms"`
	RoundTicks     int64  `json:"round_ticks"`
	MaxDeferTicks  int64  `json:"max_defer_ticks"`
	Budget         struct {
		GlobalSlots int `json:"global_slots"`
		RackSlots   []struct {
			RackID string `json:"rack_id"`
			Slots  int    `json:"slots"`
		} `json:"rack_slots"`
	} `json:"budget"`
	WorkerBudget   int    `json:"worker_budget"`
	BufferCapacity int    `json:"buffer_capacity"`
	SlotPriority   string `json:"slot_priority"`
	CountPolicy    struct {
		SuccessAction        string `json:"success_threshold_action"`
		FailureAction        string `json:"failure_threshold_action"`
		HalfOpenSuccessLimit int    `json:"half_open_success_limit"`
		HalfOpenFailureLimit int    `json:"half_open_failure_limit"`
		HalfOpenDelayTicks   int64  `json:"half_open_delay_ticks"`
	} `json:"count_policy"`
	Targets []struct {
		ID               string `json:"id"`
		Node             string `json:"node"`
		Rack             string `json:"rack"`
		Protocol         string `json:"protocol"`
		Address          string `json:"address"`
		IntervalTicks    int64  `json:"interval_ticks"`
		TimeoutTicks     int64  `json:"timeout_ticks"`
		BackoffTicks     int64  `json:"backoff_ticks"`
		Retries          int    `json:"retries"`
		SuccessThreshold int    `json:"success_threshold"`
		FailureThreshold int    `json:"failure_threshold"`
		InitialState     string `json:"initial_state"`
		WeightPermille   int64  `json:"weight_permille"`
		ActiveProbe      *bool  `json:"active_probe"`
	} `json:"targets"`
}

func loadV1(data []byte) (*LoadResult, error) {
	var doc v1Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, newError(ErrSchemaDecodeFailed, "v1 document decode failed", withCause(err))
	}
	if doc.TickMs <= 0 {
		return nil, newError(ErrIntervalInvalid, "v1 tick_ms must be positive", withPath("tick_ms"))
	}
	conversions := []string{
		"v1 tick durations multiplied by tick_ms",
		"v1 absolute consecutive thresholds used directly",
		"v1 flat targets mapped to canonical instances",
		"v1 weight_permille multiplied by 1000 to weight_ppm",
	}
	racks := make([]Rack, 0, len(doc.Budget.RackSlots))
	for _, r := range doc.Budget.RackSlots {
		racks = append(racks, Rack{ID: r.RackID, Concurrency: r.Slots})
	}
	p := &Project{
		ProjectID:         doc.ProjectName,
		ProjectVersion:    doc.ProjectVersion,
		Seed:              doc.Seed,
		Rounds:            doc.Rounds,
		StepMs:            doc.RoundTicks * doc.TickMs,
		MaxDeferMs:        doc.MaxDeferTicks * doc.TickMs,
		GlobalConcurrency: doc.Budget.GlobalSlots,
		Racks:             racks,
		Workers:           doc.WorkerBudget,
		BufferCapacity:    doc.BufferCapacity,
		SlotPriority:      SlotPriority(doc.SlotPriority),
		CountPolicy: CountPolicy{
			SuccessReachedAction: ThresholdAction(doc.CountPolicy.SuccessAction),
			FailureReachedAction: ThresholdAction(doc.CountPolicy.FailureAction),
			ResetOppositeStreak:  true,
			HalfOpenSuccessLimit: doc.CountPolicy.HalfOpenSuccessLimit,
			HalfOpenFailureLimit: doc.CountPolicy.HalfOpenFailureLimit,
			HalfOpenDelayMs:      doc.CountPolicy.HalfOpenDelayTicks * doc.TickMs,
		},
	}
	for _, t := range doc.Targets {
		p.Instances = append(p.Instances, Instance{
			ID:               t.ID,
			NodeID:           t.Node,
			RackID:           t.Rack,
			Protocol:         strings.ToUpper(t.Protocol),
			Address:          t.Address,
			IntervalMs:       t.IntervalTicks * doc.TickMs,
			TimeoutMs:        t.TimeoutTicks * doc.TickMs,
			BackoffMs:        t.BackoffTicks * doc.TickMs,
			RetryLimit:       t.Retries,
			SuccessThreshold: t.SuccessThreshold,
			FailureThreshold: t.FailureThreshold,
			InitialState:     normalizeState(t.InitialState),
			WeightPPM:        t.WeightPermille * 1000,
			ActiveProbe:      boolValue(t.ActiveProbe, true),
		})
	}
	res, err := canonicalize(p, conversions)
	if err != nil {
		return nil, err
	}
	res.SourceSchema = SchemaV1
	res.Project.SourceSchema = SchemaV1
	return res, nil
}

type v2Document struct {
	SchemaVersion string `json:"schemaVersion"`
	Metadata      struct {
		ProjectID      string `json:"projectId"`
		ProjectVersion string `json:"projectVersion"`
		Seed           int64  `json:"seed"`
	} `json:"metadata"`
	Rounds int `json:"rounds"`
	Timing struct {
		StepMs     int64 `json:"stepMs"`
		MaxDeferMs int64 `json:"maxDeferMs"`
	} `json:"timing"`
	Concurrency struct {
		GlobalPermits int            `json:"globalPermits"`
		RackPermits   map[string]int `json:"rackPermits"`
	} `json:"concurrency"`
	WorkerCount  int    `json:"workerCount"`
	ProbeBuffer  int    `json:"probeBuffer"`
	SlotPriority string `json:"slotPriority"`
	CountPolicy  struct {
		OnSuccessThreshold   string `json:"onSuccessThreshold"`
		OnFailureThreshold   string `json:"onFailureThreshold"`
		HalfOpenSuccessLimit int    `json:"halfOpenSuccessLimit"`
		HalfOpenFailureLimit int    `json:"halfOpenFailureLimit"`
		HalfOpenDelayMs      int64  `json:"halfOpenDelayMs"`
	} `json:"countPolicy"`
	Instances []struct {
		InstanceID           string `json:"instanceId"`
		NodeName             string `json:"nodeName"`
		RackID               string `json:"rackId"`
		ProbeType            string `json:"probeType"`
		Address              string `json:"address"`
		IntervalMs           int64  `json:"intervalMs"`
		TimeoutMs            int64  `json:"timeoutMs"`
		BackoffMs            int64  `json:"backoffMs"`
		RetryLimit           int    `json:"retryLimit"`
		RemainingSuccesses   int    `json:"remainingSuccesses"`
		RemainingFailures    int    `json:"remainingFailures"`
		StateAtStart         string `json:"stateAtStart"`
		WeightPerTenThousand int64  `json:"weightPerTenThousand"`
		ActiveProbe          *bool  `json:"activeProbe"`
	} `json:"instances"`
}

func loadV2(data []byte) (*LoadResult, error) {
	var doc v2Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, newError(ErrSchemaDecodeFailed, "v2 document decode failed", withCause(err))
	}
	conversions := []string{
		"v2 millisecond durations used directly",
		"v2 remaining-to-decision counts become absolute thresholds at zero initial streak",
		"v2 flat instances mapped to canonical model",
		"v2 weight_per_ten_thousand multiplied by 100 to weight_ppm",
	}
	rackIDs := make([]string, 0, len(doc.Concurrency.RackPermits))
	for id := range doc.Concurrency.RackPermits {
		rackIDs = append(rackIDs, id)
	}
	sort.Strings(rackIDs)
	racks := make([]Rack, 0, len(rackIDs))
	for _, id := range rackIDs {
		racks = append(racks, Rack{ID: id, Concurrency: doc.Concurrency.RackPermits[id]})
	}
	p := &Project{
		ProjectID:         doc.Metadata.ProjectID,
		ProjectVersion:    doc.Metadata.ProjectVersion,
		Seed:              doc.Metadata.Seed,
		Rounds:            doc.Rounds,
		StepMs:            doc.Timing.StepMs,
		MaxDeferMs:        doc.Timing.MaxDeferMs,
		GlobalConcurrency: doc.Concurrency.GlobalPermits,
		Racks:             racks,
		Workers:           doc.WorkerCount,
		BufferCapacity:    doc.ProbeBuffer,
		SlotPriority:      SlotPriority(doc.SlotPriority),
		CountPolicy: CountPolicy{
			SuccessReachedAction: ThresholdAction(doc.CountPolicy.OnSuccessThreshold),
			FailureReachedAction: ThresholdAction(doc.CountPolicy.OnFailureThreshold),
			ResetOppositeStreak:  true,
			HalfOpenSuccessLimit: doc.CountPolicy.HalfOpenSuccessLimit,
			HalfOpenFailureLimit: doc.CountPolicy.HalfOpenFailureLimit,
			HalfOpenDelayMs:      doc.CountPolicy.HalfOpenDelayMs,
		},
	}
	for _, in := range doc.Instances {
		p.Instances = append(p.Instances, Instance{
			ID:               in.InstanceID,
			NodeID:           in.NodeName,
			RackID:           in.RackID,
			Protocol:         strings.ToUpper(in.ProbeType),
			Address:          in.Address,
			IntervalMs:       in.IntervalMs,
			TimeoutMs:        in.TimeoutMs,
			BackoffMs:        in.BackoffMs,
			RetryLimit:       in.RetryLimit,
			SuccessThreshold: in.RemainingSuccesses,
			FailureThreshold: in.RemainingFailures,
			InitialState:     normalizeState(in.StateAtStart),
			WeightPPM:        in.WeightPerTenThousand * 100,
			ActiveProbe:      boolValue(in.ActiveProbe, true),
		})
	}
	res, err := canonicalize(p, conversions)
	if err != nil {
		return nil, err
	}
	res.SourceSchema = SchemaV2
	res.Project.SourceSchema = SchemaV2
	return res, nil
}

type v3Document struct {
	SchemaVersion string `json:"schema_version"`
	Project       struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Seed    int64  `json:"seed"`
		Rounds  int    `json:"rounds"`
	} `json:"project"`
	LogicClock struct {
		RoundSeconds    float64 `json:"round_seconds"`
		MaxDeferSeconds float64 `json:"max_defer_seconds"`
	} `json:"logic_clock"`
	BudgetPolicy struct {
		TotalPermits int `json:"total_permits"`
		RackPolicies []struct {
			Name    string `json:"name"`
			Permits int    `json:"permits"`
		} `json:"rack_policies"`
	} `json:"budget_policy"`
	WorkerPolicy struct {
		Threads      int    `json:"threads"`
		ProbeBuffer  int    `json:"probe_buffer"`
		SlotPriority string `json:"slot_priority"`
	} `json:"worker_policy"`
	CountingRules struct {
		OnSuccessLimit string  `json:"on_success_limit"`
		OnFailureLimit string  `json:"on_failure_limit"`
		HalfOpenOK     int     `json:"half_open_ok"`
		HalfOpenBad    int     `json:"half_open_bad"`
		HalfOpenWaitS  float64 `json:"half_open_wait_seconds"`
	} `json:"counting_rules"`
	Topology []struct {
		Rack  string `json:"rack"`
		AZ    string `json:"az"`
		Nodes []struct {
			Node     string `json:"node"`
			Backends []struct {
				BackendID            string  `json:"backend_id"`
				Protocol             string  `json:"protocol"`
				Address              string  `json:"address"`
				ProbeIntervalSeconds float64 `json:"probe_interval_seconds"`
				ProbeTimeoutMs       int64   `json:"probe_timeout_ms"`
				BackoffSeconds       float64 `json:"backoff_seconds"`
				MaxRetries           int     `json:"max_retries"`
				SuccessLimit         int     `json:"success_limit"`
				FailureLimit         int     `json:"failure_limit"`
				BootState            string  `json:"boot_state"`
				WeightPPM            int64   `json:"weight_ppm"`
			} `json:"backends"`
		} `json:"nodes"`
	} `json:"topology"`
}

func loadV3(data []byte) (*LoadResult, error) {
	var doc v3Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, newError(ErrSchemaDecodeFailed, "v3 document decode failed", withCause(err))
	}
	stepMs, err := secondsToMs(doc.LogicClock.RoundSeconds, "logic_clock.round_seconds")
	if err != nil {
		return nil, err
	}
	maxDeferMs, err := secondsToMs(doc.LogicClock.MaxDeferSeconds, "logic_clock.max_defer_seconds")
	if err != nil {
		return nil, err
	}
	halfOpenMs, err := secondsToMs(doc.CountingRules.HalfOpenWaitS, "counting_rules.half_open_wait_seconds")
	if err != nil {
		return nil, err
	}
	conversions := []string{
		"v3 interval/backoff seconds converted to integer milliseconds",
		"v3 timeout already expressed in milliseconds",
		"v3 absolute success_limit/failure_limit mapped directly",
		"v3 rack-grouped tree flattened to canonical instances",
		"v3 omits active marker; schema default active_probe=true",
	}
	racks := make([]Rack, 0, len(doc.BudgetPolicy.RackPolicies))
	for _, r := range doc.BudgetPolicy.RackPolicies {
		racks = append(racks, Rack{ID: r.Name, AvailabilityZone: "", Concurrency: r.Permits})
	}
	p := &Project{
		ProjectID:         doc.Project.ID,
		ProjectVersion:    doc.Project.Version,
		Seed:              doc.Project.Seed,
		Rounds:            doc.Project.Rounds,
		StepMs:            stepMs,
		MaxDeferMs:        maxDeferMs,
		GlobalConcurrency: doc.BudgetPolicy.TotalPermits,
		Racks:             racks,
		Workers:           doc.WorkerPolicy.Threads,
		BufferCapacity:    doc.WorkerPolicy.ProbeBuffer,
		SlotPriority:      SlotPriority(doc.WorkerPolicy.SlotPriority),
		CountPolicy: CountPolicy{
			SuccessReachedAction: ThresholdAction(doc.CountingRules.OnSuccessLimit),
			FailureReachedAction: ThresholdAction(doc.CountingRules.OnFailureLimit),
			ResetOppositeStreak:  true,
			HalfOpenSuccessLimit: doc.CountingRules.HalfOpenOK,
			HalfOpenFailureLimit: doc.CountingRules.HalfOpenBad,
			HalfOpenDelayMs:      halfOpenMs,
		},
	}
	for _, rackGroup := range doc.Topology {
		for _, node := range rackGroup.Nodes {
			for _, b := range node.Backends {
				intervalMs, err := secondsToMs(b.ProbeIntervalSeconds, "probe_interval_seconds")
				if err != nil {
					return nil, err
				}
				backoffMs, err := secondsToMs(b.BackoffSeconds, "backoff_seconds")
				if err != nil {
					return nil, err
				}
				p.Instances = append(p.Instances, Instance{
					ID:               b.BackendID,
					NodeID:           node.Node,
					RackID:           rackGroup.Rack,
					Protocol:         strings.ToUpper(b.Protocol),
					Address:          b.Address,
					IntervalMs:       intervalMs,
					TimeoutMs:        b.ProbeTimeoutMs,
					BackoffMs:        backoffMs,
					RetryLimit:       b.MaxRetries,
					SuccessThreshold: b.SuccessLimit,
					FailureThreshold: b.FailureLimit,
					InitialState:     normalizeState(b.BootState),
					WeightPPM:        b.WeightPPM,
					ActiveProbe:      true, // omitted field has an explicit schema default.
				})
			}
		}
	}
	res, lerr := canonicalize(p, conversions)
	if lerr != nil {
		return nil, lerr
	}
	res.SourceSchema = SchemaV3
	res.Project.SourceSchema = SchemaV3
	return res, nil
}

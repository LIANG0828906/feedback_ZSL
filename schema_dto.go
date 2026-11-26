package cachebench

import "encoding/json"

type envelope struct {
	SchemaVersion string `json:"schema_version"`
}

type docV1 struct {
	SchemaVersion  string         `json:"schema_version"`
	ProjectID      string         `json:"project_id"`
	Seed           int64          `json:"seed"`
	TickDurationMS int64          `json:"tick_duration_ms"`
	DurationTicks  int64          `json:"duration_ticks"`
	WindowTicks    int64          `json:"window_ticks"`
	WeightScale    string         `json:"weight_scale"`
	CapacityScale  string         `json:"capacity_scale"`
	RoutingMode    string         `json:"routing_mode"`
	FlatNodes      []nodeV1       `json:"flat_nodes"`
	Requests       []requestV1    `json:"request_stream"`
	Observations   []obsV1        `json:"observations"`
	CriticalCases  []CriticalCase `json:"critical_cases"`
}

type nodeV1 struct {
	NodeID                    string `json:"node_id"`
	RackName                  string `json:"rack_name"`
	Priority                  int    `json:"priority_no"`
	DeclarationOrder          int    `json:"declaration_order"`
	WeightPermille            int64  `json:"weight_permille"`
	MaxConnections            int    `json:"max_connections"`
	ServiceTicks              int64  `json:"service_ticks"`
	TimeoutTicks              int64  `json:"timeout_ticks"`
	ConstraintNo              string `json:"constraint_no"`
	CapacityPermille          int64  `json:"capacity_permille"`
	InflightThreshold         int    `json:"inflight_threshold"`
	RecoveryInflightThreshold int    `json:"recovery_inflight_threshold"`
	ErrorRateLimitPermille    int64  `json:"error_rate_limit_permille"`
	ErrorRateRecoveryPermille int64  `json:"error_rate_recovery_permille"`
	TriggerTicks              int64  `json:"trigger_ticks"`
	RecoveryTicks             int64  `json:"recovery_ticks"`
	MinErrorSamples           int64  `json:"min_error_samples"`
}

type requestV1 struct {
	RequestID   string   `json:"request_id"`
	Ordinal     int      `json:"ordinal"`
	AtTick      int64    `json:"at_tick"`
	Candidates  []string `json:"candidate_nodes"`
	ForcedError bool     `json:"forced_error"`
}

type obsV1 struct {
	WindowIndex       int   `json:"window_index"`
	AtTick            int64 `json:"at_tick"`
	RequestCount      int64 `json:"request_count"`
	ErrorCount        int64 `json:"error_count"`
	TotalLatencyTicks int64 `json:"total_latency_ticks"`
}

type docV2 struct {
	SchemaVersion string         `json:"schemaVersion"`
	ProjectID     string         `json:"projectId"`
	Seed          int64          `json:"seed"`
	DurationMS    int64          `json:"durationMs"`
	WindowMS      int64          `json:"windowMs"`
	WeightScale   string         `json:"weightScale"`
	CapacityScale string         `json:"capacityScale"`
	RoutingMode   string         `json:"routingMode"`
	Topology      topologyV2     `json:"topology"`
	Requests      []requestV2    `json:"requests"`
	Observations  []obsV2        `json:"observations"`
	CriticalCases []CriticalCase `json:"criticalCases"`
}

type topologyV2 struct {
	Racks []rackV2 `json:"racks"`
}

type rackV2 struct {
	RackID string   `json:"rackId"`
	Nodes  []nodeV2 `json:"nodes"`
}

type nodeV2 struct {
	NodeID                    string `json:"nodeId"`
	Priority                  int    `json:"priority"`
	DeclarationOrder          int    `json:"declarationOrder"`
	WeightPermyriad           int64  `json:"weightPermyriad"`
	MaxConnections            int    `json:"maxConnections"`
	ServiceMS                 int64  `json:"serviceMs"`
	TimeoutMS                 int64  `json:"timeoutMs"`
	ConstraintID              string `json:"constraintId"`
	CapacityPermyriad         int64  `json:"capacityPermyriad"`
	InflightThreshold         int    `json:"inflightThreshold"`
	RecoveryInflightThreshold int    `json:"recoveryInflightThreshold"`
	ErrorRateLimitPPM         int64  `json:"errorRateLimitPpm"`
	ErrorRateRecoveryPPM      int64  `json:"errorRateRecoveryPpm"`
	TriggerMS                 int64  `json:"triggerMs"`
	RecoveryMS                int64  `json:"recoveryMs"`
	MinErrorSamples           int64  `json:"minErrorSamples"`
}

type requestV2 struct {
	RequestID   string   `json:"requestId"`
	Ordinal     int      `json:"ordinal"`
	AtMS        int64    `json:"atMs"`
	Candidates  []string `json:"candidateNodes"`
	ForcedError bool     `json:"forcedError"`
}

type obsV2 struct {
	WindowIndex    int   `json:"windowIndex"`
	WindowMS       int64 `json:"windowMs"`
	RequestCount   int64 `json:"requestCount"`
	ErrorCount     int64 `json:"errorCount"`
	TotalLatencyMS int64 `json:"totalLatencyMs"`
	Overloaded     *bool `json:"overloaded,omitempty"`
}

type docV3 struct {
	SchemaVersion string         `json:"schema-version"`
	ProjectID     string         `json:"project-id"`
	Seed          int64          `json:"seed"`
	DurationQ3Sec int64          `json:"duration-seconds-q3"`
	WindowQ3Sec   int64          `json:"window-seconds-q3"`
	WeightScale   string         `json:"weight-scale"`
	CapacityScale string         `json:"capacity-scale"`
	RoutingMode   string         `json:"routing-mode"`
	Topology      topologyV3     `json:"topology"`
	Requests      []requestV3    `json:"requests"`
	Observations  []obsV3        `json:"observations"`
	CriticalCases []CriticalCase `json:"critical-cases"`
}

type topologyV3 struct {
	RackTree []rackV3 `json:"rack-tree"`
}

type rackV3 struct {
	RackID string   `json:"rack-id"`
	Nodes  []nodeV3 `json:"nodes"`
}

type nodeV3 struct {
	NodeID                    string `json:"node-id"`
	Priority                  int    `json:"priority"`
	DeclarationOrder          int    `json:"declaration-order"`
	WeightPermyriad           int64  `json:"weight-permyriad"`
	MaxConnections            int    `json:"max-connections"`
	ServiceQ3Seconds          int64  `json:"service-seconds-q3"`
	TimeoutMS                 int64  `json:"timeout-ms"`
	ConstraintID              string `json:"constraint-id"`
	CapacityPermyriad         int64  `json:"capacity-permyriad"`
	InflightThreshold         int    `json:"inflight-threshold"`
	RecoveryInflightThreshold int    `json:"recovery-inflight-threshold"`
	ErrorRateLimitPPM         int64  `json:"error-rate-limit-ppm"`
	ErrorRateRecoveryPPM      int64  `json:"error-rate-recovery-ppm"`
	TriggerMS                 int64  `json:"trigger-ms"`
	RecoveryMS                int64  `json:"recovery-ms"`
	MinErrorSamples           int64  `json:"min-error-samples"`
}

type requestV3 struct {
	RequestID   string   `json:"request-id"`
	Ordinal     int      `json:"ordinal"`
	AtQ3Seconds int64    `json:"at-seconds-q3"`
	Candidates  []string `json:"candidate-nodes"`
	ForcedError bool     `json:"forced-error"`
}

type obsV3 struct {
	WindowIndex    int   `json:"window-index"`
	AtQ3Seconds    int64 `json:"at-seconds-q3"`
	RequestCount   int64 `json:"request-count"`
	ErrorCount     int64 `json:"error-count"`
	TotalLatencyMS int64 `json:"total-latency-ms"`
	OverloadFlag   *bool `json:"overload-flag,omitempty"`
}

func marshalDoc(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, NewError(CodeSchemaField, "encode schema document failed: %v", err)
	}
	return b, nil
}

// Load detects the schema_version field and dispatches to the matching loader.
func Load(data []byte) (*Project, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, NewError(CodeSchemaField, "invalid JSON envelope: %v", err)
	}
	version := ""
	for _, key := range []string{"schema_version", "schemaVersion", "schema-version"} {
		if raw, ok := head[key]; ok {
			_ = json.Unmarshal(raw, &version)
			if version != "" {
				break
			}
		}
	}
	switch version {
	case SchemaV1:
		return LoadV1(data)
	case SchemaV2:
		return LoadV2(data)
	case SchemaV3:
		return LoadV3(data)
	default:
		return nil, NewError(CodeSchemaVersion, "unsupported schema_version %q", version).WithField("schema_version")
	}
}

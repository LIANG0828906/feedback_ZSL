package schemaio

// V1 uses snake_case fields, epoch milliseconds, seconds-like durations and a
// flat edge table. It is also the variant that may carry truth-check fields.
type V1Document struct {
	SchemaVersion string      `json:"schema_version"`
	ProjectID     string      `json:"project_id"`
	Revision      string      `json:"revision"`
	Seed          int64       `json:"seed"`
	Window        V1Window    `json:"window"`
	Policy        V1Policy    `json:"policy"`
	Services      []V1Service `json:"services"`
	Dependencies  []V1Edge    `json:"dependencies"`
	Events        []V1Event   `json:"events"`
	Truth         *V1Truth    `json:"truth,omitempty"`
}

type V1Window struct {
	StartEpochMS   int64 `json:"start_epoch_ms"`
	EndEpochMS     int64 `json:"end_epoch_ms"`
	TickIntervalMS int64 `json:"tick_interval_ms,omitempty"`
}

type V1Policy struct {
	DedupWindowSeconds       int64 `json:"dedup_window_seconds"`
	MergeGapSeconds          int64 `json:"merge_gap_seconds"`
	LookbackSeconds          int64 `json:"lookback_seconds"`
	MaxPropagationGeneration int   `json:"max_propagation_generation"`
	ScoreScale               int   `json:"score_scale"`
	ScoreToleranceRaw        int64 `json:"score_tolerance_raw"`
}

type V1Service struct {
	ServiceID                      string `json:"service_id"`
	DisplayName                    string `json:"display_name"`
	ErrorUnhealthyThresholdBP      int    `json:"error_unhealthy_threshold_bp"`
	SaturationUnhealthyThresholdBP int    `json:"saturation_unhealthy_threshold_bp"`
	SteadyErrorBaselineBP          int    `json:"steady_error_baseline_bp"`
	SteadySaturationBaselineBP     int    `json:"steady_saturation_baseline_bp"`
}

type V1Edge struct {
	UpstreamCallerID       string `json:"upstream_caller_id"`
	DownstreamDependencyID string `json:"downstream_dependency_id"`
}

type V1Event struct {
	EventID               string `json:"event_id"`
	LogicalSequence       int    `json:"logical_sequence"`
	SourceKind            string `json:"source_kind"`
	ServiceID             string `json:"service_id"`
	EventKind             string `json:"event_kind"`
	DedupFingerprint      string `json:"dedup_fingerprint"`
	OccurredAtEpochMS     int64  `json:"occurred_at_epoch_ms"`
	IngestLagSeconds      int64  `json:"ingest_lag_seconds"`
	AliveTTLSeconds       int64  `json:"alive_ttl_seconds"`
	ChangeType            string `json:"change_type,omitempty"`
	ChangeWeightPermyriad int    `json:"change_weight_permyriad"`
	ErrorRateBP           *int   `json:"error_rate_bp,omitempty"`
	SaturationBP          *int   `json:"saturation_bp,omitempty"`
	RecoveryFor           string `json:"recovery_for,omitempty"`
}

type V1Truth struct {
	AlignedEvents []V1TruthEvent     `json:"aligned_events"`
	Intervals     []V1TruthInterval  `json:"intervals"`
	Candidates    []V1TruthCandidate `json:"candidates"`
}

type V1TruthEvent struct {
	EventID      string   `json:"event_id"`
	KeptID       string   `json:"kept_id"`
	EffectiveMS  int64    `json:"effective_ms"`
	ServiceID    string   `json:"service_id"`
	DedupedCount int      `json:"deduped_count"`
	MergedRaw    int      `json:"merged_raw_count"`
	MergedIDs    []string `json:"merged_ids,omitempty"`
}

type V1TruthInterval struct {
	Ordinal     int      `json:"ordinal"`
	ServiceID   string   `json:"service_id"`
	StartMS     int64    `json:"start_ms"`
	EndMS       int64    `json:"end_ms"`
	EvidenceIDs []string `json:"evidence_ids"`
	ThresholdBP int      `json:"threshold_bp"`
	Metric      string   `json:"metric"`
}

type V1TruthCandidate struct {
	EventID        string `json:"event_id"`
	Rank           int    `json:"rank"`
	ScorePermyriad int64  `json:"score_permyriad"`
}

// V2 uses camelCase, relative ticks, millisecond durations and a nested tree.
type V2Document struct {
	SchemaVersion  string      `json:"schemaVersion"`
	ProjectID      string      `json:"projectId"`
	Revision       string      `json:"revision"`
	Seed           int64       `json:"seed"`
	Window         V2Window    `json:"window"`
	Policy         V2Policy    `json:"policy"`
	Services       []V2Service `json:"services"`
	DependencyTree []V2Node    `json:"dependencyTree"`
	Events         []V2Event   `json:"events"`
}

type V2Window struct {
	OriginEpochMS  int64 `json:"originEpochMs"`
	StartTick      int64 `json:"startTick"`
	EndTick        int64 `json:"endTick"`
	TickIntervalMS int64 `json:"tickIntervalMs"`
}

type V2Policy struct {
	DedupToleranceMS         int64 `json:"dedupToleranceMs"`
	MergeGapMS               int64 `json:"mergeGapMs"`
	LookbackMS               int64 `json:"lookbackMs"`
	MaxPropagationGeneration int   `json:"maxPropagationGeneration"`
	ScoreScale               int   `json:"scoreScale"`
	ScoreToleranceRaw        int64 `json:"scoreToleranceRaw"`
}

type V2Service struct {
	ServiceID                   string `json:"serviceId"`
	DisplayName                 string `json:"displayName"`
	ErrorThresholdPermille      int    `json:"errorThresholdPermille"`
	SaturationThresholdPermille int    `json:"saturationThresholdPermille"`
	BaselineErrorPermille       int    `json:"baselineErrorPermille"`
	BaselineSaturationPermille  int    `json:"baselineSaturationPermille"`
}

type V2Node struct {
	ServiceID string   `json:"serviceId"`
	Calls     []V2Node `json:"calls,omitempty"`
}

type V2Event struct {
	EventID              string `json:"eventId"`
	LogicalSequence      int    `json:"logicalSequence"`
	SourceKind           string `json:"sourceKind"`
	ServiceID            string `json:"serviceId"`
	EventKind            string `json:"eventKind"`
	DedupFingerprint     string `json:"dedupFingerprint"`
	AtTick               int64  `json:"atTick"`
	IngestLagMS          int64  `json:"ingestLagMs"`
	AliveTTLMS           int64  `json:"aliveTtlMs"`
	ChangeType           string `json:"changeType,omitempty"`
	ChangeWeightPermille int    `json:"changeWeightPermille"`
	ErrorRatePermille    *int   `json:"errorRatePermille,omitempty"`
	SaturationPermille   *int   `json:"saturationPermille,omitempty"`
	RecoveryFor          string `json:"recoveryFor,omitempty"`
}

// V3 uses short snake_case field names, relative milliseconds and mixed units.
type V3Document struct {
	Schema          string      `json:"schema"`
	ProjectCode     string      `json:"project_code"`
	RevisionID      string      `json:"revision_id"`
	Seed            int64       `json:"seed"`
	TimeWindow      V3Window    `json:"time_window"`
	AnalysisConfig  V3Config    `json:"analysis_config"`
	ManagedServices []V3Service `json:"managed_services"`
	TopologyEdges   []V3Edge    `json:"topology_edges"`
	TimelineEvents  []V3Event   `json:"timeline_events"`
}

type V3Window struct {
	OriginEpochMS int64 `json:"origin_epoch_ms"`
	StartRelMS    int64 `json:"start_rel_ms"`
	EndRelMS      int64 `json:"end_rel_ms"`
}

type V3Config struct {
	DedupToleranceMS       int64 `json:"dedup_tolerance_ms"`
	MergeGapMS             int64 `json:"merge_gap_ms"`
	LookbackMS             int64 `json:"lookback_ms"`
	PropagationGenerations int   `json:"propagation_generations"`
	ScoreScale             int   `json:"score_scale"`
	ScoreToleranceRaw      int64 `json:"score_tolerance_raw"`
}

type V3Service struct {
	ID                           string `json:"id"`
	Label                        string `json:"label"`
	ErrorThresholdPermyriad      int    `json:"error_threshold_permyriad"`
	SaturationThresholdPermyriad int    `json:"saturation_threshold_permyriad"`
	BaselineErrorPermyriad       int    `json:"baseline_error_permyriad"`
	BaselineSaturationPermyriad  int    `json:"baseline_saturation_permyriad"`
}

type V3Edge struct {
	Caller     string `json:"caller"`
	Dependency string `json:"dependency"`
}

type V3Event struct {
	ID                  string `json:"id"`
	Seq                 int    `json:"seq"`
	Source              string `json:"source"`
	Service             string `json:"service"`
	Kind                string `json:"kind"`
	Fingerprint         string `json:"fingerprint"`
	AtRelMS             int64  `json:"at_rel_ms"`
	LagMS               int64  `json:"lag_ms"`
	TTLSeconds          int64  `json:"ttl_seconds"`
	ChangeType          string `json:"change_type,omitempty"`
	WeightPermyriad     int    `json:"weight_permyriad"`
	ErrorRatePermyriad  *int   `json:"error_rate_permyriad,omitempty"`
	SaturationPermyriad *int   `json:"saturation_permyriad,omitempty"`
	RecoveryFor         string `json:"recovery_for,omitempty"`
}

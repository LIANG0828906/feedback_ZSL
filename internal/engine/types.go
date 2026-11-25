package engine

import (
	"combatjudge/internal/model"
	"combatjudge/internal/store"
)

const (
	DefaultIntervalFrames = 2
	DefaultWorkers        = 2
	DefaultBufferBytes    = 256 * 1024
	DefaultQueueBudget    = 8
	HitToleranceQ         = int64(1_000)
	DependencyMarginQ     = int64(1_000_000)
)

type Options struct {
	PathLabel           string
	IntervalFrames      int
	MaxParallel         int
	GeometryBufferBytes int
	MaxQueuedIntervals  int
	SnapshotID          string
	DBPath              string
	StopAfterInterval   int
	Injection           Injection
}

type Injection struct {
	CorruptColliderNo int
	BadStepMS         int64
	UseBadStep        bool
	UnitContradiction bool
	FailOnceIntervals map[int]bool
}

type IntervalSpec struct {
	No         int `json:"no"`
	StartFrame int `json:"start_frame"`
	EndFrame   int `json:"end_frame"`
}

type TruncationRecord struct {
	Frame       int        `json:"frame"`
	SweepID     string     `json:"sweep_id,omitempty"`
	FieldPath   string     `json:"field_path"`
	Axis        string     `json:"axis,omitempty"`
	Op          string     `json:"op"`
	Position    model.Vec3 `json:"position,omitempty"`
	ValueQ      int64      `json:"value_q,omitempty"`
	Remainder   int64      `json:"remainder,omitempty"`
	Denominator int64      `json:"denominator,omitempty"`
	RuleVersion string     `json:"rule_version"`
}

type ContactResult struct {
	ColliderNo int        `json:"collider_no"`
	ColliderID string     `json:"collider_id"`
	Part       string     `json:"part"`
	Surface    string     `json:"surface"`
	TOI        float64    `json:"toi"`
	TOIQ       int64      `json:"toi_q"`
	Point      model.Vec3 `json:"point_q"`
	DistanceQ  int64      `json:"distance_q"`
	Hit        bool       `json:"hit"`
}

type FrameGeometry struct {
	Frame            int                `json:"frame"`
	SweepID          string             `json:"sweep_id"`
	AtMS             int64              `json:"at_ms"`
	MovementFrom     model.Vec3         `json:"movement_from"`
	MovementTo       model.Vec3         `json:"movement_to"`
	Truncations      []TruncationRecord `json:"truncations"`
	Contacts         []ContactResult    `json:"contacts"`
	NearestDistanceQ int64              `json:"nearest_distance_q"`
}

type GeometryResult struct {
	IntervalNo int             `json:"interval_no"`
	Frames     []FrameGeometry `json:"frames"`
}

type SegmentResult struct {
	Segment          int        `json:"segment"`
	Status           string     `json:"status"`
	Frame            int        `json:"frame"`
	ColliderNo       int        `json:"collider_no,omitempty"`
	ColliderID       string     `json:"collider_id,omitempty"`
	Part             string     `json:"part,omitempty"`
	Surface          string     `json:"surface,omitempty"`
	TOI              float64    `json:"toi,omitempty"`
	TOIQ             int64      `json:"toi_q,omitempty"`
	Point            model.Vec3 `json:"point_q,omitempty"`
	DistanceQ        int64      `json:"distance_q,omitempty"`
	DamageBeforeQ    int64      `json:"damage_before_q,omitempty"`
	RemainingDamageQ int64      `json:"remaining_damage_q"`
	Reason           string     `json:"reason,omitempty"`
}

type FrameResult struct {
	Frame            int                `json:"frame"`
	SweepID          string             `json:"sweep_id"`
	AtMS             int64              `json:"at_ms"`
	Segments         []SegmentResult    `json:"segments"`
	FirstColliderNo  int                `json:"first_collider_no"`
	FirstPart        string             `json:"first_part"`
	FirstSurface     string             `json:"first_surface"`
	RemainingDamageQ int64              `json:"remaining_damage_q"`
	NearestDistanceQ int64              `json:"nearest_distance_q"`
	Truncations      []TruncationRecord `json:"truncations"`
}

type SweepState struct {
	DamageQ      int64 `json:"damage_q"`
	SegmentCount int   `json:"segment_count"`
	HitColliders []int `json:"hit_colliders"`
}

type State struct {
	Sweeps map[string]SweepState `json:"sweeps"`
}

type Dependency struct {
	IntervalNo int    `json:"interval_no"`
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	Hash       string `json:"hash,omitempty"`
	Reason     string `json:"reason"`
}

type IntervalResult struct {
	No               int           `json:"no"`
	StartFrame       int           `json:"start_frame"`
	EndFrame         int           `json:"end_frame"`
	CacheKey         string        `json:"cache_key"`
	UpstreamKey      string        `json:"upstream_key"`
	ContentHash      string        `json:"content_hash"`
	Frames           []FrameResult `json:"frames"`
	StateOut         State         `json:"state_out"`
	Dependencies     []Dependency  `json:"dependencies"`
	Status           string        `json:"status"`
	AlgorithmVersion string        `json:"algorithm_version"`
}

type Invalidation struct {
	IntervalNo int    `json:"interval_no"`
	Kind       string `json:"kind"`
	Reason     string `json:"reason"`
}

type FailedInterval struct {
	IntervalNo int          `json:"interval_no"`
	Error      *model.Error `json:"error"`
	Attempts   int          `json:"attempts"`
}

type Report struct {
	Path            string                  `json:"path"`
	SnapshotID      string                  `json:"snapshot_id"`
	Generation      int                     `json:"generation"`
	Voided          bool                    `json:"voided"`
	VoidReason      string                  `json:"void_reason,omitempty"`
	Intervals       []*IntervalResult       `json:"intervals"`
	Frames          []FrameResult           `json:"frames"`
	CommitSequence  []int                   `json:"commit_sequence"`
	WriteOrder      []int                   `json:"write_order"`
	ReusedIntervals []int                   `json:"reused_intervals"`
	Recomputed      []int                   `json:"recomputed_intervals"`
	Invalidations   []Invalidation          `json:"invalidations"`
	QueueWaits      []store.QueueWaitRecord `json:"queue_waits"`
	Failures        []FailedInterval        `json:"failures"`
}

type CompareMismatch struct {
	Frame   int    `json:"frame"`
	SweepID string `json:"sweep_id"`
	Field   string `json:"field"`
	Want    string `json:"want"`
	Got     string `json:"got"`
}

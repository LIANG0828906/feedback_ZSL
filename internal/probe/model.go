package probe

import (
	"time"

	"lightprobe/internal/scene"
)

type Config struct {
	MaxParallelism       int    `json:"max_parallelism"`
	ProbeBufferPerWorker int    `json:"probe_buffer_per_worker"`
	SHBufferCoeffBudget  int    `json:"sh_buffer_coeff_budget"`
	CheckpointPath       string `json:"checkpoint_path,omitempty"`

	beforeBudgetWait func(resource string, probeID string)
}

func DefaultConfig() Config {
	return Config{
		MaxParallelism:       4,
		ProbeBufferPerWorker: 2,
		SHBufferCoeffBudget:  scene.SHCoefficientCount * 3 * 2,
	}
}

func (c Config) Normalize() Config {
	if c.MaxParallelism <= 0 {
		c.MaxParallelism = 1
	}
	if c.MaxParallelism > 8 {
		c.MaxParallelism = 8
	}
	if c.ProbeBufferPerWorker <= 0 {
		c.ProbeBufferPerWorker = 1
	}
	if c.SHBufferCoeffBudget < scene.SHCoefficientCount*3 {
		c.SHBufferCoeffBudget = scene.SHCoefficientCount * 3
	}
	return c
}

type ProbeStatus string

const (
	ProbePending       ProbeStatus = "pending"
	ProbeSampling      ProbeStatus = "sampling"
	ProbeReconstructed ProbeStatus = "reconstructed"
	ProbeFailed        ProbeStatus = "failed"
)

type RoomState string

const (
	RoomPending       RoomState = "pending"
	RoomSampling      RoomState = "sampling"
	RoomReconstructed RoomState = "reconstructed"
	RoomCommitted     RoomState = "committed"
	RoomFailed        RoomState = "failed"
)

type Record struct {
	ID                string      `json:"id"`
	RoomID            string      `json:"room_id"`
	I                 int         `json:"i"`
	J                 int         `json:"j"`
	K                 int         `json:"k"`
	Position          scene.Vec3  `json:"position"`
	InputRadiance     scene.Vec3  `json:"input_radiance"`
	Radiance          scene.Vec3  `json:"radiance"`
	VisibilitySamples int         `json:"visibility_samples"`
	PhotonCell        [3]int      `json:"photon_cell"`
	SH                [9][3]int64 `json:"sh_fixed"`
	SHDigest          string      `json:"sh_digest"`
	Status            ProbeStatus `json:"status"`
	Dirty             bool        `json:"dirty"`
	Attempts          int         `json:"attempts"`
	RecomputeCount    int         `json:"recompute_count"`
	LastError         string      `json:"last_error,omitempty"`
}

type Submission struct {
	Seq        int        `json:"seq"`
	Order      int        `json:"order"`
	ProbeID    string     `json:"probe_id"`
	RoomID     string     `json:"room_id"`
	Radiance   scene.Vec3 `json:"radiance"`
	SHDigest   string     `json:"sh_digest"`
	Recomputed bool       `json:"recomputed"`
	At         time.Time  `json:"at"`
}

type QueueEvent struct {
	ProbeID   string  `json:"probe_id"`
	Worker    int     `json:"worker,omitempty"`
	Resource  string  `json:"resource"`
	Reason    string  `json:"reason"`
	WaitNanos int64   `json:"wait_nanos"`
	WaitMS    float64 `json:"wait_ms"`
	Detail    string  `json:"detail,omitempty"`
}

type InvalidationItem struct {
	NodeID         string `json:"node_id"`
	Kind           string `json:"kind"`
	RoomID         string `json:"room_id,omitempty"`
	ProbeID        string `json:"probe_id,omitempty"`
	Reason         string `json:"reason"`
	AdjacentRoomID string `json:"adjacent_room_id,omitempty"`
}

type RecomputeReport struct {
	Jobs             int                `json:"jobs"`
	RecomputedProbes []string           `json:"recomputed_probes"`
	FailedProbes     []string           `json:"failed_probes"`
	CacheHits        int                `json:"cache_hits"`
	PixelCacheHits   int                `json:"pixel_cache_hits"`
	PixelsRecomputed []string           `json:"pixels_recomputed"`
	SubmissionIDs    []string           `json:"submission_ids"`
	QueueEvents      []QueueEvent       `json:"queue_events"`
	Invalidation     []InvalidationItem `json:"invalidation"`
	TopologicalOrder []string           `json:"topological_order,omitempty"`
	StartedAt        time.Time          `json:"started_at"`
	Duration         time.Duration      `json:"duration_ns"`
}

type MutationPlan struct {
	Mutation     string             `json:"mutation"`
	TargetID     string             `json:"target_id"`
	Invalidation []InvalidationItem `json:"invalidation"`
	ChangedNodes []string           `json:"changed_nodes"`
}

type PixelResult struct {
	PixelID       string     `json:"pixel_id"`
	RoomID        string     `json:"room_id"`
	BoundaryID    string     `json:"boundary_id,omitempty"`
	Position      scene.Vec3 `json:"position"`
	Radiance      scene.Vec3 `json:"radiance"`
	OwnerRadiance scene.Vec3 `json:"owner_radiance"`
	OtherRadiance scene.Vec3 `json:"other_radiance,omitempty"`
	Alpha         float64    `json:"blend_alpha"`
	Color         scene.Vec3 `json:"color"`
	Corners       []string   `json:"corners"`
	CacheHit      bool       `json:"cache_hit"`
	Error         string     `json:"error,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Progress struct {
	CommittedRooms      int                  `json:"committed_rooms"`
	TotalRooms          int                  `json:"total_rooms"`
	ReconstructedRooms  int                  `json:"reconstructed_rooms"`
	FailedRooms         int                  `json:"failed_rooms"`
	TotalProbes         int                  `json:"total_probes"`
	ReconstructedProbes int                  `json:"reconstructed_probes"`
	FailedProbes        int                  `json:"failed_probes"`
	SubmittedProbes     int                  `json:"submitted_probes"`
	RoomStates          map[string]RoomState `json:"room_states"`
}

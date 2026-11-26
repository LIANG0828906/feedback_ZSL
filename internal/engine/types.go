package engine

import (
	"time"

	"layerforge/internal/apperr"
	"layerforge/internal/checkpoint"
	"layerforge/internal/model"
	"layerforge/internal/sqlitefile"
)

type BuildOptions struct {
	Budget              *BudgetOverride
	PreviousProject     *model.Project
	InterruptAfterRound int
	CommitHook          func(round int, stage sqlitefile.CommitStage) error
}

type BudgetOverride struct {
	Latency         time.Duration
	MaxRounds       int
	ThreadLimit     int
	MemoryPerThread int64
	BufferPerThread int
}

type Decision struct {
	Step        int    `json:"step"`
	Name        string `json:"name"`
	LayerID     string `json:"layerId"`
	CacheKey    string `json:"cacheKey"`
	Digest      string `json:"digest"`
	SizeBytes   int64  `json:"sizeBytes"`
	Hit         bool   `json:"hit"`
	Source      string `json:"source"`
	BuildCostMS int64  `json:"buildCostMs"`
	MemoryBytes int64  `json:"memoryBytes"`
}

type BuiltLayer struct {
	Step        int    `json:"step"`
	LayerID     string `json:"layerId"`
	CacheKey    string `json:"cacheKey"`
	Digest      string `json:"digest"`
	SizeBytes   int64  `json:"sizeBytes"`
	BuildCostMS int64  `json:"buildCostMs"`
	MemoryBytes int64  `json:"memoryBytes"`
	Streaming   bool   `json:"streaming,omitempty"`
}

type Deferral struct {
	Step    int         `json:"step"`
	LayerID string      `json:"layerId"`
	Reason  string      `json:"reason"`
	Code    apperr.Code `json:"code"`
	Rank    int         `json:"rank"`
	Round   int         `json:"round"`
}

type Degradation struct {
	Code         apperr.Code `json:"code"`
	Strategy     string      `json:"strategy"`
	Reason       string      `json:"reason"`
	Round        int         `json:"round"`
	ImpactLayers []string    `json:"impactLayers"`
}

type BackpressureEvent struct {
	Code    apperr.Code `json:"code"`
	Round   int         `json:"round"`
	Step    int         `json:"step"`
	LayerID string      `json:"layerId"`
	Rank    int         `json:"rank"`
}

type Recovery struct {
	Code   apperr.Code `json:"code,omitempty"`
	Reason string      `json:"reason,omitempty"`
	Reset  bool        `json:"reset"`
}

type RoundReport struct {
	Round              int          `json:"round"`
	Built              []BuiltLayer `json:"built"`
	Deferred           []Deferral   `json:"deferred"`
	Remaining          int          `json:"remaining"`
	LatencyBudgetMS    int64        `json:"latencyBudgetMs"`
	LatencyUsedMS      int64        `json:"latencyUsedMs"`
	LatencyRemainingMS int64        `json:"latencyRemainingMs"`
	WorkerLimit        int          `json:"workerLimit"`
	WorkersUsed        int          `json:"workersUsed"`
	MemoryPerThread    int64        `json:"memoryPerThreadBytes"`
	PeakMemoryBytes    int64        `json:"peakMemoryBytes"`
}

type BuildResult struct {
	ProjectID    string              `json:"projectId"`
	ProjectHash  string              `json:"projectHash"`
	Decisions    []Decision          `json:"decisions"`
	Rounds       []RoundReport       `json:"rounds"`
	Degradations []Degradation       `json:"degradations"`
	Backpressure []BackpressureEvent `json:"backpressure"`
	Codes        []apperr.Code       `json:"codes"`
	Recovery     Recovery            `json:"recovery"`
	Interrupted  bool                `json:"interrupted"`
	HitCount     int                 `json:"hitCount"`
	BuildCount   int                 `json:"buildCount"`
	FinalDigests map[string]string   `json:"finalDigests"`
}

type CleanupResult struct {
	RemovedLayers []string             `json:"removedLayers"`
	KeptLayers    []string             `json:"keptLayers"`
	RebuildSteps  []int                `json:"rebuildSteps"`
	Decisions     []Decision           `json:"decisions"`
	Snapshot      *checkpoint.Snapshot `json:"-"`
}

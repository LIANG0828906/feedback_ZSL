package engine

import (
	"atelier/internal/model"
)

const (
	DensityAbsTolerance = 0.012
	RarityL1Tolerance   = 0.055
	ReconCountRelTol    = 0.10
	ReconCountAbsTol    = 5
	HeatAbsTolerance    = 0.018
)

type Tolerances struct {
	DensityAbs                 float64 `json:"densityAbs"`
	RarityL1                   float64 `json:"rarityL1"`
	ReconstructionCountPercent float64 `json:"reconstructionCountPercent"`
	ReconstructionCountAbs     float64 `json:"reconstructionCountAbs"`
	HeatmapAbs                 float64 `json:"heatmapAbs"`
}

func DefaultTolerances() Tolerances {
	return Tolerances{
		DensityAbs:                 DensityAbsTolerance,
		RarityL1:                   RarityL1Tolerance,
		ReconstructionCountPercent: ReconCountRelTol * 100,
		ReconstructionCountAbs:     ReconCountAbsTol,
		HeatmapAbs:                 HeatAbsTolerance,
	}
}

type CacheTrace struct {
	Ordinal     int    `json:"ordinal"`
	ZoneID      string `json:"zoneId"`
	SamplingKey string `json:"samplingKey"`
	EstimateKey string `json:"estimateKey"`
	PointsKey   string `json:"pointsKey"`
	ContentHash string `json:"contentHash"`
}

type ZoneInvalidation struct {
	ZoneID      string `json:"zoneId"`
	Resample    bool   `json:"resample"`
	Reestimate  bool   `json:"reestimate"`
	Reconstruct bool   `json:"reconstruct"`
	Heatmap     bool   `json:"heatmap"`
	Reason      string `json:"reason"`
}

type InvalidationEntry struct {
	ZoneID           string `json:"zoneId"`
	TileX            int    `json:"tileX"`
	TileY            int    `json:"tileY"`
	ReuseState       string `json:"reuseState"`
	Reason           string `json:"reason"`
	ReusedPixels     int    `json:"reusedPixels"`
	RecomputedPixels int    `json:"recomputedPixels"`
}

type InvalidationReport struct {
	AffectedZones     []ZoneInvalidation  `json:"affectedZones"`
	Entries           []InvalidationEntry `json:"entries"`
	AffectedTileCount int                 `json:"affectedTileCount"`
	ReusedTileCount   int                 `json:"reusedTileCount"`
	ReusedPixels      int                 `json:"reusedPixels"`
	RecomputedPixels  int                 `json:"recomputedPixels"`
	ReusedZoneCount   int                 `json:"reusedZoneCount"`
	AffectedNodes     []string            `json:"affectedNodes"`
	Explanation       string              `json:"explanation"`
}

type RunMetrics struct {
	MaxParallelism      int    `json:"maxParallelism"`
	PerWorkerBufferSize int    `json:"perWorkerBufferSize"`
	Tasks               int64  `json:"tasks"`
	BufferBlockEvents   int64  `json:"bufferBlockEvents"`
	LastBlockReason     string `json:"lastBlockReason"`
	ZoneTasks           int    `json:"zoneTasks"`
	HeatmapTileTasks    int    `json:"heatmapTileTasks"`
}

type Snapshot struct {
	RegionID         string                `json:"regionId"`
	Seed             int64                 `json:"seed"`
	AlgorithmVersion string                `json:"algorithmVersion"`
	Revision         uint64                `json:"revision"`
	Bounds           model.Bounds          `json:"bounds"`
	Config           model.SamplingConfig  `json:"config"`
	Boundary         model.Polygon         `json:"boundary"`
	Zones            []model.Zone          `json:"zones"`
	Exclusions       []model.Exclusion     `json:"exclusions"`
	Terrain          model.TerrainRaster   `json:"terrain"`
	ZoneStates       []model.ZoneState     `json:"zoneStates"`
	Points           []model.ResourcePoint `json:"points"`
	Heatmap          model.Heatmap         `json:"heatmap"`
	Report           BiasReport            `json:"report"`
	Graph            DependencyGraph       `json:"graph"`
	Invalidation     *InvalidationReport   `json:"invalidation,omitempty"`
	CacheHitSequence []CacheTrace          `json:"cacheHitSequence"`
	Metrics          RunMetrics            `json:"metrics"`
	Checkpoint       *CheckpointStatus     `json:"checkpoint,omitempty"`
	Recovery         *RecoveryReport       `json:"recovery,omitempty"`
	Tolerances       Tolerances            `json:"tolerances"`
}

type ComparisonMismatch struct {
	Kind        string  `json:"kind"`
	Index       int     `json:"index"`
	ZoneID      string  `json:"zoneId"`
	PixelX      int     `json:"pixelX,omitempty"`
	PixelY      int     `json:"pixelY,omitempty"`
	Incremental float64 `json:"incremental,omitempty"`
	Full        float64 `json:"full,omitempty"`
	Message     string  `json:"message"`
}

type ComparisonResult struct {
	Equal      bool                `json:"equal"`
	PointCount int                 `json:"pointCount"`
	PixelCount int                 `json:"pixelCount"`
	ZoneCount  int                 `json:"zoneCount"`
	Mismatch   *ComparisonMismatch `json:"mismatch,omitempty"`
}

type CheckpointStatus struct {
	SessionID        string   `json:"sessionId"`
	CommittedZones   int      `json:"committedZones"`
	Order            []string `json:"order"`
	AlgorithmVersion string   `json:"algorithmVersion"`
}

type RecoveryReport struct {
	Source             string   `json:"source"`
	SnapshotStatus     string   `json:"snapshotStatus"`
	ResumedZones       []string `json:"resumedZones"`
	Discarded          bool     `json:"discarded"`
	DiscardReason      string   `json:"discardReason"`
	FullRebuild        bool     `json:"fullRebuild"`
	RecoveredFromStore bool     `json:"recoveredFromStore"`
}

type RunMode string

const (
	ModeFull        RunMode = "full"
	ModeIncremental RunMode = "incremental"
)

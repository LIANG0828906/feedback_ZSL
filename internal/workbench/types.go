package workbench

import "encoding/json"

const (
	AlgorithmVersion = "density-reconstruction-v1"
	SchemaVersion    = "1.0"
)

type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Bounds struct {
	MinX float64 `json:"min_x"`
	MinY float64 `json:"min_y"`
	MaxX float64 `json:"max_x"`
	MaxY float64 `json:"max_y"`
}

func (b Bounds) Width() float64  { return b.MaxX - b.MinX }
func (b Bounds) Height() float64 { return b.MaxY - b.MinY }

type TerrainRaster struct {
	CellSize float64 `json:"cell_size"`
	Cols     int     `json:"cols"`
	Rows     int     `json:"rows"`
	// Values are terrain type indexes, row-major from the north-west corner.
	Values []int `json:"values"`
}

type Zone struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Polygon          []Vec2             `json:"polygon"`
	TerrainTypes     []string           `json:"terrain_types"`
	DensityLimit     float64            `json:"density_limit"`
	RarityMix        map[string]float64 `json:"rarity_mix,omitempty"`
	SampleCountLimit int                `json:"sample_count_limit"`
	DependsOn        []string           `json:"depends_on,omitempty"`
}

type NoCollect struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Polygon []Vec2 `json:"polygon"`
}

type SampleSite struct {
	ID              string         `json:"id"`
	X               float64        `json:"x"`
	Y               float64        `json:"y"`
	TerrainType     string         `json:"terrain_type,omitempty"`
	ObservedDensity float64        `json:"observed_density"`
	RarityCounts    map[string]int `json:"rarity_counts"`
}

func (s SampleSite) Point() Vec2 { return Vec2{X: s.X, Y: s.Y} }

type SamplingConfig struct {
	GridSpacing           float64 `json:"grid_spacing"`
	MaxSamples            int     `json:"max_samples"`
	MinPerStratum         int     `json:"min_per_stratum"`
	PerThreadSampleBuffer int     `json:"per_thread_sample_buffer"`
	MaxParallel           int     `json:"max_parallel"`
	DensityStep           float64 `json:"density_step"`
	RarityStep            float64 `json:"rarity_step"`
	DensityTolerance      float64 `json:"density_tolerance"`
	CountTolerance        float64 `json:"count_tolerance"`
	HeatmapTolerance      float64 `json:"heatmap_tolerance"`
	BiasThreshold         float64 `json:"bias_threshold"`
	HeatmapCellSize       float64 `json:"heatmap_cell_size"`
	MaxPointsPerZone      int     `json:"max_points_per_zone"`
}

type ZoneTruth struct {
	Density   float64            `json:"density"`
	RarityMix map[string]float64 `json:"rarity_mix"`
}

type TruthPoint struct {
	ZoneID      string  `json:"zone_id"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	TerrainType string  `json:"terrain_type"`
	Rarity      string  `json:"rarity"`
}

type Truth struct {
	ZoneTruth map[string]ZoneTruth `json:"zone_truth"`
	Points    []TruthPoint         `json:"points"`
}

type Region struct {
	SchemaVersion string         `json:"schema_version"`
	RegionID      string         `json:"region_id"`
	DisplayName   string         `json:"display_name,omitempty"`
	Seed          uint64         `json:"seed"`
	Bounds        Bounds         `json:"bounds"`
	Boundary      []Vec2         `json:"boundary"`
	TerrainTypes  []string       `json:"terrain_types"`
	TerrainRaster TerrainRaster  `json:"terrain_raster"`
	Zones         []Zone         `json:"zones"`
	NoCollect     []NoCollect    `json:"no_collect"`
	Sampling      SamplingConfig `json:"sampling_config"`
	SampleSites   []SampleSite   `json:"sample_sites"`
	Truth         Truth          `json:"truth"`
}

// WorkspaceState contains only user-editable parameters. Immutable sample
// content lives in Region and is hashed separately.
type WorkspaceState struct {
	ZoneParams map[string]ZoneParams `json:"zone_params"`
	NoCollect  []NoCollect           `json:"no_collect"`
}

type ZoneParams struct {
	DensityLimit      float64            `json:"density_limit"`
	RarityMixOverride map[string]float64 `json:"rarity_mix_override,omitempty"`
}

type RuntimeConfig struct {
	MaxParallel int
	BufferLimit int
	Serial      bool
}

type Point struct {
	ID          string  `json:"id"`
	ZoneID      string  `json:"zone_id"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	TerrainType string  `json:"terrain_type"`
	Rarity      string  `json:"rarity"`
}

type Heatmap struct {
	Bounds   Bounds    `json:"bounds"`
	CellSize float64   `json:"cell_size"`
	Cols     int       `json:"cols"`
	Rows     int       `json:"rows"`
	Values   []float64 `json:"values"`
}

func (h Heatmap) At(x, y float64) float64 {
	if h.CellSize <= 0 || h.Cols <= 0 || h.Rows <= 0 {
		return 0
	}
	col := int((x - h.Bounds.MinX) / h.CellSize)
	row := int((y - h.Bounds.MinY) / h.CellSize)
	if col < 0 || col >= h.Cols || row < 0 || row >= h.Rows {
		return 0
	}
	return h.Values[row*h.Cols+col]
}

type SampleSelection struct {
	Indices []int              `json:"indices"`
	SiteIDs []string           `json:"site_ids"`
	Strata  []StratumSelection `json:"strata"`
}

type StratumSelection struct {
	TerrainType string   `json:"terrain_type"`
	Area        float64  `json:"area"`
	SiteIDs     []string `json:"site_ids"`
}

type ZoneEstimate struct {
	Density        float64            `json:"density"`
	RawDensity     float64            `json:"raw_density"`
	DensityLimit   float64            `json:"density_limit"`
	StandardError  float64            `json:"standard_error"`
	Strata         []StratumEstimate  `json:"strata"`
	RarityMix      map[string]float64 `json:"rarity_mix"`
	ObservedRarity map[string]float64 `json:"observed_rarity"`
	RarityResidual float64            `json:"rarity_residual"`
	EffectiveArea  float64            `json:"effective_area"`
	SampleCount    int                `json:"sample_count"`
}

type StratumEstimate struct {
	TerrainType   string  `json:"terrain_type"`
	Area          float64 `json:"area"`
	SampleCount   int     `json:"sample_count"`
	Density       float64 `json:"density"`
	StandardError float64 `json:"standard_error"`
}

type ZoneMetrics struct {
	DensityQuantizationError   float64 `json:"density_quantization_error"`
	StandardError              float64 `json:"standard_error"`
	RarityRoundingResidual     float64 `json:"rarity_rounding_residual"`
	HeatmapReconstructionError float64 `json:"heatmap_reconstruction_error"`
}

type BiasBound struct {
	DensityQuantization float64 `json:"density_quantization"`
	StandardError       float64 `json:"standard_error"`
	RarityRounding      float64 `json:"rarity_rounding"`
	Heatmap             float64 `json:"heatmap"`
}

type ZoneBias struct {
	ZoneID     string      `json:"zone_id"`
	Order      int         `json:"order"`
	Observed   ZoneMetrics `json:"observed"`
	Upper      BiasBound   `json:"upper"`
	Cumulative BiasBound   `json:"cumulative"`
}

type BiasReport struct {
	Threshold           float64    `json:"threshold"`
	Zones               []ZoneBias `json:"zones"`
	TotalUpper          BiasBound  `json:"total_upper"`
	Passed              bool       `json:"passed"`
	FirstViolationZone  string     `json:"first_violation_zone,omitempty"`
	FirstViolationValue float64    `json:"first_violation_value,omitempty"`
}

type ZoneResult struct {
	ZoneID          string          `json:"zone_id"`
	TopologyOrder   int             `json:"topology_order"`
	SampleSelection SampleSelection `json:"sample_selection"`
	Estimate        ZoneEstimate    `json:"estimate"`
	Points          []Point         `json:"points"`
	PointCount      int             `json:"point_count"`
	PointDigest     string          `json:"point_digest"`
	HeatLayer       map[int]float64 `json:"heat_layer"`
	HeatDigest      string          `json:"heat_digest"`
	Metrics         ZoneMetrics     `json:"metrics"`
	ParamVersion    string          `json:"param_version"`
	Generation      int             `json:"generation"`
	CommitSeq       int64           `json:"commit_seq"`
}

type CacheState string

const (
	CacheRecomputed CacheState = "recomputed"
	CacheReused     CacheState = "reused"
	CacheMissing    CacheState = "missing"
)

type InvalidationEntry struct {
	ZoneID      string     `json:"zone_id"`
	Stage       string     `json:"stage"`
	Invalidated bool       `json:"invalidated"`
	Reused      bool       `json:"reused"`
	Downstream  bool       `json:"downstream"`
	Reasons     []string   `json:"reasons"`
	CacheState  CacheState `json:"cache_state"`
}

type DependencyNode struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	ZoneID  string   `json:"zone_id,omitempty"`
	Depends []string `json:"depends_on"`
}

type DependencyGraph struct {
	Nodes         []DependencyNode `json:"nodes"`
	TopologyOrder []string         `json:"topology_order"`
}

type BufferEvent struct {
	ZoneID  string `json:"zone_id"`
	Worker  int    `json:"worker"`
	Reason  string `json:"reason"`
	Blocked bool   `json:"blocked"`
	Count   int    `json:"count"`
}

type WorkerStats struct {
	MaxParallel  int           `json:"max_parallel"`
	BufferLimit  int           `json:"buffer_limit"`
	WorkersUsed  int           `json:"workers_used"`
	BufferEvents []BufferEvent `json:"buffer_events"`
}

type RunResult struct {
	RegionID      string              `json:"region_id"`
	Mode          string              `json:"mode"`
	BaseHash      string              `json:"base_hash"`
	StateHash     string              `json:"state_hash"`
	Algorithm     string              `json:"algorithm_version"`
	CommitOrder   []string            `json:"commit_order"`
	Recomputed    []string            `json:"recomputed"`
	Invalidations []InvalidationEntry `json:"invalidations"`
	Dependency    DependencyGraph     `json:"dependency_graph"`
	Zones         []*ZoneResult       `json:"zones"`
	Points        []Point             `json:"points"`
	Heatmap       Heatmap             `json:"heatmap"`
	Bias          BiasReport          `json:"bias_report"`
	Worker        WorkerStats         `json:"worker_stats"`
	Snapshot      SnapshotStatus      `json:"snapshot"`
	Warnings      []string            `json:"warnings,omitempty"`
}

type SnapshotStatus struct {
	Enabled      bool   `json:"enabled"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	Restored     bool   `json:"restored"`
	Invalidated  bool   `json:"invalidated"`
	RestoreError string `json:"restore_error,omitempty"`
	Committed    int    `json:"committed"`
}

type SnapshotMeta struct {
	SnapshotID       string `json:"snapshot_id"`
	RegionID         string `json:"region_id"`
	BaseHash         string `json:"base_hash"`
	AlgorithmVersion string `json:"algorithm_version"`
	StateJSON        string `json:"state_json"`
	StateHash        string `json:"state_hash"`
	UpdatedAt        string `json:"updated_at"`
}

type ZoneCheckpoint struct {
	ZoneID           string `json:"zone_id"`
	TopologyOrder    int    `json:"topology_order"`
	Generation       int    `json:"generation"`
	CommitSeq        int64  `json:"commit_seq"`
	ContentHash      string `json:"content_hash"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	SampleIndices    string `json:"sample_indices"`
	EstimateJSON     string `json:"estimate"`
	PointSummary     string `json:"point_summary"`
	HeatSummary      string `json:"heat_summary"`
	ResultJSON       string `json:"result"`
	IsCurrent        bool   `json:"is_current"`
}

type PersistentStore interface {
	LoadSnapshot(snapshotID string) (*SnapshotMeta, []ZoneCheckpoint, error)
	SaveSnapshot(meta SnapshotMeta) error
	DeleteSnapshot(snapshotID string) error
	SaveState(snapshotID, stateJSON, stateHash string) error
	CommitZone(snapshotID, stateJSON, stateHash string, checkpoint ZoneCheckpoint) (ZoneCheckpoint, error)
	Close() error
}

type CompareReport struct {
	Equal            bool     `json:"equal"`
	FirstZoneID      string   `json:"first_zone_id,omitempty"`
	FirstDataItem    string   `json:"first_data_item,omitempty"`
	IncrementalOrder []string `json:"incremental_commit_order"`
	FullOrder        []string `json:"full_commit_order"`
	CheckedItems     int      `json:"checked_items"`
}

type ZoneEditRequest struct {
	ZoneID              string             `json:"zone_id"`
	DensityLimit        *float64           `json:"density_limit,omitempty"`
	RarityMix           map[string]float64 `json:"rarity_mix,omitempty"`
	ClearRarityOverride bool               `json:"clear_rarity_override,omitempty"`
}

type NoCollectEditRequest struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Polygon []Vec2 `json:"polygon"`
	Remove  bool   `json:"remove,omitempty"`
}

type View struct {
	RegionID      string              `json:"region_id"`
	DisplayName   string              `json:"display_name"`
	Bounds        Bounds              `json:"bounds"`
	Boundary      []Vec2              `json:"boundary"`
	Zones         []ZoneView          `json:"zones"`
	NoCollect     []NoCollect         `json:"no_collect"`
	Terrain       TerrainRaster       `json:"terrain_raster"`
	TerrainTypes  []string            `json:"terrain_types"`
	SampleSites   []SampleSite        `json:"sample_sites"`
	Heatmap       Heatmap             `json:"heatmap"`
	Points        []Point             `json:"points"`
	Bias          BiasReport          `json:"bias_report"`
	Invalidations []InvalidationEntry `json:"invalidations"`
	CommitOrder   []string            `json:"commit_order"`
	Worker        WorkerStats         `json:"worker_stats"`
	Dependency    DependencyGraph     `json:"dependency_graph"`
	Snapshot      SnapshotStatus      `json:"snapshot"`
	Mode          string              `json:"mode"`
}

type ZoneView struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Polygon          []Vec2             `json:"polygon"`
	TerrainTypes     []string           `json:"terrain_types"`
	DensityLimit     float64            `json:"density_limit"`
	RarityMix        map[string]float64 `json:"rarity_mix"`
	RarityOverride   map[string]float64 `json:"rarity_override,omitempty"`
	DependsOn        []string           `json:"depends_on,omitempty"`
	EstimatedDensity float64            `json:"estimated_density"`
	StandardError    float64            `json:"standard_error"`
	SampleCount      int                `json:"sample_count"`
	EffectiveArea    float64            `json:"effective_area"`
	PointCount       int                `json:"point_count"`
	PointDigest      string             `json:"point_digest"`
	HeatDigest       string             `json:"heat_digest"`
	CacheState       CacheState         `json:"cache_state"`
	ParamVersion     string             `json:"param_version"`
	Generation       int                `json:"generation"`
}

func cloneJSON[T any](v T) T {
	data, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(data, &out)
	return out
}

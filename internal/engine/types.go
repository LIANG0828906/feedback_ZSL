package engine

import (
	"volumediag/internal/model"
	"volumediag/internal/store"
	"volumediag/internal/volume"
)

type Mode string

const (
	ModeFull        Mode = "full"
	ModeIncremental Mode = "incremental"
	ModePreview     Mode = "preview"
	ModeResume      Mode = "resume"
)

type RenderOptions struct {
	PauseAfterBatches int  `json:"pause_after_batches,omitempty"`
	ForceFull         bool `json:"force_full,omitempty"`
}

type Invalidation struct {
	CameraID         string   `json:"camera_id"`
	BlockX           int      `json:"block_x"`
	BlockY           int      `json:"block_y"`
	Stage            string   `json:"stage"`
	Depth            int      `json:"impact_depth"`
	Reason           string   `json:"reason"`
	SourceLights     []string `json:"source_lights,omitempty"`
	SourceOccluders  []string `json:"source_occluders,omitempty"`
	PreviouslyCached bool     `json:"previously_cached"`
}

type BlockFailure struct {
	CameraID  string `json:"camera_id"`
	BlockX    int    `json:"block_x"`
	BlockY    int    `json:"block_y"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type RenderReport struct {
	Mode             Mode              `json:"mode"`
	Preview          bool              `json:"preview"`
	StartedAt        string            `json:"started_at"`
	DurationMS       float64           `json:"duration_ms"`
	RequestedBlocks  int               `json:"requested_blocks"`
	ComputedBlocks   int               `json:"computed_blocks"`
	ReusedBlocks     int               `json:"reused_blocks"`
	BatchesCommitted int               `json:"batches_committed"`
	BufferWaitCount  int               `json:"buffer_wait_count"`
	BufferEvents     []string          `json:"buffer_events,omitempty"`
	Paused           bool              `json:"paused"`
	Complete         bool              `json:"complete"`
	RestoredBlocks   int               `json:"restored_blocks"`
	SnapshotInvalid  bool              `json:"snapshot_invalidated"`
	SnapshotReason   string            `json:"snapshot_reason,omitempty"`
	Affected         []Invalidation    `json:"affected"`
	ReusedBlockKeys  []volume.BlockKey `json:"reused_block_keys,omitempty"`
	Failures         []BlockFailure    `json:"failures,omitempty"`
	Quality          volume.Quality    `json:"quality"`
}

type CompareReport struct {
	Equal            bool    `json:"equal"`
	BlocksCompared   int     `json:"blocks_compared"`
	PixelsCompared   int     `json:"pixels_compared"`
	FirstMismatch    string  `json:"first_mismatch,omitempty"`
	FirstBlockX      int     `json:"first_block_x,omitempty"`
	FirstBlockY      int     `json:"first_block_y,omitempty"`
	PixelX           int     `json:"pixel_x,omitempty"`
	PixelY           int     `json:"pixel_y,omitempty"`
	Item             string  `json:"item,omitempty"`
	Left             string  `json:"left,omitempty"`
	Right            string  `json:"right,omitempty"`
	MaxAbsVolumeDiff float64 `json:"max_abs_volume_diff"`
	MaxAbsStatDiff   float64 `json:"max_abs_stat_diff"`
	MaxAbsPixelDiff  float64 `json:"max_abs_pixel_diff"`
}

type TruthReport struct {
	Equal                 bool    `json:"equal"`
	PixelsCompared        int     `json:"pixels_compared"`
	RaysCompared          int     `json:"rays_compared"`
	MaxPixelError         float64 `json:"max_pixel_error"`
	MaxScatterError       float64 `json:"max_scatter_error"`
	MaxTransmittanceError float64 `json:"max_transmittance_error"`
	PixelTolerance        float64 `json:"pixel_tolerance"`
	ScatterTolerance      float64 `json:"scatter_tolerance"`
}

type BlockView struct {
	CameraID        string   `json:"camera_id"`
	BlockX          int      `json:"block_x"`
	BlockY          int      `json:"block_y"`
	State           string   `json:"state"`
	CacheHit        bool     `json:"cache_hit"`
	Invalidated     bool     `json:"invalidated"`
	Reasons         []string `json:"reasons,omitempty"`
	SourceLights    []string `json:"source_lights,omitempty"`
	SourceOccluders []string `json:"source_occluders,omitempty"`
	UsedLights      []string `json:"used_lights,omitempty"`
	Digest          string   `json:"digest,omitempty"`
	Samples         int      `json:"samples"`
	ActualStep      float64  `json:"actual_step"`
	DurationMS      float64  `json:"duration_ms"`
	Failed          bool     `json:"failed"`
}

type State struct {
	SourceHash     string            `json:"source_hash"`
	SceneHash      string            `json:"scene_hash"`
	SceneVersions  model.Versions    `json:"scene_versions"`
	Params         volume.Params     `json:"params"`
	Quality        volume.Quality    `json:"quality"`
	Viewport       model.Viewport    `json:"viewport"`
	BlockSize      model.BlockSize   `json:"block_size"`
	CameraPath     []model.Camera    `json:"camera_path"`
	ActiveCameraID string            `json:"active_camera_id"`
	Geometry       []model.DepthMesh `json:"geometry"`
	Lights         []model.Light     `json:"lights"`
	Medium         model.Medium      `json:"medium"`
	Blocks         []BlockView       `json:"blocks"`
	CacheCount     int               `json:"cache_count"`
	TotalBlocks    int               `json:"total_blocks"`
	PendingCount   int               `json:"pending_count"`
	FailedCount    int               `json:"failed_count"`
	Complete       bool              `json:"complete"`
	Paused         bool              `json:"paused"`
	Restored       store.Restore     `json:"-"`
	RestoreSummary string            `json:"restore_summary"`
	DBPath         string            `json:"db_path,omitempty"`
	Checkpoint     store.Counts      `json:"checkpoint"`
}

type ImageData struct {
	CameraID  string    `json:"camera_id"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Volume    []float64 `json:"volume"`
	Composite []float64 `json:"composite"`
	TruthDiff []float64 `json:"truth_diff"`
	Base      []float64 `json:"base"`
}

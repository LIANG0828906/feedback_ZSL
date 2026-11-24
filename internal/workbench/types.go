package workbench

import (
	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
	"occlusionlab/internal/streaming"
	"occlusionlab/internal/verify"
)

type ProjectSummary struct {
	Meta        model.ProjectMeta `json:"meta"`
	ContentHash string            `json:"content_hash"`
	World       model.World       `json:"world"`
	Truth       model.Truth       `json:"truth"`
}

type WindowSummary struct {
	Index         int                         `json:"index"`
	Time          int64                       `json:"time"`
	State         streaming.WindowState       `json:"state"`
	Visible       []int                       `json:"visible"`
	Occluded      []int                       `json:"occluded"`
	Uncertain     []int                       `json:"uncertain"`
	Resident      []int                       `json:"resident"`
	Loaded        []int                       `json:"loaded"`
	Unloaded      []int                       `json:"unloaded"`
	CameraOffset  int64                       `json:"camera_offset"`
	ParamsVersion int                         `json:"params_version"`
	Error         streaming.FrameErrorSummary `json:"error"`
	LastError     *model.AppError             `json:"last_error,omitempty"`
}

type WindowDetail struct {
	Window        int                         `json:"window"`
	Time          int64                       `json:"time"`
	State         streaming.WindowState       `json:"state"`
	Frozen        bool                        `json:"frozen"`
	Preview       bool                        `json:"preview"`
	Visible       []int                       `json:"visible"`
	Occluded      []int                       `json:"occluded"`
	Uncertain     []int                       `json:"uncertain"`
	Resident      []int                       `json:"resident"`
	Loaded        []int                       `json:"loaded"`
	Unloaded      []int                       `json:"unloaded"`
	References    []streaming.ChunkReference  `json:"references"`
	Chunks        []geometry.ChunkVerdict     `json:"chunks"`
	Error         streaming.FrameErrorSummary `json:"error"`
	Truth         *model.TruthSample          `json:"truth,omitempty"`
	Camera        model.CameraSample          `json:"camera"`
	LastError     *model.AppError             `json:"last_error,omitempty"`
	ParamsHash    string                      `json:"params_hash"`
	ParamsVersion int                         `json:"params_version"`
	CameraOffset  int64                       `json:"camera_offset"`
}

type InvalidWindow struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Kind   string `json:"kind"`
}

type ErrorPoint struct {
	Window                int   `json:"window"`
	Time                  int64 `json:"time"`
	Committed             bool  `json:"committed"`
	ProjectionResidualQ   int64 `json:"projection_residual_q"`
	ProjectionErrorQ      int64 `json:"projection_error_q"`
	PlaneErrorWorldQ      int64 `json:"plane_error_world_q"`
	CoverageErrorQ        int64 `json:"coverage_error_q"`
	DepthErrorWorldQ      int64 `json:"depth_error_world_q"`
	CumulativeUpperBoundQ int64 `json:"cumulative_upper_bound_q"`
}

type SnapshotInfo struct {
	Path        string `json:"path"`
	Count       int    `json:"count"`
	Invalidated bool   `json:"invalidated"`
	Reason      string `json:"reason,omitempty"`
	LastWindow  int    `json:"last_window"`
}

type LogEntry struct {
	At      string `json:"at"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type Dashboard struct {
	AppVersion     string                  `json:"app_version"`
	Project        ProjectSummary          `json:"project"`
	Params         model.Params            `json:"params"`
	Progress       Progress                `json:"progress"`
	Windows        []WindowSummary         `json:"windows"`
	SelectedWindow int                     `json:"selected_window"`
	Selected       *WindowDetail           `json:"selected,omitempty"`
	ErrorSeries    []ErrorPoint            `json:"error_series"`
	InvalidWindows []InvalidWindow         `json:"invalid_windows"`
	Snapshot       SnapshotInfo            `json:"snapshot"`
	BudgetEvents   []streaming.BudgetEvent `json:"budget_events"`
	Verification   *verify.Report          `json:"verification,omitempty"`
	Paused         bool                    `json:"paused"`
	Logs           []LogEntry              `json:"logs"`
	CanStep        bool                    `json:"can_step"`
	CanRetry       bool                    `json:"can_retry"`
}

type Progress struct {
	Committed int `json:"committed"`
	Failed    int `json:"failed"`
	Total     int `json:"total"`
}

type GenerateOptions struct {
	Seed             int64   `json:"seed"`
	WorldFracBits    uint8   `json:"world_frac_bits"`
	BasisFracBits    uint8   `json:"basis_frac_bits"`
	ScreenFracBits   uint8   `json:"screen_frac_bits"`
	ParamFracBits    uint8   `json:"param_frac_bits"`
	RasterResolution int     `json:"raster_resolution"`
	MaxWorkers       int     `json:"max_workers"`
	MaxResident      int     `json:"max_resident"`
	TolerancePx      float64 `json:"tolerance_px"`
}

type RuntimeOptions struct {
	RasterResolution  int     `json:"raster_resolution"`
	MaxWorkers        int     `json:"max_workers"`
	RasterBufferCells int     `json:"raster_buffer_cells"`
	MaxResident       int     `json:"max_resident"`
	LoadCostBudget    int     `json:"load_cost_budget"`
	TolerancePx       float64 `json:"tolerance_px"`
}

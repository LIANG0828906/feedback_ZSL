package streaming

import (
	"encoding/json"
	"fmt"
	"time"

	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
)

type WindowState string

const (
	StatePending   WindowState = "pending"
	StateCulling   WindowState = "culling"
	StateLoading   WindowState = "loading"
	StateCommitted WindowState = "committed"
	StateFailed    WindowState = "failed"
)

var allowedTransitions = map[WindowState]map[WindowState]bool{
	StatePending:   {StateCulling: true, StateFailed: true},
	StateCulling:   {StateLoading: true, StateFailed: true},
	StateLoading:   {StateCommitted: true, StateFailed: true},
	StateCommitted: {},
	StateFailed:    {StatePending: true},
}

func CanTransition(from, to WindowState) bool {
	return allowedTransitions[from][to]
}

type TransitionEvent struct {
	From WindowState `json:"from"`
	To   WindowState `json:"to"`
	At   string      `json:"at"`
}

type Window struct {
	Index     int               `json:"index"`
	State     WindowState       `json:"state"`
	LastError *model.AppError   `json:"last_error,omitempty"`
	Frame     *WindowFrame      `json:"frame,omitempty"`
	History   []TransitionEvent `json:"history"`
}

func NewWindow(index int) *Window {
	return &Window{
		Index: index,
		State: StatePending,
		History: []TransitionEvent{{
			From: "", To: StatePending, At: time.Now().UTC().Format(time.RFC3339Nano),
		}},
	}
}

func (w *Window) Transition(to WindowState) *model.AppError {
	if w == nil {
		return model.StateError(0, "窗口不存在")
	}
	if !CanTransition(w.State, to) {
		return model.StateError(w.Index,
			fmt.Sprintf("非法窗口状态迁移 %s -> %s；原状态保持不变", w.State, to))
	}
	w.History = append(w.History, TransitionEvent{
		From: w.State, To: to, At: time.Now().UTC().Format(time.RFC3339Nano),
	})
	w.State = to
	if to != StateFailed {
		w.LastError = nil
	}
	return nil
}

func (w *Window) Freeze(frame *WindowFrame) *model.AppError {
	if w == nil {
		return model.StateError(0, "窗口不存在")
	}
	if w.State != StateLoading {
		return model.StateError(w.Index, "仅加载中的窗口可以提交")
	}
	frozen := cloneFrame(frame)
	w.Frame = frozen
	w.State = StateCommitted
	w.History = append(w.History, TransitionEvent{
		From: StateLoading, To: StateCommitted, At: time.Now().UTC().Format(time.RFC3339Nano),
	})
	return nil
}

type ChunkReference struct {
	Chunk    int    `json:"chunk"`
	Status   string `json:"status"`
	Occluder string `json:"occluder,omitempty"`
}

type FrameErrorSummary struct {
	ProjectionResidualQ     int64 `json:"projection_residual_q"`
	ProjectionErrorQ        int64 `json:"projection_error_q"`
	FrustumPlaneErrorWorldQ int64 `json:"frustum_plane_error_world_q"`
	CoverageErrorQ          int64 `json:"coverage_error_q"`
	DepthCompareErrorWorldQ int64 `json:"depth_compare_error_world_q"`
	CumulativeUpperBoundQ   int64 `json:"cumulative_upper_bound_q"`
}

type WindowFrame struct {
	Window          int                     `json:"window"`
	Time            int64                   `json:"time"`
	CameraOffset    int64                   `json:"camera_offset"`
	ParamsHash      string                  `json:"params_hash"`
	ParamsVersion   int                     `json:"params_version"`
	SceneHash       string                  `json:"scene_hash"`
	Visible         []int                   `json:"visible"`
	Occluded        []int                   `json:"occluded"`
	Uncertain       []int                   `json:"uncertain"`
	Resident        []int                   `json:"resident"`
	Loaded          []int                   `json:"loaded"`
	Unloaded        []int                   `json:"unloaded"`
	References      []ChunkReference        `json:"references"`
	Chunks          []geometry.ChunkVerdict `json:"chunks"`
	VisibleDigest   string                  `json:"visible_digest"`
	ResultHash      string                  `json:"result_hash"`
	Error           FrameErrorSummary       `json:"error"`
	LoadCost        int                     `json:"load_cost"`
	RasterCellsWork int                     `json:"raster_cells_work"`
}

func (f *WindowFrame) computeDigests() {
	f.VisibleDigest = model.CanonicalHash(f.Visible)
	copy := *f
	copy.ResultHash = ""
	f.ResultHash = model.CanonicalHash(copy)
}

func cloneFrame(frame *WindowFrame) *WindowFrame {
	if frame == nil {
		return nil
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return nil
	}
	var out WindowFrame
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return &out
}

type Snapshot struct {
	Window        int          `json:"window"`
	Frame         *WindowFrame `json:"frame"`
	Params        model.Params `json:"params"`
	SceneHash     string       `json:"scene_hash"`
	ParamsHash    string       `json:"params_hash"`
	ParamsVersion int          `json:"params_version"`
}

type SnapshotSink interface {
	AppendSnapshot(snapshot Snapshot) error
}

type BudgetEvent struct {
	Window int    `json:"window"`
	Chunk  int    `json:"chunk"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

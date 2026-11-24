package pipeline

import (
	"fmt"
	"sync"
	"time"
)

type WindowState string

const (
	WindowPending     WindowState = "pending"
	WindowCompositing WindowState = "compositing"
	WindowCommitted   WindowState = "committed"
	WindowFailed      WindowState = "failed"
)

type SourceSnapshot struct {
	Versions []int    `json:"versions"`
	TimeBase TimeBase `json:"time_base"`
	Canvas   Canvas   `json:"canvas"`
}

type WindowSnapshot struct {
	WindowIndex       int            `json:"window_index"`
	StartFrame        int            `json:"start_frame"`
	EndFrame          int            `json:"end_frame"`
	Layers            []Layer        `json:"layers"`
	Source            SourceSnapshot `json:"source"`
	ModelHash         string         `json:"model_hash"`
	SourceHash        string         `json:"source_hash"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	FrameHash         string         `json:"frame_hash"`
	FramePath         string         `json:"frame_path"`
	Summary           FrameSummary   `json:"summary"`
	FrameSummaries    []FrameSummary `json:"frame_summaries,omitempty"`
	CommittedSequence int64          `json:"committed_sequence"`
	CommittedAt       string         `json:"committed_at"`
}

type Window struct {
	Index        int             `json:"index"`
	StartFrame   int             `json:"start_frame"`
	EndFrame     int             `json:"end_frame"`
	State        WindowState     `json:"state"`
	Snapshot     *WindowSnapshot `json:"snapshot,omitempty"`
	Failure      string          `json:"failure,omitempty"`
	Invalidated  bool            `json:"invalidated"`
	Reused       bool            `json:"reused"`
	Invalidation *Invalidation   `json:"invalidation,omitempty"`
}

type Invalidation struct {
	Window   int      `json:"window"`
	LayerIDs []string `json:"layer_ids"`
	Reason   string   `json:"reason"`
	Stage    string   `json:"stage"`
}

type InvalidationList struct {
	Operation    string         `json:"operation"`
	Affected     []Invalidation `json:"affected"`
	Reused       []int          `json:"reused"`
	TotalWindows int            `json:"total_windows"`
	TotalFrames  int            `json:"total_frames"`
}

type WindowSet struct {
	mu      sync.RWMutex
	windows []*Window
}

func NewWindowSet(frameCount, windowSize int) (*WindowSet, error) {
	if frameCount <= 0 {
		return nil, paraError(-1, "frame count must be positive")
	}
	if windowSize <= 0 {
		return nil, paraError(-1, "window size must be positive")
	}
	if windowSize > frameCount {
		windowSize = frameCount
	}
	count := (frameCount + windowSize - 1) / windowSize
	ws := &WindowSet{windows: make([]*Window, 0, count)}
	for i := 0; i < count; i++ {
		start := i * windowSize
		end := start + windowSize
		if end > frameCount {
			end = frameCount
		}
		ws.windows = append(ws.windows, &Window{
			Index: i, StartFrame: start, EndFrame: end, State: WindowPending,
		})
	}
	return ws, nil
}

func (ws *WindowSet) Len() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.windows)
}

func (ws *WindowSet) Get(index int) (Window, error) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	if index < 0 || index >= len(ws.windows) {
		return Window{}, stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	return cloneWindow(*ws.windows[index]), nil
}

func (ws *WindowSet) Windows() []Window {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make([]Window, len(ws.windows))
	for i, window := range ws.windows {
		out[i] = cloneWindow(*window)
	}
	return out
}

func (ws *WindowSet) Transition(index int, to WindowState) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	from := window.State
	if !legalTransition(from, to) {
		return stateError(index, "illegal window transition %s -> %s", from, to)
	}
	window.State = to
	if to != WindowFailed {
		window.Failure = ""
	}
	if to == WindowPending {
		window.Snapshot = nil
	}
	return nil
}

func legalTransition(from, to WindowState) bool {
	switch from {
	case WindowPending:
		return to == WindowCompositing
	case WindowCompositing:
		return to == WindowCommitted || to == WindowFailed
	case WindowFailed:
		return to == WindowCompositing
	case WindowCommitted:
		return to == WindowPending
	default:
		return false
	}
}

func (ws *WindowSet) Begin(index int) (Window, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return Window{}, stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if !legalTransition(window.State, WindowCompositing) {
		return *window, stateError(index, "cannot begin window in state %s", window.State)
	}
	window.State = WindowCompositing
	window.Failure = ""
	return *window, nil
}

func (ws *WindowSet) Commit(index int, snapshot WindowSnapshot, seq int64) (WindowSnapshot, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return WindowSnapshot{}, stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if window.State != WindowCompositing {
		return WindowSnapshot{}, stateError(index, "cannot commit window in state %s", window.State)
	}
	if snapshot.WindowIndex != index || snapshot.StartFrame != window.StartFrame || snapshot.EndFrame != window.EndFrame {
		return WindowSnapshot{}, stateError(index, "window snapshot bounds do not match the target window")
	}
	if snapshot.AlgorithmVersion == "" || snapshot.SourceHash == "" || snapshot.ModelHash == "" || snapshot.FrameHash == "" {
		return WindowSnapshot{}, stateError(index, "window snapshot is incomplete")
	}
	snapshot.CommittedSequence = seq
	if snapshot.CommittedAt == "" {
		snapshot.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	locked := cloneSnapshot(snapshot)
	window.Snapshot = &locked
	window.State = WindowCommitted
	window.Failure = ""
	window.Invalidated = false
	window.Invalidation = nil
	return snapshot, nil
}

func (ws *WindowSet) Fail(index int, cause error) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if window.State != WindowCompositing {
		return stateError(index, "cannot fail window in state %s", window.State)
	}
	window.State = WindowFailed
	window.Snapshot = nil
	if cause != nil {
		window.Failure = cause.Error()
	} else {
		window.Failure = "window processing failed"
	}
	return nil
}

func (ws *WindowSet) Retry(index int) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if window.State != WindowFailed {
		return stateError(index, "only failed windows can be retried, got %s", window.State)
	}
	window.State = WindowCompositing
	window.Failure = ""
	return nil
}

func (ws *WindowSet) Invalidate(index int, invalidation Invalidation) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if window.State == WindowCompositing {
		return stateError(index, "cannot invalidate a window while compositing")
	}
	window.State = WindowPending
	window.Snapshot = nil
	window.Failure = ""
	window.Invalidated = true
	window.Reused = false
	invalidation.Window = index
	window.Invalidation = &invalidation
	return nil
}

func (ws *WindowSet) MarkReused(index int) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if window.State != WindowCommitted {
		return stateError(index, "only committed windows can be reused, got %s", window.State)
	}
	window.Reused = true
	window.Invalidated = false
	window.Invalidation = nil
	return nil
}

func (ws *WindowSet) Restore(index int, snapshot WindowSnapshot) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	if snapshot.WindowIndex != index || snapshot.StartFrame != window.StartFrame || snapshot.EndFrame != window.EndFrame {
		return stateError(index, "persisted snapshot bounds do not match window")
	}
	window.State = WindowCommitted
	locked := cloneSnapshot(snapshot)
	window.Snapshot = &locked
	window.Failure = ""
	window.Invalidated = false
	window.Reused = true
	return nil
}

func (ws *WindowSet) Reset(index int) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if index < 0 || index >= len(ws.windows) {
		return stateError(index, "window index is outside [0,%d)", len(ws.windows))
	}
	window := ws.windows[index]
	window.State = WindowPending
	window.Snapshot = nil
	window.Failure = ""
	window.Invalidated = false
	window.Reused = false
	window.Invalidation = nil
	return nil
}

func (ws *WindowSet) Progress() (committed, total int) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	for _, window := range ws.windows {
		if window.State == WindowCommitted {
			committed++
		}
	}
	return committed, len(ws.windows)
}

func (ws *WindowSet) CommittedSnapshots() []WindowSnapshot {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make([]WindowSnapshot, 0, len(ws.windows))
	for _, window := range ws.windows {
		if window.State == WindowCommitted && window.Snapshot != nil {
			out = append(out, cloneSnapshot(*window.Snapshot))
		}
	}
	return out
}

func (ws *WindowSet) StateMatrix() []string {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make([]string, len(ws.windows))
	for i, window := range ws.windows {
		state := string(window.State)
		if window.Invalidated {
			state += ":invalid"
		} else if window.Reused {
			state += ":reused"
		}
		out[i] = state
	}
	return out
}

func (ws *WindowSet) WindowForFrame(frame int) (int, error) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	for _, window := range ws.windows {
		if frame >= window.StartFrame && frame < window.EndFrame {
			return window.Index, nil
		}
	}
	return 0, paraError(frame, "frame is outside all windows")
}

func (ws *WindowSet) String() string {
	committed, total := ws.Progress()
	return fmt.Sprintf("%d/%d windows committed", committed, total)
}

func cloneWindow(window Window) Window {
	out := window
	if window.Snapshot != nil {
		snapshot := cloneSnapshot(*window.Snapshot)
		out.Snapshot = &snapshot
	}
	if window.Invalidation != nil {
		invalidation := *window.Invalidation
		invalidation.LayerIDs = append([]string(nil), window.Invalidation.LayerIDs...)
		out.Invalidation = &invalidation
	}
	return out
}

func cloneSnapshot(snapshot WindowSnapshot) WindowSnapshot {
	out := snapshot
	out.Layers = append([]Layer(nil), snapshot.Layers...)
	out.FrameSummaries = append([]FrameSummary(nil), snapshot.FrameSummaries...)
	out.Source.Versions = append([]int(nil), snapshot.Source.Versions...)
	return out
}

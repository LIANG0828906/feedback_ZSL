package workbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	stdcolor "image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"hdrproof/internal/apperr"
	"hdrproof/internal/audit"
	"hdrproof/internal/color"
	"hdrproof/internal/compare"
	"hdrproof/internal/engine"
	"hdrproof/internal/exporter"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
	"hdrproof/internal/snapshot"
	"hdrproof/internal/state"
)

type App struct {
	mu      sync.Mutex
	ctx     context.Context
	root    string
	samples string
	db      string
	params  model.Params
	workers int
	buffer  int
	last    *engine.RunResult
	lastErr *apperr.Info
	events  []engine.Event
	cancel  context.CancelFunc
	running bool
}

func New() *App {
	return &App{
		ctx: context.Background(), params: model.DefaultParams(),
		workers: 4, buffer: 2,
	}
}

func (a *App) Startup(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctx = ctx
	if a.root == "" {
		a.root = detectRoot()
		a.samples = filepath.Join(a.root, "samples")
		a.db = filepath.Join(a.root, "full.db")
	}
}

type ResourceView struct {
	MaxWorkers        int    `json:"max_workers"`
	FrameBufferLimit  int    `json:"frame_buffer_limit_per_worker"`
	BufferBlocks      int    `json:"buffer_blocks"`
	LastBlockedReason string `json:"last_blocked_reason,omitempty"`
	Running           bool   `json:"running"`
	EventCount        int    `json:"event_count"`
}

type SegmentView struct {
	Index      int                `json:"index"`
	Phase      state.SegmentPhase `json:"phase"`
	FrameStart int                `json:"frame_start"`
	FrameEnd   int                `json:"frame_end"`
	Error      string             `json:"error,omitempty"`
}

type RouteView struct {
	Route           model.Route   `json:"route"`
	Phase           string        `json:"phase"`
	FramesCommitted int           `json:"frames_committed"`
	Segments        []SegmentView `json:"segments"`
}

type WorkspaceView struct {
	Root             string            `json:"root"`
	SamplesDir       string            `json:"samples_dir"`
	DBPath           string            `json:"db_path"`
	TruthFingerprint string            `json:"truth_fingerprint"`
	Params           model.Params      `json:"params"`
	MaxWorkers       int               `json:"max_workers"`
	FrameBufferLimit int               `json:"frame_buffer_limit_per_worker"`
	Routes           []RouteView       `json:"routes"`
	Progress         state.Progress    `json:"progress"`
	LastRun          *engine.RunResult `json:"last_run,omitempty"`
	LastError        *apperr.Info      `json:"last_error,omitempty"`
	Resource         ResourceView      `json:"resource"`
}

func (a *App) Workspace() WorkspaceView {
	a.mu.Lock()
	root, samples, db := a.root, a.samples, a.db
	params, workers, buffer := a.params, a.workers, a.buffer
	last, lastErr := a.last, a.lastErr
	events := append([]engine.Event(nil), a.events...)
	running := a.running
	a.mu.Unlock()
	view, _ := buildWorkspace(root, samples, db, params, workers, buffer, last, lastErr, events, running)
	return view
}

func buildWorkspace(root, samples, db string, params model.Params, workers, buffer int,
	last *engine.RunResult, lastErr *apperr.Info, events []engine.Event, running bool) (WorkspaceView, error) {
	view := WorkspaceView{
		Root: root, SamplesDir: samples, DBPath: db, Params: params,
		MaxWorkers: workers, FrameBufferLimit: buffer, LastRun: last, LastError: lastErr,
		Resource: ResourceView{MaxWorkers: workers, FrameBufferLimit: buffer,
			BufferBlocks: countBlocks(events), Running: running, EventCount: len(events)},
	}
	if running {
		view.Resource.Running = true
	}
	if len(events) > 0 {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Kind == "frame_buffer_blocked" {
				view.Resource.LastBlockedReason = events[i].Reason + ": " + events[i].Message
				break
			}
		}
	}
	if last != nil {
		view.Progress = last.Progress
		view.Resource.BufferBlocks = last.BufferBlocks
	}
	ds, err := sample.LoadMetadata(samples)
	if err != nil {
		if view.Resource.Running {
			view.Resource.Running = false
		}
		view.LastError = infoFromError(err)
		return view, err
	}
	view.TruthFingerprint = ds.TruthFingerprint
	committedSegments, frameCounts := committed(db)
	errBySegment := make(map[string]string)
	if lastErr != nil && lastErr.Segment >= 0 {
		errBySegment[fmt.Sprintf("%s/%d", lastErr.RouteID, lastErr.Segment)] = lastErr.Message
	}
	if last != nil {
		for _, info := range last.Errors {
			if info.RouteID != "" && info.Segment >= 0 {
				errBySegment[fmt.Sprintf("%s/%d", info.RouteID, info.Segment)] = info.Message
			}
		}
	}
	for _, rd := range ds.Routes {
		rv := RouteView{Route: rd.Route}
		count := model.SegmentCount(rd.Route)
		committedCount := 0
		for seg := 0; seg < count; seg++ {
			indices := model.FrameIndicesForSegment(rd.Route, seg)
			if len(indices) == 0 {
				continue
			}
			phase := state.SegmentPending
			if _, ok := committedSegments[snapshot.SegmentKey{RouteID: rd.Route.ID, Segment: seg}]; ok {
				phase = state.SegmentCommitted
				committedCount++
			}
			key := snapshot.SegmentKey{RouteID: rd.Route.ID, Segment: seg}
			rv.Segments = append(rv.Segments, SegmentView{
				Index: seg, Phase: phase, FrameStart: indices[0], FrameEnd: indices[len(indices)-1],
				Error: errBySegment[fmt.Sprintf("%s/%d", rd.Route.ID, seg)],
			})
			rv.FramesCommitted += frameCounts[key]
		}
		switch {
		case committedCount == len(rv.Segments):
			rv.Phase = "completed"
		case committedCount > 0:
			rv.Phase = "partial"
		default:
			rv.Phase = "pending"
		}
		view.Routes = append(view.Routes, rv)
	}
	if !running {
		view.Progress.RoutesTotal = len(view.Routes)
		view.Progress.SegmentsTotal = 0
		view.Progress.SegmentsCommitted = 0
		view.Progress.RoutesCommitted = 0
		for _, route := range view.Routes {
			view.Progress.SegmentsTotal += len(route.Segments)
			for _, segment := range route.Segments {
				if segment.Phase == state.SegmentCommitted {
					view.Progress.SegmentsCommitted++
				}
			}
			if route.Phase == "completed" {
				view.Progress.RoutesCommitted++
			}
		}
	}
	return view, nil
}

func countBlocks(events []engine.Event) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == "frame_buffer_blocked" {
			n++
		}
	}
	return n
}

func committed(dbPath string) (map[snapshot.SegmentKey]snapshot.StoredSegment, map[snapshot.SegmentKey]int) {
	segmentsOut := make(map[snapshot.SegmentKey]snapshot.StoredSegment)
	countsOut := make(map[snapshot.SegmentKey]int)
	if _, err := os.Stat(dbPath); err != nil {
		return segmentsOut, countsOut
	}
	store, err := snapshot.OpenReadOnly(dbPath)
	if err != nil {
		return segmentsOut, countsOut
	}
	defer store.Close()
	segments, frames, err := store.ReadAll(context.Background())
	if err != nil {
		return segmentsOut, countsOut
	}
	for key, seg := range segments {
		segmentsOut[key] = seg
	}
	for key := range frames {
		segKey := snapshot.SegmentKey{RouteID: key.RouteID, Segment: keySegment(frames, key)}
		countsOut[segKey]++
	}
	return segmentsOut, countsOut
}

func keySegment(frames map[snapshot.FrameKey]snapshot.StoredFrame, key snapshot.FrameKey) int {
	return frames[key].Segment
}

type OpenRequest struct {
	Root       string `json:"root"`
	SamplesDir string `json:"samples_dir"`
	DBPath     string `json:"db_path"`
}

func (a *App) Open(req OpenRequest) (WorkspaceView, error) {
	root := strings.TrimSpace(req.Root)
	if root == "" {
		root = detectRoot()
	}
	samples := strings.TrimSpace(req.SamplesDir)
	if samples == "" {
		samples = filepath.Join(root, "samples")
	}
	db := strings.TrimSpace(req.DBPath)
	if db == "" {
		db = filepath.Join(root, "full.db")
	}
	if _, err := sample.LoadWithOptions(samples, sample.LoadOptions{}); err != nil {
		return a.Workspace(), err
	}
	a.mu.Lock()
	a.root, a.samples, a.db = root, samples, db
	a.mu.Unlock()
	return a.Workspace(), nil
}

type GenerateRequest struct {
	Root string `json:"root"`
	Seed int64  `json:"seed"`
}

func (a *App) Generate(req GenerateRequest) (WorkspaceView, error) {
	root := strings.TrimSpace(req.Root)
	if root == "" {
		a.mu.Lock()
		root = a.root
		a.mu.Unlock()
	}
	if root == "" {
		root = detectRoot()
	}
	seed := req.Seed
	if seed == 0 {
		seed = 42
	}
	samples := filepath.Join(root, "samples")
	if _, err := sample.Generate(sample.GenerateOptions{Dir: samples, Seed: seed}); err != nil {
		return a.Workspace(), err
	}
	a.mu.Lock()
	a.root, a.samples, a.db = root, samples, filepath.Join(root, "full.db")
	a.mu.Unlock()
	return a.Workspace(), nil
}

type RunRequest struct {
	Mode          string       `json:"mode"`
	Params        model.Params `json:"params"`
	MaxWorkers    int          `json:"max_workers"`
	Buffer        int          `json:"buffer"`
	CancelAfter   int          `json:"cancel_after"`
	InjectionPath string       `json:"injection_path"`
	AutoRetry     bool         `json:"auto_retry"`
}

type RunResponse struct {
	Result    *engine.RunResult `json:"result,omitempty"`
	Error     *apperr.Info      `json:"error,omitempty"`
	Workspace WorkspaceView     `json:"workspace"`
}

func (a *App) Run(req RunRequest) (RunResponse, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return RunResponse{}, fmt.Errorf("a pipeline run is already active")
	}
	if a.params == (model.Params{}) {
		a.params = model.DefaultParams()
	}
	if req.Params == (model.Params{}) {
		req.Params = a.params
	}
	if req.MaxWorkers <= 0 {
		req.MaxWorkers = a.workers
	}
	if req.Buffer <= 0 {
		req.Buffer = a.buffer
	}
	runCtx, cancel := context.WithCancel(a.ctx)
	a.cancel = cancel
	a.running = true
	a.params = req.Params
	a.workers, a.buffer = req.MaxWorkers, req.Buffer
	a.events = nil
	samples, db := a.samples, a.db
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.running = false
		a.cancel = nil
		a.mu.Unlock()
	}()
	mode := engine.Mode(req.Mode)
	if mode == "" {
		mode = engine.ModeRealtime
	}
	result, runErr := engine.Execute(runCtx, engine.Config{
		DatasetDir: samples, DBPath: db, InjectionPath: req.InjectionPath, Mode: mode,
		Params: req.Params, MaxWorkers: req.MaxWorkers, FrameBufferPerWorker: req.Buffer,
		CancelAfterCommits: req.CancelAfter, AutoRetryInjectedFailure: req.AutoRetry,
		EventSink: func(ev engine.Event) {
			a.mu.Lock()
			a.events = append(a.events, ev)
			a.mu.Unlock()
		},
	})
	response := RunResponse{Result: result}
	a.mu.Lock()
	a.last = result
	if runErr != nil && result != nil {
		info := apperr.Describe(runErr)
		a.lastErr = &info
		response.Error = &info
	} else if runErr != nil {
		a.lastErr = infoFromError(runErr)
	} else {
		a.lastErr = nil
	}
	a.mu.Unlock()
	response.Workspace = a.Workspace()
	if result == nil && runErr != nil {
		return response, runErr
	}
	return response, nil
}

func (a *App) Cancel() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return false
	}
	a.cancel()
	return true
}

type RetryRequest struct {
	RouteID string `json:"route_id"`
	Segment int    `json:"segment"`
}

func (a *App) RetrySegment(req RetryRequest) (RunResponse, error) {
	a.mu.Lock()
	db := a.db
	params := a.params
	maxWorkers, buffer := a.workers, a.buffer
	a.mu.Unlock()
	if store, err := snapshot.OpenReadOnly(db); err == nil {
		segments, _, readErr := store.ReadAll(context.Background())
		store.Close()
		if readErr == nil {
			if _, ok := segments[snapshot.SegmentKey{RouteID: req.RouteID, Segment: req.Segment}]; ok {
				resp := RunResponse{Workspace: a.Workspace()}
				info := apperr.Describe(apperr.New(apperr.CodeState, req.RouteID, req.Segment, -1,
					"segment_state", "uncommitted segment", "committed segment is frozen", nil))
				resp.Error = &info
				return resp, nil
			}
		}
	}
	return a.Run(RunRequest{Mode: string(engine.ModeResume), Params: params,
		MaxWorkers: maxWorkers, Buffer: buffer})
}

type AuditResponse struct {
	Result *audit.Result `json:"result,omitempty"`
	Error  *apperr.Info  `json:"error,omitempty"`
}

func (a *App) Audit() AuditResponse {
	a.mu.Lock()
	samples, db := a.samples, a.db
	a.mu.Unlock()
	result, err := audit.Audit(context.Background(), samples, db)
	response := AuditResponse{Result: result}
	if err != nil {
		info := apperr.Describe(err)
		response.Error = &info
	}
	return response
}

type CompareRequest struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type CompareResponse struct {
	Result *compare.Result `json:"result,omitempty"`
	Error  *apperr.Info    `json:"error,omitempty"`
}

func (a *App) Compare(req CompareRequest) CompareResponse {
	a.mu.Lock()
	left, right := req.Left, req.Right
	if left == "" {
		left = a.db
	}
	if right == "" {
		right = filepath.Join(a.root, "realtime.db")
	}
	a.mu.Unlock()
	result, err := compare.Snapshots(context.Background(), left, right)
	response := CompareResponse{Result: result}
	if err != nil {
		info := apperr.Describe(err)
		response.Error = &info
	}
	return response
}

func (a *App) Export(outDir string) (*exporter.Summary, error) {
	a.mu.Lock()
	root, samples, db := a.root, a.samples, a.db
	a.mu.Unlock()
	if outDir == "" {
		outDir = filepath.Join(root, "export")
	}
	return exporter.Run(context.Background(), db, outDir, samples)
}

type FrameDetail struct {
	RouteID         string            `json:"route_id"`
	FrameIndex      int               `json:"frame_index"`
	Segment         int               `json:"segment"`
	TimestampMS     int64             `json:"timestamp_ms"`
	Width           int               `json:"width"`
	Height          int               `json:"height"`
	Committed       bool              `json:"committed"`
	SDRDataURL      string            `json:"sdr_data_url,omitempty"`
	HDRDataURL      string            `json:"hdr_data_url,omitempty"`
	Proof           *model.Proof      `json:"proof,omitempty"`
	Truth           *model.FrameTruth `json:"truth,omitempty"`
	InputPeakNits   float64           `json:"input_peak_nits"`
	InputEncoding   string            `json:"input_encoding"`
	InputColorSpace string            `json:"input_color_space"`
}

func (a *App) FrameDetail(routeID string, frameIndex int) (FrameDetail, error) {
	a.mu.Lock()
	samples, db := a.samples, a.db
	a.mu.Unlock()
	ds, err := sample.Load(samples)
	if err != nil {
		return FrameDetail{}, err
	}
	var rd *model.RouteData
	for _, candidate := range ds.Routes {
		if candidate.Route.ID == routeID {
			rd = candidate
			break
		}
	}
	if rd == nil {
		return FrameDetail{}, fmt.Errorf("unknown route %q", routeID)
	}
	if frameIndex < 0 || frameIndex >= len(rd.DecodedFrames) {
		return FrameDetail{}, fmt.Errorf("frame index %d out of range", frameIndex)
	}
	detail := FrameDetail{
		RouteID: routeID, FrameIndex: frameIndex,
		Segment:     model.SegmentOf(rd.Route, rd.Route.TimestampsMS[frameIndex]),
		TimestampMS: rd.Route.TimestampsMS[frameIndex], Width: rd.Route.Width, Height: rd.Route.Height,
		InputPeakNits: rd.Route.PeakNits, InputEncoding: rd.Route.Encoding,
		InputColorSpace: rd.Route.ColorSpace.Name,
	}
	truthCopy := rd.Truth.Frames[frameIndex]
	detail.Truth = &truthCopy
	detail.HDRDataURL, _ = hdrDataURL(rd, frameIndex)
	if store, err := snapshot.OpenReadOnly(db); err == nil {
		_, frames, readErr := store.ReadAll(context.Background())
		store.Close()
		if readErr == nil {
			if frame, ok := frames[snapshot.FrameKey{RouteID: routeID, Frame: frameIndex}]; ok {
				var proof model.Proof
				if err := jsonUnmarshal(frame.ProofJSON, &proof); err == nil {
					detail.Committed = true
					detail.Proof = &proof
					detail.SDRDataURL = rgbDataURL(frame.Width, frame.Height, frame.Pixels)
				}
			}
		}
	}
	return detail, nil
}

func (a *App) Events() []engine.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]engine.Event(nil), a.events...)
}

func hdrDataURL(rd *model.RouteData, frameIndex int) (string, error) {
	frame := rd.DecodedFrames[frameIndex]
	matrix := color.PrimariesToTarget(rd.Route.ColorSpace.Primaries)
	pixels := make([]byte, rd.Route.Width*rd.Route.Height*3)
	for i := 0; i < rd.Route.Width*rd.Route.Height; i++ {
		src := [3]float64{frame.LinearRGB[i*3], frame.LinearRGB[i*3+1], frame.LinearRGB[i*3+2]}
		target := [3]float64{
			matrix[0][0]*src[0] + matrix[0][1]*src[1] + matrix[0][2]*src[2],
			matrix[1][0]*src[0] + matrix[1][1]*src[1] + matrix[1][2]*src[2],
			matrix[2][0]*src[0] + matrix[2][1]*src[1] + matrix[2][2]*src[2],
		}
		target = color.Desaturate(target)
		for c := 0; c < 3; c++ {
			pixels[i*3+c] = color.EncodeSRGBByte(color.ACES(target[c]))
		}
	}
	return rgbDataURL(rd.Route.Width, rd.Route.Height, pixels), nil
}

func rgbDataURL(width, height int, pixels []byte) string {
	if width <= 0 || height <= 0 || len(pixels) != width*height*3 {
		return ""
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 3
			img.SetNRGBA(x, y, stdcolor.NRGBA{R: pixels[i], G: pixels[i+1], B: pixels[i+2], A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func infoFromError(err error) *apperr.Info {
	if err == nil {
		return nil
	}
	info := apperr.Describe(err)
	return &info
}

func detectRoot() string {
	if env := os.Getenv("HDRPROOF_ROOT"); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
	}
	candidates := []string{
		filepath.Join("artifacts", "demo"),
		filepath.Join("..", "artifacts", "demo"),
		filepath.Join("..", "..", "artifacts", "demo"),
		filepath.Join("..", "..", "..", "artifacts", "demo"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(candidate, "samples", "manifest.json")); err == nil {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}
	abs, _ := filepath.Abs(filepath.Join("artifacts", "demo"))
	return abs
}

// The wrappers keep app.go free of a second JSON import and make the use of
// encoding/json visible to static analysis.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

func firstEventReason(events []engine.Event, kind string) string {
	for _, ev := range events {
		if ev.Kind == kind {
			return ev.Reason + ": " + ev.Message
		}
	}
	return ""
}

func sortedRouteIDs(routes []RouteView) []string {
	out := make([]string, 0, len(routes))
	for _, route := range routes {
		out = append(out, route.Route.ID)
	}
	sort.Strings(out)
	return out
}

func frameRange(route model.Route, segment int) string {
	indices := model.FrameIndicesForSegment(route, segment)
	if len(indices) == 0 {
		return "empty"
	}
	return strconv.Itoa(indices[0]) + ".." + strconv.Itoa(indices[len(indices)-1])
}

func clampPreview(v float64) byte {
	return color.EncodeSRGBByte(math.Max(0, math.Min(1, v)))
}

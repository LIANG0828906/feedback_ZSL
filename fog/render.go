package fog

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type RenderMode string

const (
	ModePreview RenderMode = "preview"
	ModeFull    RenderMode = "full"
)

type PixelValue struct {
	X             int     `json:"x"`
	Y             int     `json:"y"`
	Transmittance float64 `json:"transmittance"`
	Radiance      float64 `json:"radiance"`
}

type BlockResult struct {
	BlockID           int           `json:"block_id"`
	TX                int           `json:"tx"`
	TY                int           `json:"ty"`
	X0                int           `json:"x0"`
	Y0                int           `json:"y0"`
	Width             int           `json:"width"`
	Height            int           `json:"height"`
	Status            string        `json:"status"`
	CacheHit          bool          `json:"cache_hit"`
	Pixels            []PixelValue  `json:"pixels"`
	Transmittance     []float64     `json:"transmittance"`
	Radiance          []float64     `json:"radiance"`
	RayDigest         string        `json:"ray_digest"`
	SegmentCount      int           `json:"segment_count"`
	StepBudgetPerSpan int           `json:"step_budget_per_span"`
	StepsUsed         int64         `json:"steps_used"`
	ShadowSamplesUsed int64         `json:"shadow_samples_used"`
	ComputeMS         float64       `json:"compute_ms"`
	Attempts          int           `json:"attempts"`
	Cache             CacheIdentity `json:"cache"`
	Error             *Error        `json:"error,omitempty"`
	Traces            []TraceResult `json:"-"`
}

type QueueEvent struct {
	BlockID int     `json:"block_id"`
	Reason  string  `json:"reason"`
	WaitMS  float64 `json:"wait_ms"`
	Waited  bool    `json:"waited"`
}

type BudgetReport struct {
	MaxParallelBlocks  int          `json:"max_parallel_blocks"`
	RayBufferPerWorker int          `json:"ray_buffer_per_worker"`
	ShadowBudget       int64        `json:"shadow_budget"`
	ShadowUsed         int64        `json:"shadow_used"`
	ShadowRemaining    int64        `json:"shadow_remaining"`
	PeakParallel       int          `json:"peak_parallel"`
	Queue              []QueueEvent `json:"queue"`
}

type QualityReport struct {
	Mode                 RenderMode `json:"mode"`
	StepBudgetPerSpan    int        `json:"step_budget_per_span"`
	ShadowSamplesPerStep int        `json:"shadow_samples_per_step"`
	TotalStepsUsed       int64      `json:"total_steps_used"`
	TotalShadowSamples   int64      `json:"total_shadow_samples"`
	ElapsedMS            float64    `json:"elapsed_ms"`
	PreviewDelayLimitMS  int64      `json:"preview_delay_limit_ms,omitempty"`
	WithinDelayLimit     bool       `json:"within_delay_limit"`
	Basis                string     `json:"basis"`
}

type FrameVersions struct {
	SnapshotVersion  string            `json:"snapshot_version"`
	MediumHashes     map[string]string `json:"medium_hashes"`
	CameraID         string            `json:"camera_id"`
	CameraVersion    string            `json:"camera_version"`
	RecipeVersion    string            `json:"recipe_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
}

type Frame struct {
	Width        int            `json:"width"`
	Height       int            `json:"height"`
	Mode         RenderMode     `json:"mode"`
	Versions     FrameVersions  `json:"versions"`
	Pixels       []PixelValue   `json:"pixels"`
	T            []float64      `json:"t"`
	Radiance     []float64      `json:"radiance"`
	Blocks       []*BlockResult `json:"blocks"`
	BlockOrder   []int          `json:"block_order"`
	Budget       BudgetReport   `json:"budget"`
	Quality      QualityReport  `json:"quality"`
	FailedBlocks []int          `json:"failed_blocks,omitempty"`
}

type BudgetGate struct {
	maxBlocks   int
	slots       chan struct{}
	rayBuffer   int
	mu          sync.Mutex
	shadowTotal int64
	shadowUsed  int64
	events      []QueueEvent
	active      map[int]bool
	peak        int
	refresh     chan struct{}
}

func NewBudgetGate(r Recipe) *BudgetGate {
	return &BudgetGate{
		maxBlocks:   r.MaxParallelBlocks,
		slots:       make(chan struct{}, r.MaxParallelBlocks),
		rayBuffer:   r.RayBufferPerWorker,
		shadowTotal: r.ShadowSampleBudget,
		active:      map[int]bool{},
		refresh:     make(chan struct{}, 1),
	}
}

func (g *BudgetGate) AcquireBlock(ctx context.Context, blockID int) (func(), error) {
	start := time.Now()
	select {
	case g.slots <- struct{}{}:
		g.markActive(blockID)
		return g.release(blockID), nil
	default:
	}

	event := QueueEvent{BlockID: blockID, Reason: "max_parallel_blocks", Waited: true}
	eventIndex := g.appendEvent(event)
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, &Error{Code: EBlock, BlockID: blockIDPtr(blockID), Field: "budget.parallel_blocks", Message: ctx.Err().Error()}
	}
	waited := time.Since(start)
	g.updateWait(eventIndex, waited)
	g.markActive(blockID)
	return g.release(blockID), nil
}

func (g *BudgetGate) release(blockID int) func() {
	return func() {
		g.mu.Lock()
		delete(g.active, blockID)
		g.mu.Unlock()
		<-g.slots
	}
}

func (g *BudgetGate) markActive(blockID int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active[blockID] = true
	if len(g.active) > g.peak {
		g.peak = len(g.active)
	}
}

func (g *BudgetGate) appendEvent(e QueueEvent) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, e)
	return len(g.events) - 1
}

func (g *BudgetGate) updateWait(idx int, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if idx >= 0 && idx < len(g.events) {
		g.events[idx].WaitMS = float64(d.Microseconds()) / 1000.0
	}
}

func (g *BudgetGate) ReserveShadow(ctx context.Context, blockID int, n int64) error {
	if n <= 0 {
		return nil
	}
	start := time.Now()
	g.mu.Lock()
	if g.shadowUsed+n <= g.shadowTotal {
		g.shadowUsed += n
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	idx := g.appendEvent(QueueEvent{BlockID: blockID, Reason: "shadow_sample_budget", Waited: true})
	for {
		g.mu.Lock()
		if g.shadowUsed+n <= g.shadowTotal {
			g.shadowUsed += n
			g.mu.Unlock()
			g.updateWait(idx, time.Since(start))
			return nil
		}
		g.mu.Unlock()
		select {
		case <-g.refresh:
		case <-ctx.Done():
			g.updateWait(idx, time.Since(start))
			return &Error{Code: EBudget, BlockID: blockIDPtr(blockID), Field: "budget.shadow_samples", Message: "shadow budget wait canceled: " + ctx.Err().Error()}
		}
	}
}

// AddShadowQuota makes additional budget available. It is used by a new frame
// or an explicit scheduler replenishment, never to silently drop work.
func (g *BudgetGate) AddShadowQuota(n int64) {
	g.mu.Lock()
	g.shadowTotal += n
	g.mu.Unlock()
	select {
	case g.refresh <- struct{}{}:
	default:
	}
}

func (g *BudgetGate) Report(r Recipe) BudgetReport {
	g.mu.Lock()
	defer g.mu.Unlock()
	events := append([]QueueEvent(nil), g.events...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].BlockID != events[j].BlockID {
			return events[i].BlockID < events[j].BlockID
		}
		return events[i].Reason < events[j].Reason
	})
	return BudgetReport{
		MaxParallelBlocks: r.MaxParallelBlocks, RayBufferPerWorker: r.RayBufferPerWorker,
		ShadowBudget: g.shadowTotal, ShadowUsed: g.shadowUsed,
		ShadowRemaining: g.shadowTotal - g.shadowUsed, PeakParallel: g.peak, Queue: events,
	}
}

type FaultController struct {
	mu         sync.Mutex
	once       map[int]string
	persistent map[int]string
	attempts   map[int]int
}

func NewFaultController() *FaultController {
	return &FaultController{once: map[int]string{}, persistent: map[int]string{}, attempts: map[int]int{}}
}

func (f *FaultController) FailBlockOnce(id int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.once[id] = message
}

func (f *FaultController) FailBlockPersistent(id int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persistent[id] = message
}

func (f *FaultController) Clear(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.once, id)
	delete(f.persistent, id)
}

func (f *FaultController) Attempts(id int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[id]
}

func (f *FaultController) check(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[id]++
	if msg, ok := f.once[id]; ok {
		delete(f.once, id)
		return &Error{Code: EBlock, BlockID: blockIDPtr(id), Field: "block.execute", Message: msg}
	}
	if msg, ok := f.persistent[id]; ok {
		return &Error{Code: EBlock, BlockID: blockIDPtr(id), Field: "block.execute", Message: msg}
	}
	return nil
}

func RenderFrame(ctx context.Context, s *Scenario, cameraID string, mode RenderMode, faults *FaultController) (*Frame, error) {
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)
	ids := make([]int, 0, len(vp.Tiles))
	for _, tile := range vp.Tiles {
		ids = append(ids, tile.ID)
	}
	return RenderBlocks(ctx, s, cameraID, mode, ids, faults)
}

func RenderBlocks(ctx context.Context, s *Scenario, cameraID string, mode RenderMode, blockIDs []int, faults *FaultController) (*Frame, error) {
	start := time.Now()
	cam, ok := s.Camera(cameraID)
	if !ok {
		cam = s.ActiveCamera()
	}
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)
	gate := NewBudgetGate(s.Recipe)
	integ := NewIntegrator(s, mode)

	frame := &Frame{
		Width: s.Width, Height: s.Height, Mode: mode,
		T: make([]float64, s.Width*s.Height), Radiance: make([]float64, s.Width*s.Height),
		Blocks: make([]*BlockResult, len(vp.Tiles)), BlockOrder: make([]int, 0, len(vp.Tiles)),
		Versions: FrameVersions{
			SnapshotVersion: SnapshotVersion(s), MediumHashes: map[string]string{},
			CameraID: cam.ID, CameraVersion: cam.Version,
			RecipeVersion: s.Recipe.Version, AlgorithmVersion: AlgorithmVersion,
		},
	}
	for _, m := range s.Media {
		frame.Versions.MediumHashes[m.ID] = m.ContentHash
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	requested := map[int]bool{}
	for _, id := range blockIDs {
		requested[id] = true
	}
	for _, blockID := range blockIDs {
		tile, tileErr := vp.Tile(blockID)
		if tileErr != nil {
			return nil, tileErr
		}
		wg.Add(1)
		go func(tile Tile) {
			defer wg.Done()
			release, err := gate.AcquireBlock(ctx, tile.ID)
			if err != nil {
				mu.Lock()
				frame.Blocks[tile.ID] = failedBlock(tile, err)
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			defer release()
			if faults != nil {
				if err := faults.check(tile.ID); err != nil {
					mu.Lock()
					frame.Blocks[tile.ID] = failedBlock(tile, err)
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
			br, err := computeBlock(ctx, s, cam, tile, integ, gate)
			if err != nil {
				mu.Lock()
				frame.Blocks[tile.ID] = failedBlock(tile, err)
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			frame.Blocks[tile.ID] = br
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(tile)
	}
	wg.Wait()
	_ = requested

	for _, block := range frame.Blocks {
		if block == nil {
			continue
		}
		frame.BlockOrder = append(frame.BlockOrder, block.BlockID)
		if block.Status == "failed" {
			frame.FailedBlocks = append(frame.FailedBlocks, block.BlockID)
			continue
		}
		for _, p := range block.Pixels {
			idx := p.Y*s.Width + p.X
			frame.T[idx] = p.Transmittance
			frame.Radiance[idx] = p.Radiance
			frame.Pixels = append(frame.Pixels, p)
		}
		frame.Quality.TotalStepsUsed += block.StepsUsed
		frame.Quality.TotalShadowSamples += block.ShadowSamplesUsed
	}
	sort.Ints(frame.BlockOrder)
	sort.Ints(frame.FailedBlocks)
	sort.Slice(frame.Pixels, func(i, j int) bool {
		if frame.Pixels[i].Y != frame.Pixels[j].Y {
			return frame.Pixels[i].Y < frame.Pixels[j].Y
		}
		return frame.Pixels[i].X < frame.Pixels[j].X
	})

	elapsed := time.Since(start)
	frame.Budget = gate.Report(s.Recipe)
	frame.Quality.Mode = mode
	frame.Quality.StepBudgetPerSpan = integ.Steps
	frame.Quality.ShadowSamplesPerStep = integ.ShadowSamples
	frame.Quality.ElapsedMS = float64(elapsed.Microseconds()) / 1000.0
	frame.Quality.PreviewDelayLimitMS = s.Recipe.PreviewDelayLimitMS
	if mode == ModePreview {
		frame.Quality.WithinDelayLimit = frame.Quality.ElapsedMS <= float64(s.Recipe.PreviewDelayLimitMS)
		frame.Quality.Basis = fmt.Sprintf("preview uses %d steps/span and %d shadow sample(s)/step from recipe; delay limit %dms", integ.Steps, integ.ShadowSamples, s.Recipe.PreviewDelayLimitMS)
	} else {
		frame.Quality.WithinDelayLimit = true
		frame.Quality.Basis = fmt.Sprintf("full uses %d steps/span and %d shadow samples/step from recipe", integ.Steps, integ.ShadowSamples)
	}
	return frame, firstErr
}

func computeBlock(ctx context.Context, s *Scenario, cam Camera, tile Tile, integ Integrator, gate *BudgetGate) (*BlockResult, error) {
	start := time.Now()
	br := &BlockResult{
		BlockID: tile.ID, TX: tile.Coord.TX, TY: tile.Coord.TY,
		X0: tile.X0, Y0: tile.Y0, Width: tile.Width, Height: tile.Height,
		Status: "success", StepBudgetPerSpan: integ.Steps,
		Transmittance: make([]float64, tile.Width*tile.Height),
		Radiance:      make([]float64, tile.Width*tile.Height),
	}
	allTraces := make([]TraceResult, 0, tile.Width*tile.Height)
	points := make([]Point, 0, tile.Width*tile.Height)
	for y := tile.Y0; y < tile.Y0+tile.Height; y++ {
		for x := tile.X0; x < tile.X0+tile.Width; x++ {
			points = append(points, Point{X: x, Y: y})
		}
	}
	for _, chunk := range chunkPoints(points, gate.rayBuffer) {
		rays := make([]Ray, 0, len(chunk))
		for _, p := range chunk {
			rays = append(rays, cam.RayForPixel(p.X, p.Y, s.Width, s.Height, tile.ID))
		}
		chunkTraces := make([]TraceResult, 0, len(rays))
		var shadowCost int64
		for _, ray := range rays {
			tr := TraceRay(ray, s.Media, s.FixedScale)
			chunkTraces = append(chunkTraces, tr)
			shadowCost += integ.ShadowCost(tr)
		}
		if err := gate.ReserveShadow(ctx, tile.ID, shadowCost); err != nil {
			return nil, err
		}
		for _, tr := range chunkTraces {
			rr, err := integ.Integrate(tr)
			if err != nil {
				return nil, err
			}
			allTraces = append(allTraces, tr)
			br.StepsUsed += int64(rr.StepsUsed)
			br.ShadowSamplesUsed += int64(rr.ShadowSamplesUsed)
			br.SegmentCount += len(rr.Segments)
			idx := tile.PixelIndex(tr.Ray.PixelX, tr.Ray.PixelY)
			br.Transmittance[idx] = rr.Transmittance
			br.Radiance[idx] = rr.Radiance
			br.Pixels = append(br.Pixels, PixelValue{
				X: tr.Ray.PixelX, Y: tr.Ray.PixelY,
				Transmittance: rr.Transmittance, Radiance: rr.Radiance,
			})
		}
	}
	sort.Slice(br.Pixels, func(i, j int) bool {
		if br.Pixels[i].Y != br.Pixels[j].Y {
			return br.Pixels[i].Y < br.Pixels[j].Y
		}
		return br.Pixels[i].X < br.Pixels[j].X
	})
	br.Traces = allTraces
	br.Cache = BuildCacheIdentity(s, tile, allTraces)
	br.RayDigest = br.Cache.RayDigest
	br.ComputeMS = float64(time.Since(start).Microseconds()) / 1000.0
	return br, nil
}

func failedBlock(tile Tile, err error) *BlockResult {
	var ferr *Error
	if asError(err, &ferr) {
		if ferr.BlockID == nil {
			ferr.BlockID = blockIDPtr(tile.ID)
		}
	} else {
		ferr = &Error{Code: EBlock, BlockID: blockIDPtr(tile.ID), Message: err.Error()}
	}
	return &BlockResult{
		BlockID: tile.ID, TX: tile.Coord.TX, TY: tile.Coord.TY,
		X0: tile.X0, Y0: tile.Y0, Width: tile.Width, Height: tile.Height,
		Status: "failed", Error: ferr,
	}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func chunkPoints(points []Point, size int) [][]Point {
	if size <= 0 {
		size = len(points)
	}
	var out [][]Point
	for i := 0; i < len(points); i += size {
		end := i + size
		if end > len(points) {
			end = len(points)
		}
		out = append(out, points[i:end])
	}
	return out
}

func SnapshotVersion(s *Scenario) string {
	hashes := map[string]string{}
	for _, m := range s.Media {
		hashes[m.ID] = m.ContentHash
	}
	cam := s.ActiveCamera()
	return ContentHash("snapshot", struct {
		Media  map[string]string `json:"media"`
		Camera string            `json:"camera"`
		Recipe string            `json:"recipe"`
		Algo   string            `json:"algorithm"`
		Width  int               `json:"width"`
		Height int               `json:"height"`
	}{hashes, cam.Version, s.Recipe.Version, AlgorithmVersion, s.Width, s.Height})
}

func CompareFrames(a, b *Frame, transTol, radTol float64) error {
	if a.Width != b.Width || a.Height != b.Height {
		return &Error{Code: EDiverge, Field: "viewport", Message: "frame dimensions differ"}
	}
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			idx := y*a.Width + x
			dt := absFloat(a.T[idx] - b.T[idx])
			dl := absFloat(a.Radiance[idx] - b.Radiance[idx])
			if dt > transTol || dl > radTol {
				return &Error{
					Code: EDiverge, X: x, Y: y,
					Actual: a.T[idx], Expected: b.T[idx], Tolerance: transTol,
					Field:   "pixel",
					Message: fmt.Sprintf("first divergent pixel (%d,%d): ΔT=%.8g ΔL=%.8g", x, y, dt, dl),
					Detail: map[string]any{
						"transmittance_a": a.T[idx], "transmittance_b": b.T[idx],
						"radiance_a": a.Radiance[idx], "radiance_b": b.Radiance[idx],
						"delta_t": dt, "delta_l": dl,
						"transmittance_tolerance": transTol, "radiance_tolerance": radTol,
						"versions_a": a.Versions, "versions_b": b.Versions,
					},
				}
			}
		}
	}
	return nil
}

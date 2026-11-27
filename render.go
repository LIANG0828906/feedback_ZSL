package volumefog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"volumefog/store"
)

type PixelResult struct {
	X             int        `json:"x"`
	Y             int        `json:"y"`
	RayID         string     `json:"ray_id"`
	Transmittance float64    `json:"transmittance"`
	Scattering    float64    `json:"scattering"`
	Color         [3]float64 `json:"color"`
	StepsUsed     int        `json:"steps_used"`
	ShadowSamples int        `json:"shadow_samples"`
}

type ChunkResult struct {
	ChunkX              int             `json:"chunk_x"`
	ChunkY              int             `json:"chunk_y"`
	X0                  int             `json:"x0"`
	Y0                  int             `json:"y0"`
	Width               int             `json:"width"`
	Height              int             `json:"height"`
	Status              string          `json:"status"`
	Pixels              []PixelResult   `json:"pixels"`
	CenterX             int             `json:"center_x"`
	CenterY             int             `json:"center_y"`
	CenterColor         [3]float64      `json:"center_color"`
	CenterTransmittance float64         `json:"center_transmittance"`
	CenterScattering    float64         `json:"center_scattering"`
	RayDigest           string          `json:"ray_digest"`
	ContentHash         string          `json:"content_hash"`
	CacheKey            string          `json:"cache_key"`
	RecipeVersion       string          `json:"recipe_version"`
	CameraVersion       string          `json:"camera_version"`
	AlgorithmVersion    string          `json:"algorithm_version"`
	SceneVersion        string          `json:"scene_version"`
	CommitSeq           int64           `json:"commit_seq"`
	RayDeps             []RayDependency `json:"ray_deps"`
	Dependencies        []DependencyRef `json:"dependencies"`
	StepsUsed           int             `json:"steps_used"`
	ShadowSamplesUsed   int             `json:"shadow_samples_used"`
	StepsBudget         int             `json:"steps_budget"`
	ShadowBudget        int             `json:"shadow_budget"`
	RayBufferFlushes    int             `json:"ray_buffer_flushes"`
	RayBufferLimit      int             `json:"ray_buffer_limit"`
	Error               *FogError       `json:"error,omitempty"`
}

type FrameStats struct {
	TotalChunks       int `json:"total_chunks"`
	ComputedChunks    int `json:"computed_chunks"`
	CacheHits         int `json:"cache_hits"`
	InvalidatedChunks int `json:"invalidated_chunks"`
	FailedChunks      int `json:"failed_chunks"`
	Rays              int `json:"rays"`
	StepsUsed         int `json:"steps_used"`
	ShadowSamplesUsed int `json:"shadow_samples_used"`
	CommitsWritten    int `json:"commits_written"`
}

type CacheEvent struct {
	Chunk  ChunkCoord `json:"chunk"`
	Kind   string     `json:"kind"`
	Reason string     `json:"reason"`
}

type Frame struct {
	Mode              string         `json:"mode"`
	SceneVersion      string         `json:"scene_version"`
	MediaVersion      string         `json:"media_version"`
	CameraVersion     string         `json:"camera_version"`
	RecipeVersion     string         `json:"recipe_version"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	Chunks            []ChunkResult  `json:"chunks"`
	Stats             FrameStats     `json:"stats"`
	Budget            BudgetSnapshot `json:"budget"`
	CacheEvents       []CacheEvent   `json:"cache_events"`
	CommittedSequence []int64        `json:"committed_sequence"`
	ElapsedMS         float64        `json:"elapsed_ms"`
	WithinLatency     bool           `json:"within_latency"`
	LatencyLimitMS    int            `json:"latency_limit_ms"`
	Reason            string         `json:"reason,omitempty"`
}

type RenderOptions struct {
	Mode          string
	Steps         int
	ShadowSamples int
	MaxParallel   int
	RayBuffer     int
	UseCache      bool
	Persist       bool
	FailChunks    map[ChunkCoord]string
}

type Renderer struct {
	Store store.Store
	Graph *DependencyGraph
}

func NewRenderer(st store.Store) *Renderer {
	return &Renderer{Store: st, Graph: NewDependencyGraph()}
}

type chunkSpec struct {
	Index  int
	Coord  ChunkCoord
	X0, Y0 int
	W, H   int
}

func enumerateChunks(scene *Scene) []chunkSpec {
	cam := scene.ActiveCameraSample()
	if cam == nil {
		return nil
	}
	var out []chunkSpec
	for y := 0; y < cam.Height; y += scene.ChunkSize {
		for x := 0; x < cam.Width; x += scene.ChunkSize {
			w := minInt(scene.ChunkSize, cam.Width-x)
			h := minInt(scene.ChunkSize, cam.Height-y)
			out = append(out, chunkSpec{
				Index: len(out), Coord: ChunkCoord{X: x / scene.ChunkSize, Y: y / scene.ChunkSize},
				X0: x, Y0: y, W: w, H: h,
			})
		}
	}
	return out
}

func chunkRayID(spec chunkSpec, px, py int) string {
	return fmt.Sprintf("chunk:%d,%d:ray:%d,%d", spec.Coord.X, spec.Coord.Y, px, py)
}

func pixelCenter(spec chunkSpec) (int, int) {
	return spec.X0 + spec.W/2, spec.Y0 + spec.H/2
}

func (r *Renderer) storePtr() store.Store {
	return r.Store
}

func gzipJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipJSON(data []byte, v any) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func cacheRecordValid(scene *Scene, rec store.ChunkRecord, want chunkSpec) (ChunkResult, bool, string) {
	if rec.AlgorithmVersion != AlgorithmVersion {
		return ChunkResult{}, false, "algorithm version mismatch"
	}
	if rec.RecipeVersion != scene.RecipeVersion() {
		return ChunkResult{}, false, "recipe version mismatch"
	}
	if rec.CameraVersion != scene.CameraVersion() {
		return ChunkResult{}, false, "camera version mismatch"
	}
	var res ChunkResult
	if err := gunzipJSON(rec.Payload, &res); err != nil {
		return ChunkResult{}, false, "checkpoint payload decode failed: " + err.Error()
	}
	if res.X0 != want.X0 || res.Y0 != want.Y0 || res.Width != want.W || res.Height != want.H {
		return ChunkResult{}, false, "chunk geometry mismatch"
	}
	for i := range res.RayDeps {
		rd := &res.RayDeps[i]
		if len(rd.Segments) != rd.SegmentCount {
			return ChunkResult{}, false, "segment dependency chain missing"
		}
		fp, key := RayCacheKey(scene, rd.RayID, rd.Dependencies, "")
		if fp != rd.ContentHash {
			return ChunkResult{}, false, "ray content hash changed"
		}
		if key != rd.CacheKey {
			return ChunkResult{}, false, "ray cache key changed"
		}
		prev := ""
		for _, sd := range rd.Segments {
			sfp, skey := RayCacheKey(scene, fmt.Sprintf("%s:seg:%d", rd.RayID, sd.Index),
				sd.Dependencies, prev)
			if sfp != sd.ContentHash || skey != sd.CacheKey {
				return ChunkResult{}, false, "ray segment cache key changed"
			}
			prev = skey
		}
	}
	content := ChunkContentHash(scene, res.RayDeps)
	if content != rec.ContentHash {
		return ChunkResult{}, false, "chunk content hash changed"
	}
	key := ChunkCacheKey(scene, res.RayDeps, content)
	if key != rec.CacheKey {
		return ChunkResult{}, false, "chunk cache key changed"
	}
	return res, true, ""
}

func buildChunkResult(scene *Scene, spec chunkSpec, rayResults []RayResult,
	stepsBudget, shadowBudget, rayBuffer, flushes int) ChunkResult {
	cam := scene.ActiveCameraSample()
	_ = cam
	cr := ChunkResult{
		ChunkX: spec.Coord.X, ChunkY: spec.Coord.Y, X0: spec.X0, Y0: spec.Y0,
		Width: spec.W, Height: spec.H, Status: "computed",
		RecipeVersion: scene.RecipeVersion(), CameraVersion: scene.CameraVersion(),
		AlgorithmVersion: AlgorithmVersion, SceneVersion: scene.SnapshotVersion(),
		StepsBudget: stepsBudget, ShadowBudget: shadowBudget,
		RayBufferFlushes: flushes, RayBufferLimit: rayBuffer,
	}
	centerX, centerY := pixelCenter(spec)
	allDeps := make([]DependencyRef, 0, len(rayResults)*8)
	for _, rr := range rayResults {
		rd := RayDependency{
			RayID: rr.RayID, CacheKey: rr.RayCacheKey, ContentHash: rr.RayContentHash,
			SegmentCount: len(rr.Segments), Dependencies: rr.Dependencies,
		}
		rd.Segments = buildSegmentDependencies(scene, rr.RayID, rr.Segments)
		for _, sd := range rd.Segments {
			rd.SegmentKeys = append(rd.SegmentKeys, sd.CacheKey)
		}
		cr.RayDeps = append(cr.RayDeps, rd)
		allDeps = append(allDeps, rr.Dependencies...)
		cr.StepsUsed += rr.StepsUsed
		cr.ShadowSamplesUsed += rr.ShadowSamplesUsed
		px, py := pixelFromRayID(rr.RayID)
		pr := PixelResult{
			X: px, Y: py, RayID: rr.RayID, Transmittance: rr.FinalTransmittance,
			Scattering: rr.FinalRadiance, Color: rr.Color, StepsUsed: rr.StepsUsed,
			ShadowSamples: rr.ShadowSamplesUsed,
		}
		cr.Pixels = append(cr.Pixels, pr)
		if px == centerX && py == centerY {
			cr.CenterX, cr.CenterY = px, py
			cr.CenterColor, cr.CenterTransmittance, cr.CenterScattering = rr.Color, rr.FinalTransmittance, rr.FinalRadiance
		}
	}
	sort.Slice(cr.Pixels, func(i, j int) bool {
		if cr.Pixels[i].Y != cr.Pixels[j].Y {
			return cr.Pixels[i].Y < cr.Pixels[j].Y
		}
		return cr.Pixels[i].X < cr.Pixels[j].X
	})
	cr.Dependencies = dedupeDependencies(allDeps)
	cr.ContentHash = ChunkContentHash(scene, cr.RayDeps)
	cr.CacheKey = ChunkCacheKey(scene, cr.RayDeps, cr.ContentHash)
	cr.RayDigest = rayResultDigest(rayResults)
	return cr
}

func pixelFromRayID(id string) (int, int) {
	var cx, cy, px, py int
	_, _ = fmt.Sscanf(id, "chunk:%d,%d:ray:%d,%d", &cx, &cy, &px, &py)
	return px, py
}

func (r *Renderer) computeChunk(scene *Scene, spec chunkSpec, opts RenderOptions,
	worker int, budget *Budget) ChunkResult {
	if reason, fail := opts.FailChunks[spec.Coord]; fail {
		e := &FogError{Code: E_BLOCK, Message: "injected chunk failure", Chunk: &spec.Coord, Cause: reason}
		return ChunkResult{
			ChunkX: spec.Coord.X, ChunkY: spec.Coord.Y, X0: spec.X0, Y0: spec.Y0,
			Width: spec.W, Height: spec.H, Status: "failed",
			RecipeVersion: scene.RecipeVersion(), CameraVersion: scene.CameraVersion(),
			AlgorithmVersion: AlgorithmVersion, SceneVersion: scene.SnapshotVersion(), Error: e,
		}
	}
	cam := scene.ActiveCameraSample()
	if cam == nil {
		e := &FogError{Code: E_BLOCK, Message: "no active camera", Chunk: &spec.Coord}
		return ChunkResult{ChunkX: spec.Coord.X, ChunkY: spec.Coord.Y, Status: "failed", Error: e}
	}
	var rays []Ray
	for py := spec.Y0; py < spec.Y0+spec.H; py++ {
		for px := spec.X0; px < spec.X0+spec.W; px++ {
			rays = append(rays, CameraRay(*cam, scene.ActiveCamera, px, py, chunkRayID(spec, px, py)))
		}
	}
	var results []RayResult
	flushes := 0
	for start := 0; start < len(rays); start += opts.RayBuffer {
		end := minInt(start+opts.RayBuffer, len(rays))
		flushes++
		for _, ray := range rays[start:end] {
			rr, err := IntegrateRay(scene, ray, IntegrateOptions{
				Steps: opts.Steps, ShadowSamples: opts.ShadowSamples,
				Budget: budget, Chunk: spec.Coord, Worker: worker,
			})
			if err != nil {
				fe := &FogError{Code: E_BLOCK, Message: "chunk ray integration failed",
					Chunk: &spec.Coord, RayID: ray.ID, Cause: err.Error()}
				if f, ok := err.(*FogError); ok {
					fe.Actual = f.Actual
					fe.Expected = f.Expected
					fe.Deviation = f.Deviation
					fe.Tolerance = f.Tolerance
				}
				return ChunkResult{
					ChunkX: spec.Coord.X, ChunkY: spec.Coord.Y, X0: spec.X0, Y0: spec.Y0,
					Width: spec.W, Height: spec.H, Status: "failed",
					RecipeVersion: scene.RecipeVersion(), CameraVersion: scene.CameraVersion(),
					AlgorithmVersion: AlgorithmVersion, SceneVersion: scene.SnapshotVersion(), Error: fe,
				}
			}
			results = append(results, rr)
		}
	}
	return buildChunkResult(scene, spec, results, opts.Steps, opts.ShadowSamples, opts.RayBuffer, flushes)
}

type renderTask struct {
	Spec     chunkSpec
	Enqueued time.Time
}

type workerResult struct {
	Index  int
	Result ChunkResult
}

// Render executes a frame. Computed chunks are committed in row-major chunk
// order; one SQLite transaction contains each chunk's complete checkpoint.
func (r *Renderer) Render(scene *Scene, opts RenderOptions) (*Frame, error) {
	start := time.Now()
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = "exact"
	}
	if opts.Steps <= 0 {
		opts.Steps = scene.Recipe.Steps
	}
	if opts.ShadowSamples <= 0 {
		opts.ShadowSamples = scene.Recipe.ShadowSamples
	}
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = scene.Recipe.MaxParallel
	}
	if opts.RayBuffer <= 0 {
		opts.RayBuffer = scene.Recipe.RayBuffer
	}
	frame := &Frame{
		Mode: opts.Mode, SceneVersion: scene.SnapshotVersion(), MediaVersion: scene.MediaVersion(),
		CameraVersion: scene.CameraVersion(), RecipeVersion: scene.RecipeVersion(),
		AlgorithmVersion: AlgorithmVersion, LatencyLimitMS: scene.Recipe.PreviewLatencyMS,
	}
	specs := enumerateChunks(scene)
	frame.Stats.TotalChunks = len(specs)
	results := make([]ChunkResult, len(specs))
	present := make([]bool, len(specs))
	tasks := make([]renderTask, 0, len(specs))
	st := r.storePtr()
	var cached map[ChunkCoord]store.ChunkRecord
	if opts.UseCache && st != nil {
		records, err := st.ListChunks()
		if err == nil {
			cached = make(map[ChunkCoord]store.ChunkRecord, len(records))
			for _, rec := range records {
				cached[ChunkCoord{X: rec.ChunkX, Y: rec.ChunkY}] = rec
			}
		} else {
			frame.Reason = "cache load failed: " + err.Error()
		}
	}
	for _, spec := range specs {
		if _, injected := opts.FailChunks[spec.Coord]; injected {
			tasks = append(tasks, renderTask{Spec: spec, Enqueued: time.Now()})
			continue
		}
		if cached != nil {
			if rec, ok := cached[spec.Coord]; ok {
				res, valid, reason := cacheRecordValid(scene, rec, spec)
				if valid {
					res.Status = "cached"
					res.CommitSeq = rec.CommitSeq
					results[spec.Index] = res
					present[spec.Index] = true
					frame.Stats.CacheHits++
					frame.CacheEvents = append(frame.CacheEvents, CacheEvent{Chunk: spec.Coord, Kind: "hit",
						Reason: "cache key, content hash and versions match"})
					r.Graph.RecordChunk(spec.Coord, res.RayDeps, res.CacheKey)
					continue
				}
				frame.Stats.InvalidatedChunks++
				frame.CacheEvents = append(frame.CacheEvents, CacheEvent{Chunk: spec.Coord, Kind: "invalidate",
					Reason: reason})
				if st != nil {
					_ = st.DeleteChunk(spec.Coord.X, spec.Coord.Y)
				}
			} else {
				frame.CacheEvents = append(frame.CacheEvents, CacheEvent{Chunk: spec.Coord, Kind: "miss",
					Reason: "checkpoint missing"})
			}
		}
		tasks = append(tasks, renderTask{Spec: spec, Enqueued: time.Now()})
	}
	budget := NewBudget(scene.Recipe.ShadowBudget)
	if len(tasks) > 0 {
		nWorkers := opts.MaxParallel
		if nWorkers > len(tasks) {
			nWorkers = len(tasks)
		}
		taskCh := make(chan renderTask, len(tasks))
		resultCh := make(chan workerResult, len(tasks))
		var wg sync.WaitGroup
		for w := 0; w < nWorkers; w++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for task := range taskCh {
					wait := time.Since(task.Enqueued)
					if wait > time.Millisecond {
						budget.RecordQueueWait(task.Spec.Coord, worker, wait,
							"all chunk workers busy; chunk queued and later scheduled")
					}
					res := r.computeChunk(scene, task.Spec, opts, worker, budget)
					resultCh <- workerResult{Index: task.Spec.Index, Result: res}
				}
			}(w)
		}
		for _, task := range tasks {
			taskCh <- task
		}
		close(taskCh)
		wg.Wait()
		close(resultCh)
		for wr := range resultCh {
			results[wr.Index] = wr.Result
			present[wr.Index] = true
		}
	}
	// Commit and journal strictly in canonical row-major order.
	for i, spec := range specs {
		if !present[i] {
			frame.Stats.FailedChunks++
			continue
		}
		res := results[i]
		if res.Status == "failed" {
			frame.Stats.FailedChunks++
			results[i] = res
			continue
		}
		if res.Status == "cached" {
			results[i] = res
			continue
		}
		if opts.Persist && st != nil {
			payload, err := gzipJSON(res)
			if err != nil {
				fe := &FogError{Code: E_BLOCK, Message: "checkpoint encode failed", Chunk: &spec.Coord, Cause: err.Error()}
				res.Status, res.Error = "failed", fe
				results[i] = res
				frame.Stats.FailedChunks++
				continue
			}
			deps, _ := json.Marshal(res.RayDeps)
			rec := store.ChunkRecord{
				ChunkX: spec.Coord.X, ChunkY: spec.Coord.Y, CommitSeq: int64(i),
				RayDigest: res.RayDigest, CacheKey: res.CacheKey, ContentHash: res.ContentHash,
				RecipeVersion: res.RecipeVersion, AlgorithmVersion: res.AlgorithmVersion,
				CameraVersion: res.CameraVersion, SceneVersion: res.SceneVersion,
				Width: res.Width, Height: res.Height, Transmittance: res.CenterTransmittance,
				Scattering: res.CenterScattering, Dependencies: deps, Payload: payload,
				CreatedUnixUS: time.Now().UnixMicro(),
			}
			if err := st.PutChunk(rec); err != nil {
				fe := &FogError{Code: E_BLOCK, Message: "checkpoint transaction failed", Chunk: &spec.Coord, Cause: err.Error()}
				res.Status, res.Error = "failed", fe
				results[i] = res
				frame.Stats.FailedChunks++
				continue
			}
			res.CommitSeq = int64(i)
			frame.Stats.CommitsWritten++
		}
		res.Status = "computed"
		results[i] = res
		r.Graph.RecordChunk(spec.Coord, res.RayDeps, res.CacheKey)
		frame.Stats.ComputedChunks++
		frame.Stats.Rays += len(res.Pixels)
		frame.Stats.StepsUsed += res.StepsUsed
		frame.Stats.ShadowSamplesUsed += res.ShadowSamplesUsed
	}
	frame.Chunks = results
	frame.Budget = budget.Snapshot()
	if st != nil {
		if commits, err := st.Commits(); err == nil {
			for _, c := range commits {
				frame.CommittedSequence = append(frame.CommittedSequence, c.Seq)
			}
		}
	}
	frame.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000.0
	if opts.Mode == "preview" {
		frame.WithinLatency = frame.ElapsedMS <= float64(scene.Recipe.PreviewLatencyMS)
	} else {
		frame.WithinLatency = true
	}
	for i := range frame.Chunks {
		if frame.Chunks[i].Status == "failed" && frame.Chunks[i].Error != nil {
			return frame, frame.Chunks[i].Error
		}
	}
	return frame, nil
}

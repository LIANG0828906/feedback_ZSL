package volumefog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"volumefog/store"
)

type CacheStats struct {
	CacheHits         int `json:"cache_hits"`
	RecomputedChunks  int `json:"recomputed_chunks"`
	InvalidatedChunks int `json:"invalidated_chunks"`
	CommitsWritten    int `json:"commits_written"`
	FailedChunks      int `json:"failed_chunks"`
}

type MediaView struct {
	ID          string `json:"id"`
	MinQ        Vec3i  `json:"min_q"`
	MaxQ        Vec3i  `json:"max_q"`
	DensityRes  [3]int `json:"density_res"`
	Density     []int  `json:"density"`
	AbsorptionQ int64  `json:"absorption_q"`
	ScatteringQ int64  `json:"scattering_q"`
	AnisotropyQ int64  `json:"anisotropy_q"`
	ContentHash string `json:"content_hash"`
}

type WorkbenchState struct {
	Seed              int64          `json:"seed"`
	SceneVersion      string         `json:"scene_version"`
	MediaVersion      string         `json:"media_version"`
	CameraVersion     string         `json:"camera_version"`
	RecipeVersion     string         `json:"recipe_version"`
	ChunkSize         int            `json:"chunk_size"`
	Camera            CameraSample   `json:"camera"`
	Recipe            Recipe         `json:"recipe"`
	Media             []MediaView    `json:"media"`
	LastFrame         *FrameView     `json:"last_frame,omitempty"`
	PreviewAvailable  bool           `json:"preview_available"`
	ExactAvailable    bool           `json:"exact_available"`
	PreviewExactError *FogError      `json:"preview_exact_error,omitempty"`
	CacheStats        CacheStats     `json:"cache_stats"`
	Invalidations     []Invalidation `json:"invalidations"`
	Graph             GraphStats     `json:"graph"`
	TruthAvailable    bool           `json:"truth_available"`
	TruthStale        bool           `json:"truth_stale"`
	LastFault         *FaultReport   `json:"last_fault,omitempty"`
	DBPath            string         `json:"db_path"`
	DBBackend         string         `json:"db_backend"`
	OutputDir         string         `json:"output_dir"`
}

type ChunkView struct {
	ChunkX              int        `json:"chunk_x"`
	ChunkY              int        `json:"chunk_y"`
	X0                  int        `json:"x0"`
	Y0                  int        `json:"y0"`
	Width               int        `json:"width"`
	Height              int        `json:"height"`
	Status              string     `json:"status"`
	CenterX             int        `json:"center_x"`
	CenterY             int        `json:"center_y"`
	CenterColor         [3]float64 `json:"center_color"`
	CenterTransmittance float64    `json:"center_transmittance"`
	CenterScattering    float64    `json:"center_scattering"`
	RayDigest           string     `json:"ray_digest"`
	CacheKey            string     `json:"cache_key"`
	ContentHash         string     `json:"content_hash"`
	CommitSeq           int64      `json:"commit_seq"`
	StepsUsed           int        `json:"steps_used"`
	ShadowSamplesUsed   int        `json:"shadow_samples_used"`
	RayBufferFlushes    int        `json:"ray_buffer_flushes"`
	Error               *FogError  `json:"error,omitempty"`
}

type FrameView struct {
	Mode              string         `json:"mode"`
	SceneVersion      string         `json:"scene_version"`
	CameraVersion     string         `json:"camera_version"`
	RecipeVersion     string         `json:"recipe_version"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	Chunks            []ChunkView    `json:"chunks"`
	Stats             FrameStats     `json:"stats"`
	Budget            BudgetSnapshot `json:"budget"`
	CacheEvents       []CacheEvent   `json:"cache_events,omitempty"`
	CommittedSequence []int64        `json:"committed_sequence"`
	ElapsedMS         float64        `json:"elapsed_ms"`
	WithinLatency     bool           `json:"within_latency"`
	LatencyLimitMS    int            `json:"latency_limit_ms"`
	Reason            string         `json:"reason,omitempty"`
}

func frameView(f *Frame) *FrameView {
	if f == nil {
		return nil
	}
	v := &FrameView{
		Mode: f.Mode, SceneVersion: f.SceneVersion, CameraVersion: f.CameraVersion,
		RecipeVersion: f.RecipeVersion, AlgorithmVersion: f.AlgorithmVersion,
		Stats: f.Stats, Budget: f.Budget, CacheEvents: f.CacheEvents,
		CommittedSequence: f.CommittedSequence, ElapsedMS: f.ElapsedMS,
		WithinLatency: f.WithinLatency, LatencyLimitMS: f.LatencyLimitMS, Reason: f.Reason,
	}
	for _, c := range f.Chunks {
		v.Chunks = append(v.Chunks, ChunkView{
			ChunkX: c.ChunkX, ChunkY: c.ChunkY, X0: c.X0, Y0: c.Y0,
			Width: c.Width, Height: c.Height, Status: c.Status,
			CenterX: c.CenterX, CenterY: c.CenterY, CenterColor: c.CenterColor,
			CenterTransmittance: c.CenterTransmittance, CenterScattering: c.CenterScattering,
			RayDigest: c.RayDigest, CacheKey: c.CacheKey, ContentHash: c.ContentHash,
			CommitSeq: c.CommitSeq, StepsUsed: c.StepsUsed,
			ShadowSamplesUsed: c.ShadowSamplesUsed, RayBufferFlushes: c.RayBufferFlushes,
			Error: c.Error,
		})
	}
	return v
}

type FaultReport struct {
	Kind    string      `json:"kind"`
	OK      bool        `json:"ok"`
	Error   *FogError   `json:"error,omitempty"`
	Chunk   *ChunkCoord `json:"chunk,omitempty"`
	RetryOK bool        `json:"retry_ok,omitempty"`
	Detail  string      `json:"detail"`
}

type ThreePathReport struct {
	Passed            bool           `json:"passed"`
	Error             string         `json:"error,omitempty"`
	FirstMismatch     *FogError      `json:"first_mismatch,omitempty"`
	FullSequence      []int64        `json:"full_sequence"`
	RealtimeSequence  []int64        `json:"realtime_sequence"`
	ResumeSequence    []int64        `json:"resume_sequence"`
	FullElapsedMS     float64        `json:"full_elapsed_ms"`
	RealtimeElapsedMS float64        `json:"realtime_elapsed_ms"`
	ResumeElapsedMS   float64        `json:"resume_elapsed_ms"`
	RealtimeBudget    BudgetSnapshot `json:"realtime_budget"`
	ResumeCacheEvents []CacheEvent   `json:"resume_cache_events"`
	Chunks            int            `json:"chunks"`
}

type Workbench struct {
	mu        sync.Mutex
	Scene     *Scene
	Store     store.Store
	Renderer  *Renderer
	DBPath    string
	OutputDir string

	LastFrame         *Frame
	LastPreview       *Frame
	LastExact         *Frame
	PreviewExactError *FogError
	CacheStats        CacheStats
	Invalidations     []Invalidation
	TruthStale        bool
	LastFault         *FaultReport
}

func NewWorkbench(scene *Scene, dbPath, outputDir string) (*Workbench, error) {
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	wb := &Workbench{
		Scene: scene.Clone(), Store: st, Renderer: NewRenderer(st),
		DBPath: dbPath, OutputDir: outputDir,
	}
	if err := wb.writeSceneSnapshot(); err != nil {
		st.Close()
		return nil, err
	}
	wb.loadGraphFromStore()
	return wb, nil
}

func (w *Workbench) Close() error {
	if w.Store != nil {
		return w.Store.Close()
	}
	return nil
}

func (w *Workbench) writeSceneSnapshot() error {
	data, err := MarshalSceneJSON(w.Scene)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.OutputDir, "scene.json"), data, 0o644)
}

func (w *Workbench) loadGraphFromStore() {
	records, err := w.Store.ListChunks()
	if err != nil {
		return
	}
	specs := enumerateChunks(w.Scene)
	byCoord := map[ChunkCoord]chunkSpec{}
	for _, s := range specs {
		byCoord[s.Coord] = s
	}
	for _, rec := range records {
		spec, ok := byCoord[ChunkCoord{X: rec.ChunkX, Y: rec.ChunkY}]
		if !ok {
			continue
		}
		res, valid, _ := cacheRecordValid(w.Scene, rec, spec)
		if valid {
			w.Renderer.Graph.RecordChunk(spec.Coord, res.RayDeps, res.CacheKey)
		}
	}
}

func (w *Workbench) State() WorkbenchState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateLocked()
}

func (w *Workbench) stateLocked() WorkbenchState {
	cam := CameraSample{}
	if c := w.Scene.ActiveCameraSample(); c != nil {
		cam = *c
	}
	st := WorkbenchState{
		Seed: w.Scene.Seed, SceneVersion: w.Scene.SnapshotVersion(),
		MediaVersion: w.Scene.MediaVersion(), CameraVersion: w.Scene.CameraVersion(),
		RecipeVersion: w.Scene.RecipeVersion(), ChunkSize: w.Scene.ChunkSize,
		Camera: cam, Recipe: w.Scene.Recipe, LastFrame: frameView(w.LastFrame),
		PreviewAvailable: w.LastPreview != nil, ExactAvailable: w.LastExact != nil,
		PreviewExactError: w.PreviewExactError, CacheStats: w.CacheStats,
		Invalidations: append([]Invalidation(nil), w.Invalidations...),
		Graph:         w.Renderer.Graph.Stats(), TruthAvailable: w.Scene.Truth != nil,
		TruthStale: w.TruthStale, LastFault: w.LastFault,
		DBPath: w.DBPath, OutputDir: w.OutputDir,
	}
	if w.Store != nil {
		st.DBBackend = w.Store.Backend()
	}
	for i := range w.Scene.Media {
		m := &w.Scene.Media[i]
		st.Media = append(st.Media, MediaView{
			ID: m.ID, MinQ: m.MinQ, MaxQ: m.MaxQ, DensityRes: m.DensityRes,
			Density: append([]int(nil), m.Density...), AbsorptionQ: m.AbsorptionQ,
			ScatteringQ: m.ScatteringQ, AnisotropyQ: m.AnisotropyQ, ContentHash: m.ContentHash(),
		})
	}
	return st
}

func (w *Workbench) updateFrameStats(frame *Frame) {
	if frame == nil {
		return
	}
	w.CacheStats.CacheHits += frame.Stats.CacheHits
	w.CacheStats.RecomputedChunks += frame.Stats.ComputedChunks
	w.CacheStats.InvalidatedChunks += frame.Stats.InvalidatedChunks
	w.CacheStats.CommitsWritten += frame.Stats.CommitsWritten
	w.CacheStats.FailedChunks += frame.Stats.FailedChunks
}

func (w *Workbench) Preview() (*Frame, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.previewLocked()
}

func (w *Workbench) previewLocked() (*Frame, error) {
	if err := ValidateScene(w.Scene); err != nil {
		return nil, err
	}
	frame, err := w.Renderer.Render(w.Scene, RenderOptions{
		Mode: "preview", Steps: w.Scene.Recipe.PreviewSteps,
		ShadowSamples: w.Scene.Recipe.PreviewShadowSamples,
		MaxParallel:   w.Scene.Recipe.MaxParallel, RayBuffer: w.Scene.Recipe.RayBuffer,
		Persist: false, UseCache: false,
	})
	if frame != nil {
		frame.Reason = "interactive preview: reduced step and shadow budgets"
		w.LastFrame = frame
		w.LastPreview = frame
		w.PreviewExactError = nil
		w.updateFrameStats(frame)
		if !frame.WithinLatency {
			return frame, &FogError{Code: E_BLOCK,
				Message:   "preview exceeded declared latency cap",
				Deviation: floatPtr(frame.ElapsedMS),
				Tolerance: floatPtr(float64(frame.LatencyLimitMS))}
		}
	}
	return frame, err
}

func (w *Workbench) Exact() (*Frame, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exactLocked(nil)
}

func (w *Workbench) exactLocked(fail map[ChunkCoord]string) (*Frame, error) {
	if err := ValidateScene(w.Scene); err != nil {
		return nil, err
	}
	frame, err := w.Renderer.Render(w.Scene, RenderOptions{
		Mode: "exact", Steps: w.Scene.Recipe.Steps,
		ShadowSamples: w.Scene.Recipe.ShadowSamples,
		MaxParallel:   w.Scene.Recipe.MaxParallel, RayBuffer: w.Scene.Recipe.RayBuffer,
		Persist: true, UseCache: true, FailChunks: fail,
	})
	if frame != nil {
		frame.Reason = "full precision integration with persistent checkpoints"
		w.LastFrame = frame
		w.LastExact = frame
		if w.LastPreview != nil && fail == nil {
			if div := ComparePreviewExact(w.LastPreview, frame); div != nil {
				w.PreviewExactError = asFogError(div)
				err = div
			} else {
				w.PreviewExactError = nil
			}
		}
		w.updateFrameStats(frame)
	}
	return frame, err
}

func (w *Workbench) Resume() (*Frame, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ValidateScene(w.Scene); err != nil {
		return nil, err
	}
	frame, err := w.Renderer.Render(w.Scene, RenderOptions{
		Mode: "resume", Steps: w.Scene.Recipe.Steps,
		ShadowSamples: w.Scene.Recipe.ShadowSamples,
		MaxParallel:   w.Scene.Recipe.MaxParallel, RayBuffer: w.Scene.Recipe.RayBuffer,
		Persist: true, UseCache: true,
	})
	if frame != nil {
		frame.Reason = "resume path validates and reuses durable chunk checkpoints"
		w.LastFrame = frame
		w.updateFrameStats(frame)
	}
	return frame, err
}

func (w *Workbench) ApplyVoxelEdit(mediaID string, x, y, z, value int) ([]Invalidation, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applyVoxelEditLocked(mediaID, x, y, z, value)
}

func (w *Workbench) applyVoxelEditLocked(mediaID string, x, y, z, value int) ([]Invalidation, error) {
	m, _ := w.Scene.MediumByID(mediaID)
	if m == nil {
		return nil, schemaErr(mediaID, "id", "medium not found")
	}
	if x < 0 || y < 0 || z < 0 || x >= m.DensityRes[0] || y >= m.DensityRes[1] || z >= m.DensityRes[2] {
		return nil, codeErr(E_DENS, mediaID, "density", "voxel coordinate is outside density_res")
	}
	if value < 0 || value > MaxDensity {
		return nil, codeErr(E_DENS, mediaID, "density", "voxel value is outside [0,255]")
	}
	idx := m.DensityIndex(x, y, z)
	m.Density[idx] = value
	m.RefreshDensityEncoding()
	w.TruthStale = true
	w.Scene.Truth = nil
	w.LastPreview = nil
	invs := w.Renderer.Graph.InvalidateSource(fmt.Sprintf("density:%s:%d", mediaID, idx), mediaID,
		fmt.Sprintf("density voxel (%d,%d,%d) changed to %d", x, y, z, value))
	w.Invalidations = invs
	return invs, nil
}

func (w *Workbench) ApplyScatteringEdit(mediaID string, valueQ int64) ([]Invalidation, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applyScatteringEditLocked(mediaID, valueQ)
}

func (w *Workbench) applyScatteringEditLocked(mediaID string, valueQ int64) ([]Invalidation, error) {
	m, _ := w.Scene.MediumByID(mediaID)
	if m == nil {
		return nil, schemaErr(mediaID, "id", "medium not found")
	}
	if valueQ < 0 || valueQ > 16*CoeffScale {
		return nil, schemaErr(mediaID, "scattering_q", "scattering coefficient is outside fixed-point range")
	}
	m.ScatteringQ = valueQ
	w.TruthStale = true
	w.Scene.Truth = nil
	w.LastPreview = nil
	invs := w.Renderer.Graph.InvalidatePrefix("scattering:"+mediaID, mediaID,
		fmt.Sprintf("scattering coefficient changed to %d", valueQ))
	w.Invalidations = invs
	return invs, nil
}

func (w *Workbench) ValidateTruth() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Scene.Truth == nil {
		return schemaErr("", "truth", "truth is unavailable or stale after an edit")
	}
	return ValidateTruth(w.Scene)
}

func (w *Workbench) DiagnoseFault(kind string, cx, cy int) *FaultReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	rep := &FaultReport{Kind: kind}
	switch kind {
	case "decode":
		clone := w.Scene.Clone()
		if len(clone.Media) == 0 {
			rep.Error = schemaErr("", "media", "no medium")
			return rep
		}
		raw := clone.Media[0].DensityRawCopy()
		if len(raw) == 0 {
			raw = []byte{1}
		}
		raw[0] ^= 0xFF
		clone.Media[0].setDensityRaw(raw)
		err := ValidateScene(clone)
		rep.Error = asFogError(err)
		rep.Detail = "flipped one encoded density byte; checksum should reject the medium payload"
	case "order":
		clone := w.Scene.Clone()
		if len(clone.Cameras) < 2 {
			rep.Error = schemaErr("camera", "time_us", "need at least two camera samples")
			return rep
		}
		clone.Cameras[1].TimeUS = clone.Cameras[0].TimeUS
		rep.Error = asFogError(ValidateScene(clone))
		rep.Detail = "forced camera sample 1 time to equal sample 0"
	case "dens":
		clone := w.Scene.Clone()
		if len(clone.Media) == 0 || len(clone.Media[0].Density) == 0 {
			rep.Error = schemaErr("", "density", "no voxel available")
			return rep
		}
		clone.Media[0].Density[0] = MaxDensity + 1
		rep.Error = asFogError(ValidateScene(clone))
		rep.Detail = "set first voxel to 256"
	case "block":
		tmp, err := os.MkdirTemp("", "volumefog-block-*")
		if err != nil {
			rep.Error = withCause(E_BLOCK, "temp dir failed", err)
			return rep
		}
		defer os.RemoveAll(tmp)
		st, err := store.Open(filepath.Join(tmp, "block.db"))
		if err != nil {
			rep.Error = withCause(E_BLOCK, "store open failed", err)
			return rep
		}
		defer st.Close()
		renderer := NewRenderer(st)
		specs := enumerateChunks(w.Scene)
		if len(specs) == 0 {
			rep.Error = &FogError{Code: E_BLOCK, Message: "no chunks"}
			return rep
		}
		target := specs[len(specs)-1].Coord
		if cx >= 0 && cy >= 0 {
			valid := false
			for _, s := range specs {
				if s.Coord.X == cx && s.Coord.Y == cy {
					target, valid = s.Coord, true
					break
				}
			}
			if !valid {
				rep.Error = &FogError{Code: E_BLOCK, Message: "requested chunk is outside the viewport",
					Chunk: &ChunkCoord{X: cx, Y: cy}}
				return rep
			}
		}
		frame, err := renderer.Render(w.Scene, RenderOptions{
			Mode: "fault-block", Persist: true, UseCache: true,
			FailChunks: map[ChunkCoord]string{target: "injected single-chunk failure"},
		})
		rep.Chunk = &target
		rep.Error = asFogError(err)
		rep.Detail = fmt.Sprintf("one chunk failed while %d other chunks committed", countOK(frame))
		retry, rerr := renderer.Render(w.Scene, RenderOptions{Mode: "fault-retry", Persist: true, UseCache: true})
		rep.RetryOK = rerr == nil && retry != nil && allChunksOK(retry)
		if !rep.RetryOK && rerr != nil {
			rep.Detail += "; retry: " + rerr.Error()
		}
	default:
		rep.Error = schemaErr("", "fault", "unknown fault kind")
	}
	rep.OK = rep.Error == nil
	w.LastFault = rep
	return rep
}

func asFogError(err error) *FogError {
	if err == nil {
		return nil
	}
	if fe, ok := err.(*FogError); ok {
		return fe
	}
	return withCause(E_BLOCK, err.Error(), err)
}

func countOK(frame *Frame) int {
	n := 0
	if frame != nil {
		for _, c := range frame.Chunks {
			if c.Status != "failed" {
				n++
			}
		}
	}
	return n
}

func allChunksOK(frame *Frame) bool {
	if frame == nil || len(frame.Chunks) == 0 {
		return false
	}
	for _, c := range frame.Chunks {
		if c.Status == "failed" {
			return false
		}
	}
	return true
}

// CompareThreePaths renders into independent databases and verifies exact
// per-pixel, per-chunk and commit-sequence identity.
func (w *Workbench) CompareThreePaths() *ThreePathReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	report := &ThreePathReport{}
	tmp, err := os.MkdirTemp("", "volumefog-paths-*")
	if err != nil {
		report.Error = err.Error()
		return report
	}
	defer os.RemoveAll(tmp)
	scene := w.Scene.Clone()
	scene.Truth = nil
	// One-shot full path.
	fullStore, err := store.Open(filepath.Join(tmp, "full.db"))
	if err != nil {
		report.Error = err.Error()
		return report
	}
	fullRenderer := NewRenderer(fullStore)
	full, ferr := fullRenderer.Render(scene, RenderOptions{
		Mode: "full", Persist: true, UseCache: false,
		Steps: scene.Recipe.Steps, ShadowSamples: scene.Recipe.ShadowSamples,
	})
	fullStore.Close()
	if ferr != nil {
		report.Error = ferr.Error()
		report.FirstMismatch = asFogError(ferr)
		return report
	}
	report.FullSequence = append([]int64(nil), full.CommittedSequence...)
	report.FullElapsedMS = full.ElapsedMS
	// Real-time path: same math, constrained scheduler.
	rtPath := filepath.Join(tmp, "realtime.db")
	rtStore, err := store.Open(rtPath)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	rtRenderer := NewRenderer(rtStore)
	rt, rerr := rtRenderer.Render(scene, RenderOptions{
		Mode: "realtime", Persist: true, UseCache: false,
		Steps: scene.Recipe.Steps, ShadowSamples: scene.Recipe.ShadowSamples,
		MaxParallel: 2, RayBuffer: 2,
	})
	rtStore.Close()
	if rerr != nil {
		report.Error = rerr.Error()
		report.FirstMismatch = asFogError(rerr)
		return report
	}
	report.RealtimeSequence = append([]int64(nil), rt.CommittedSequence...)
	report.RealtimeElapsedMS = rt.ElapsedMS
	report.RealtimeBudget = rt.Budget
	// Simulated restart: truncate suffix, reopen, resume.
	rtStore, err = store.Open(rtPath)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	cut := int64(len(rt.Chunks) / 2)
	if err := rtStore.TruncateFrom(cut); err != nil {
		rtStore.Close()
		report.Error = err.Error()
		return report
	}
	rtStore.Close()
	rsStore, err := store.Open(rtPath)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	rsRenderer := NewRenderer(rsStore)
	rs, serr := rsRenderer.Render(scene, RenderOptions{
		Mode: "resume", Persist: true, UseCache: true,
		Steps: scene.Recipe.Steps, ShadowSamples: scene.Recipe.ShadowSamples,
	})
	rsStore.Close()
	if serr != nil {
		report.Error = serr.Error()
		report.FirstMismatch = asFogError(serr)
		return report
	}
	report.ResumeSequence = append([]int64(nil), rs.CommittedSequence...)
	report.ResumeElapsedMS = rs.ElapsedMS
	report.ResumeCacheEvents = append([]CacheEvent(nil), rs.CacheEvents...)
	report.Chunks = len(full.Chunks)
	if err := CompareFrames(full, rt); err != nil {
		report.Error = err.Error()
		report.FirstMismatch = asFogError(err)
		return report
	}
	if err := CompareFrames(full, rs); err != nil {
		report.Error = err.Error()
		report.FirstMismatch = asFogError(err)
		return report
	}
	if err := CompareCommitSequences(full.CommittedSequence, rt.CommittedSequence); err != nil {
		report.Error = err.Error()
		report.FirstMismatch = asFogError(err)
		return report
	}
	if err := CompareCommitSequences(full.CommittedSequence, rs.CommittedSequence); err != nil {
		report.Error = err.Error()
		report.FirstMismatch = asFogError(err)
		return report
	}
	report.Passed = true
	return report
}

func (w *Workbench) EditAndRecomputeVoxel(mediaID string, x, y, z, value int) ([]Invalidation, *Frame, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	invs, err := w.applyVoxelEditLocked(mediaID, x, y, z, value)
	if err != nil {
		return nil, nil, err
	}
	frame, err := w.exactLocked(nil)
	return invs, frame, err
}

func (w *Workbench) EditAndRecomputeScattering(mediaID string, valueQ int64) ([]Invalidation, *Frame, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	invs, err := w.applyScatteringEditLocked(mediaID, valueQ)
	if err != nil {
		return nil, nil, err
	}
	frame, err := w.exactLocked(nil)
	return invs, frame, err
}

func (w *Workbench) SaveReport(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.OutputDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.OutputDir, name), data, 0o644)
}

func (w *Workbench) ReopenStore() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.Store.Close(); err != nil {
		return err
	}
	st, err := store.Open(w.DBPath)
	if err != nil {
		return err
	}
	w.Store = st
	w.Renderer = NewRenderer(st)
	w.loadGraphFromStore()
	return nil
}

func (w *Workbench) SortedChunkCoords() []ChunkCoord {
	specs := enumerateChunks(w.Scene)
	out := make([]ChunkCoord, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Coord)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}

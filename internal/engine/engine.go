package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"volumediag/internal/diag"
	"volumediag/internal/model"
	"volumediag/internal/store"
	"volumediag/internal/volume"
)

type pendingInvalidation struct {
	Reason          string
	Stage           string
	Depth           int
	SourceLights    []string
	SourceOccluders []string
}

type blockRuntime struct {
	State           string
	Invalidated     bool
	Reasons         []string
	SourceLights    []string
	SourceOccluders []string
	DurationMS      float64
	CacheHit        bool
	Failed          bool
}

type Engine struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	bundle        *model.Bundle
	scene         *model.Scene
	params        volume.Params
	cameraIndex   int
	versions      model.Versions
	contentHash   string
	store         *store.Store
	dbPath        string
	cache         map[volume.BlockKey]*volume.BlockResult
	pending       map[volume.BlockKey]pendingInvalidation
	status        map[volume.BlockKey]*blockRuntime
	failures      map[volume.BlockKey]*diag.Error
	coverage      map[string]map[volume.BlockKey]struct{}
	blockGeometry map[volume.BlockKey]map[string]struct{}
	paused        atomic.Bool
	restore       store.Restore
	lastReport    *RenderReport
	lastTruth     *TruthReport
}

func Open(bundle *model.Bundle, p volume.Params, dbPath string) (*Engine, error) {
	if bundle == nil || bundle.Scene == nil || bundle.Truth == nil {
		return nil, diag.New(diag.ESchema, "bundle, scene and truth are required")
	}
	scene := bundle.Scene.Clone()
	if scene == nil {
		return nil, diag.New(diag.EIO, "cannot clone scene")
	}
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	if p.CameraID == "" {
		p.CameraID = scene.ActiveCameraID
	}
	if err := p.Validate(scene); err != nil {
		return nil, err
	}
	cameraIndex := cameraIndexByID(scene, p.CameraID)
	if cameraIndex < 0 {
		return nil, &diag.Error{Code: diag.ERef, Field: "params.camera_id", Message: "camera does not exist"}
	}
	versions := scene.Versions()
	coverage, err := computeLightCoverage(scene, cameraIndex, p)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		bundle:      bundle,
		scene:       scene,
		params:      p,
		cameraIndex: cameraIndex,
		versions:    versions,
		contentHash: scene.ComputeHash(),
		dbPath:      dbPath,
		cache:       make(map[volume.BlockKey]*volume.BlockResult),
		pending:     make(map[volume.BlockKey]pendingInvalidation),
		status:      make(map[volume.BlockKey]*blockRuntime),
		failures:    make(map[volume.BlockKey]*diag.Error),
		coverage:    coverage,
	}
	e.blockGeometry = computeBlockGeometry(scene, cameraIndex, coverage)
	for _, key := range e.allKeys() {
		e.status[key] = &blockRuntime{State: "pending"}
	}
	if dbPath != "" {
		st, restore, openErr := store.Open(dbPath, e.manifest())
		if openErr != nil {
			return nil, openErr
		}
		e.store = st
		e.restore = restore
		valid := true
		for _, rec := range restore.Blocks {
			result, decodeErr := e.decodeRecord(rec)
			if decodeErr != nil {
				valid = false
				break
			}
			e.cache[result.Key] = result
			e.status[result.Key] = &blockRuntime{
				State:        "committed",
				DurationMS:   0,
				Invalidated:  false,
				SourceLights: volume.SortStrings(keysOf(result.LightVersions)),
			}
		}
		if !valid {
			if wipeErr := st.WipeSnapshot(); wipeErr != nil {
				_ = st.Close()
				return nil, wipeErr
			}
			e.cache = make(map[volume.BlockKey]*volume.BlockResult)
			e.restore.Invalidated = true
			e.restore.Reason = "restored_block_digest_mismatch"
			for _, key := range e.allKeys() {
				e.status[key] = &blockRuntime{State: "pending"}
			}
		}
	}
	return e, nil
}

func (e *Engine) decodeRecord(rec store.BlockRecord) (*volume.BlockResult, error) {
	var result volume.BlockResult
	if err := json.Unmarshal(rec.Payload, &result); err != nil {
		return nil, diag.Wrap(diag.EDecode, "decode persisted block", err)
	}
	if result.Key.CameraIndex != rec.CameraIndex || result.Key.CameraID != rec.CameraID ||
		result.Key.X != rec.X || result.Key.Y != rec.Y {
		return nil, diag.New(diag.EDecode, "persisted block key mismatch")
	}
	if result.ParamsVersion != rec.ParamsVersion || result.AlgorithmVersion != rec.AlgorithmVersion ||
		result.MediumVersion != rec.MediumVersion || result.GeometryVersion != rec.GeometryVersion {
		return nil, diag.New(diag.EDecode, "persisted block version mismatch")
	}
	if result.Digest != rec.Digest || result.ComputeDigest() != rec.Digest {
		return nil, diag.New(diag.EDecode, "persisted block digest mismatch")
	}
	for id, version := range result.LightVersions {
		if e.versions.LightVersions[id] != version {
			return nil, &diag.Error{Code: diag.EDecode, LightID: id, Message: "persisted block light version mismatch"}
		}
	}
	if result.GeometryVersion != e.blockGeometryVersion(result.Key) {
		return nil, diag.New(diag.EDecode, "persisted block geometry dependency mismatch")
	}
	return &result, nil
}

func (e *Engine) manifest() store.Manifest {
	return store.Manifest{
		SourceHash:       e.bundle.SourceHash,
		SceneHash:        e.contentHash,
		ParamsVersion:    e.params.Version(),
		AlgorithmVersion: volume.AlgorithmVersion,
		MediumVersion:    e.versions.MediumVersion,
		GeometryVersion:  e.versions.GeometryVersion,
		LightVersions:    cloneStringMap(e.versions.LightVersions),
		BatchSize:        e.params.BatchSize,
	}
}

func (e *Engine) allKeys() []volume.BlockKey {
	var keys []volume.BlockKey
	camera := &e.scene.CameraPath[e.cameraIndex]
	for by := 0; by < e.scene.BlockRows(); by++ {
		for bx := 0; bx < e.scene.BlockCols(); bx++ {
			keys = append(keys, volume.BlockKey{
				CameraIndex: e.cameraIndex,
				CameraID:    camera.ID,
				X:           bx,
				Y:           by,
			})
		}
	}
	sortBlockKeys(keys)
	return keys
}

func (e *Engine) SetParams(p volume.Params) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.setParamsLocked(p)
}

func (e *Engine) setParamsLocked(p volume.Params) error {
	if p.CameraID == "" {
		p.CameraID = e.scene.ActiveCameraID
	}
	if err := p.Validate(e.scene); err != nil {
		return err
	}
	if p.CameraID != e.params.CameraID {
		return diag.Parameter("", "params.camera_id", "camera switching requires a new engine snapshot")
	}
	if p.Version() == e.params.Version() {
		e.mu.Lock()
		e.params = p
		e.mu.Unlock()
		return nil
	}
	e.mu.Lock()
	e.params = p
	for _, key := range e.allKeys() {
		e.markPendingLocked(key, pendingInvalidation{
			Reason: "params_changed",
			Stage:  "volume",
			Depth:  0,
		})
	}
	e.mu.Unlock()
	if e.store != nil {
		if err := e.store.SetComplete(e.manifest(), false); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Render(ctx context.Context, mode Mode, opts RenderOptions) (*RenderReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.params.Validate(e.scene); err != nil {
		return nil, err
	}
	started := time.Now()
	report := &RenderReport{
		Mode:            mode,
		Preview:         e.params.Preview || mode == ModePreview,
		StartedAt:       started.Format(time.RFC3339Nano),
		RestoredBlocks:  len(e.restore.Blocks),
		SnapshotInvalid: e.restore.Invalidated,
		SnapshotReason:  e.restore.Reason,
		Quality:         volume.ErrorBound(e.scene, &e.scene.CameraPath[e.cameraIndex], e.params),
	}
	if mode == ModeFull || opts.ForceFull {
		e.mu.Lock()
		for _, key := range e.allKeys() {
			e.markPendingLocked(key, pendingInvalidation{
				Reason: "full_rebuild",
				Stage:  "volume",
				Depth:  0,
			})
		}
		e.mu.Unlock()
	}
	// A resume/incremental/preview run also schedules anything absent from the
	// cache. This is what makes restart after a forced termination converge.
	e.mu.Lock()
	for _, key := range e.allKeys() {
		if _, ok := e.cache[key]; !ok {
			if _, pending := e.pending[key]; !pending {
				reason := "missing_cache"
				if mode == ModeResume {
					reason = "resume_missing_batch"
				}
				e.markPendingLocked(key, pendingInvalidation{Reason: reason, Stage: "volume", Depth: 0})
			}
		}
	}
	report.Affected = e.invalidationListLocked()
	keys := e.pendingKeysLocked()
	all := e.allKeys()
	for _, key := range all {
		if _, pending := e.pending[key]; !pending {
			if _, cached := e.cache[key]; cached {
				report.ReusedBlockKeys = append(report.ReusedBlockKeys, key)
				if st := e.status[key]; st != nil {
					st.CacheHit = true
				}
			}
		}
	}
	e.mu.Unlock()
	report.RequestedBlocks = len(keys)

	if len(keys) == 0 {
		e.mu.Lock()
		report.Complete = e.isCompleteLocked()
		e.mu.Unlock()
		if e.store != nil {
			if err := e.store.SetComplete(e.manifest(), report.Complete); err != nil {
				return report, err
			}
		}
		report.ReusedBlocks = len(report.ReusedBlockKeys)
		report.DurationMS = float64(time.Since(started).Microseconds()) / 1000
		e.mu.Lock()
		e.lastReport = report
		e.mu.Unlock()
		return report, nil
	}

	batchSize := e.params.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}
	batchGate := make(chan struct{}, e.params.BufferSlots)
	bufferEvents := make([]string, 0)
	var bufferMu sync.Mutex
	paused := false
	batchCount := 0
	for start := 0; start < len(keys); start += batchSize {
		end := min(start+batchSize, len(keys))
		batch := keys[start:end]
		if pausedNow := e.paused.Load(); pausedNow || ctx.Err() != nil {
			paused = pausedNow || ctx.Err() != nil
			break
		}
		computed := e.computeBatch(ctx, batch, batchGate, &bufferEvents, &bufferMu)
		batchFailures := false
		for _, item := range computed {
			if item.err != nil {
				batchFailures = true
				e.mu.Lock()
				e.failures[item.key] = item.err
				if st := e.status[item.key]; st != nil {
					st.State = "failed"
					st.Failed = true
					st.DurationMS = item.duration
				}
				e.mu.Unlock()
				report.Failures = append(report.Failures, BlockFailure{
					CameraID:  item.key.CameraID,
					BlockX:    item.key.X,
					BlockY:    item.key.Y,
					Code:      item.err.Code,
					Message:   item.err.Error(),
					Retryable: item.err.Code != diag.ECanceled,
				})
				if item.err.Code == diag.ECanceled {
					paused = true
				}
			}
		}
		if batchFailures {
			if paused {
				break
			}
			continue
		}
		records := make([]store.BlockRecord, 0, len(computed))
		for _, item := range computed {
			records = append(records, e.recordFor(item.key, item.result))
		}
		e.mu.RLock()
		noFailures := len(e.failures) == 0
		e.mu.RUnlock()
		complete := start+len(batch) == len(keys) && noFailures
		if e.store != nil {
			if _, err := e.store.CommitBatch(records, e.manifest(), complete); err != nil {
				return report, err
			}
		}
		e.mu.Lock()
		for _, item := range computed {
			e.cache[item.key] = item.result
			delete(e.pending, item.key)
			delete(e.failures, item.key)
			if st := e.status[item.key]; st != nil {
				st.State = "computed"
				st.Invalidated = false
				st.CacheHit = false
				st.Reasons = nil
				st.SourceLights = nil
				st.SourceOccluders = nil
				st.DurationMS = item.duration
				st.Failed = false
			}
		}
		e.mu.Unlock()
		report.ComputedBlocks += len(computed)
		report.BatchesCommitted++
		batchCount++
		if opts.PauseAfterBatches > 0 && batchCount >= opts.PauseAfterBatches && end < len(keys) {
			paused = true
			break
		}
	}
	report.Paused = paused
	e.mu.Lock()
	report.Complete = e.isCompleteLocked()
	e.mu.Unlock()
	report.ReusedBlocks = len(report.ReusedBlockKeys)
	report.BufferEvents = bufferEvents
	report.BufferWaitCount = len(bufferEvents)
	report.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	e.mu.Lock()
	e.lastReport = report
	e.mu.Unlock()
	if err := ctx.Err(); err != nil && !report.Complete {
		return report, &diag.Error{Code: diag.ECanceled, Message: "render stopped at batch boundary", Cause: err}
	}
	return report, nil
}

type computedBlock struct {
	key      volume.BlockKey
	result   *volume.BlockResult
	err      *diag.Error
	duration float64
}

func (e *Engine) computeBatch(ctx context.Context, keys []volume.BlockKey, gate chan struct{},
	events *[]string, eventMu *sync.Mutex) []computedBlock {
	out := make([]computedBlock, len(keys))
	workers := e.params.MaxWorkers
	if workers > len(keys) {
		workers = len(keys)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				key := keys[idx]
				waited := false
				select {
				case gate <- struct{}{}:
				default:
					waited = true
					select {
					case gate <- struct{}{}:
					case <-ctx.Done():
						out[idx] = computedBlock{key: key, err: &diag.Error{Code: diag.ECanceled, Message: "buffer wait canceled", Cause: ctx.Err()}}
						continue
					}
				}
				if waited {
					eventMu.Lock()
					*events = append(*events, fmt.Sprintf("buffer_wait camera=%s block=(%d,%d): all %d slots busy",
						key.CameraID, key.X, key.Y, e.params.BufferSlots))
					eventMu.Unlock()
				}
				start := time.Now()
				result, renderErr := safeRenderBlock(ctx, e.scene, e.cameraIndex, e.params, key.X, key.Y)
				duration := float64(time.Since(start).Microseconds()) / 1000
				<-gate
				if renderErr != nil {
					out[idx] = computedBlock{key: key, err: renderErr, duration: duration}
					continue
				}
				e.attachVersions(key, result)
				out[idx] = computedBlock{key: key, result: result, duration: duration}
			}
		}()
	}
	for i := range keys {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

func safeRenderBlock(ctx context.Context, scene *model.Scene, cameraIndex int, p volume.Params, bx, by int) (result *volume.BlockResult, derr *diag.Error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			derr = &diag.Error{
				Code:    diag.EBlock,
				BlockX:  bx,
				BlockY:  by,
				Message: fmt.Sprintf("block panic: %v", recovered),
				Cause:   fmt.Errorf("%s", debug.Stack()),
			}
		}
	}()
	return volume.RenderBlock(ctx, scene, cameraIndex, p, bx, by)
}

func (e *Engine) attachVersions(key volume.BlockKey, result *volume.BlockResult) {
	versions := make(map[string]string)
	for lightID, blocks := range e.coverage {
		if _, ok := blocks[key]; ok {
			versions[lightID] = e.versions.LightVersions[lightID]
		}
	}
	result.LightVersions = versions
	result.MediumVersion = e.versions.MediumVersion
	result.GeometryVersion = e.blockGeometryVersion(key)
	result.ParamsVersion = e.params.Version()
	result.AlgorithmVersion = volume.AlgorithmVersion
	result.Digest = result.ComputeDigest()
}

func (e *Engine) blockGeometryVersion(key volume.BlockKey) string {
	deps := make(map[string]string)
	for id := range e.blockGeometry[key] {
		mesh, idx := e.scene.FindMesh(id)
		if idx >= 0 {
			deps[id] = model.HashValue(*mesh)
		}
	}
	return model.HashValue(deps)
}

func (e *Engine) recordFor(key volume.BlockKey, result *volume.BlockResult) store.BlockRecord {
	payload, _ := json.Marshal(result)
	return store.BlockRecord{
		CameraIndex:      key.CameraIndex,
		CameraID:         key.CameraID,
		X:                key.X,
		Y:                key.Y,
		Payload:          payload,
		Digest:           result.Digest,
		ParamsVersion:    result.ParamsVersion,
		AlgorithmVersion: result.AlgorithmVersion,
		MediumVersion:    result.MediumVersion,
		GeometryVersion:  result.GeometryVersion,
		LightVersions:    result.LightVersions,
	}
}

func (e *Engine) pendingKeysLocked() []volume.BlockKey {
	keys := make([]volume.BlockKey, 0, len(e.pending))
	for key := range e.pending {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a := e.pending[keys[i]]
		b := e.pending[keys[j]]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		return keys[i].Less(keys[j])
	})
	return keys
}

func (e *Engine) markPendingLocked(key volume.BlockKey, inv pendingInvalidation) {
	if existing, ok := e.pending[key]; ok {
		existing.Reason = mergeReasons(existing.Reason, inv.Reason)
		existing.SourceLights = uniqueSorted(append(existing.SourceLights, inv.SourceLights...))
		existing.SourceOccluders = uniqueSorted(append(existing.SourceOccluders, inv.SourceOccluders...))
		if inv.Depth > existing.Depth {
			existing.Depth = inv.Depth
		}
		e.pending[key] = existing
	} else {
		e.pending[key] = inv
	}
	if st := e.status[key]; st != nil {
		st.Invalidated = true
		st.Failed = false
		st.CacheHit = false
		st.State = "invalidated"
		st.Reasons = uniqueSorted(append(st.Reasons, inv.Reason))
		st.SourceLights = uniqueSorted(append(st.SourceLights, inv.SourceLights...))
		st.SourceOccluders = uniqueSorted(append(st.SourceOccluders, inv.SourceOccluders...))
	}
}

func mergeReasons(a, b string) string {
	if a == "" {
		return b
	}
	for _, part := range strings.Split(a, "+") {
		if part == b {
			return a
		}
	}
	return a + "+" + b
}

func (e *Engine) invalidationListLocked() []Invalidation {
	var out []Invalidation
	for key, inv := range e.pending {
		_, cached := e.cache[key]
		out = append(out, Invalidation{
			CameraID:         key.CameraID,
			BlockX:           key.X,
			BlockY:           key.Y,
			Stage:            "volume",
			Depth:            0,
			Reason:           inv.Reason,
			SourceLights:     inv.SourceLights,
			SourceOccluders:  inv.SourceOccluders,
			PreviouslyCached: cached,
		})
		out = append(out, Invalidation{
			CameraID:         key.CameraID,
			BlockX:           key.X,
			BlockY:           key.Y,
			Stage:            "blend",
			Depth:            1,
			Reason:           "downstream_mix:" + inv.Reason,
			SourceLights:     inv.SourceLights,
			SourceOccluders:  inv.SourceOccluders,
			PreviouslyCached: cached,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		if out[i].CameraID != out[j].CameraID {
			return out[i].CameraID < out[j].CameraID
		}
		if out[i].BlockY != out[j].BlockY {
			return out[i].BlockY < out[j].BlockY
		}
		if out[i].BlockX != out[j].BlockX {
			return out[i].BlockX < out[j].BlockX
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}

func (e *Engine) isCompleteLocked() bool {
	if len(e.pending) > 0 || len(e.failures) > 0 {
		return false
	}
	for _, key := range e.allKeys() {
		if _, ok := e.cache[key]; !ok {
			return false
		}
	}
	return true
}

func (e *Engine) Pause() { e.paused.Store(true) }

func (e *Engine) Resume() { e.paused.Store(false) }

func (e *Engine) Close() error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	st := e.store
	e.store = nil
	e.mu.Unlock()
	if st != nil {
		return st.Close()
	}
	return nil
}

func (e *Engine) Cache() map[volume.BlockKey]*volume.BlockResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[volume.BlockKey]*volume.BlockResult, len(e.cache))
	for k, v := range e.cache {
		out[k] = v
	}
	return out
}

func (e *Engine) Block(x, y int) (BlockView, *volume.BlockResult, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	key := volume.BlockKey{
		CameraIndex: e.cameraIndex,
		CameraID:    e.scene.CameraPath[e.cameraIndex].ID,
		X:           x,
		Y:           y,
	}
	block, ok := e.cache[key]
	if !ok {
		return BlockView{}, nil, false
	}
	view := BlockView{
		CameraID:   key.CameraID,
		BlockX:     key.X,
		BlockY:     key.Y,
		Digest:     block.Digest,
		UsedLights: append([]string(nil), block.UsedLights...),
		Samples:    block.Stats.SamplesPerPixel,
		ActualStep: block.ActualStep,
	}
	if rt := e.status[key]; rt != nil {
		view.State = rt.State
		view.CacheHit = rt.CacheHit
		view.Invalidated = rt.Invalidated
		view.Reasons = append([]string(nil), rt.Reasons...)
		view.SourceLights = append([]string(nil), rt.SourceLights...)
		view.SourceOccluders = append([]string(nil), rt.SourceOccluders...)
		view.DurationMS = rt.DurationMS
		view.Failed = rt.Failed
	}
	return view, block, true
}

func (e *Engine) LightCoverageBlocks(lightID string) []volume.BlockKey {
	e.mu.RLock()
	defer e.mu.RUnlock()
	blocks := e.coverage[lightID]
	out := make([]volume.BlockKey, 0, len(blocks))
	for key := range blocks {
		out = append(out, key)
	}
	sortBlockKeys(out)
	return out
}

func (e *Engine) PendingCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.pending)
}

func (e *Engine) Retry(ctx context.Context) (*RenderReport, error) {
	e.opMu.Lock()
	e.mu.Lock()
	for key := range e.failures {
		e.markPendingLocked(key, pendingInvalidation{Reason: "retry_failed_block", Stage: "volume", Depth: 0})
	}
	e.mu.Unlock()
	e.opMu.Unlock()
	return e.Render(ctx, ModeIncremental, RenderOptions{})
}

func (e *Engine) State() State {
	e.mu.RLock()
	defer e.mu.RUnlock()
	st := State{
		SourceHash:     e.bundle.SourceHash,
		SceneHash:      e.contentHash,
		SceneVersions:  e.versions,
		Params:         e.params,
		Quality:        volume.ErrorBound(e.scene, &e.scene.CameraPath[e.cameraIndex], e.params),
		Viewport:       e.scene.Viewport,
		BlockSize:      e.scene.BlockSize,
		CameraPath:     append([]model.Camera(nil), e.scene.CameraPath...),
		ActiveCameraID: e.scene.ActiveCameraID,
		Geometry:       append([]model.DepthMesh(nil), e.scene.Geometry...),
		Lights:         append([]model.Light(nil), e.scene.Lights...),
		Medium:         e.scene.Medium,
		CacheCount:     len(e.cache),
		TotalBlocks:    e.scene.BlockRows() * e.scene.BlockCols(),
		PendingCount:   len(e.pending),
		FailedCount:    len(e.failures),
		Complete:       e.isCompleteLocked(),
		Paused:         e.paused.Load(),
		Restored:       e.restore,
		DBPath:         e.dbPath,
	}
	if e.restore.Invalidated {
		st.RestoreSummary = "snapshot invalidated: " + e.restore.Reason
	} else if e.restore.Complete {
		st.RestoreSummary = fmt.Sprintf("restored %d committed blocks in %d batches", len(e.restore.Blocks), e.restore.BatchCount)
	} else if len(e.restore.Blocks) > 0 {
		st.RestoreSummary = fmt.Sprintf("restored partial snapshot: %d blocks, %d batches", len(e.restore.Blocks), e.restore.BatchCount)
	} else {
		st.RestoreSummary = "fresh snapshot"
	}
	for _, key := range e.allKeys() {
		view := BlockView{
			CameraID: key.CameraID,
			BlockX:   key.X,
			BlockY:   key.Y,
		}
		if rt := e.status[key]; rt != nil {
			view.State = rt.State
			view.CacheHit = rt.CacheHit
			view.Invalidated = rt.Invalidated
			view.Reasons = append([]string(nil), rt.Reasons...)
			view.SourceLights = append([]string(nil), rt.SourceLights...)
			view.SourceOccluders = append([]string(nil), rt.SourceOccluders...)
			view.DurationMS = rt.DurationMS
			view.Failed = rt.Failed
		}
		if block := e.cache[key]; block != nil {
			view.Digest = block.Digest
			view.UsedLights = append([]string(nil), block.UsedLights...)
			view.Samples = block.Stats.SamplesPerPixel
			view.ActualStep = block.ActualStep
		}
		st.Blocks = append(st.Blocks, view)
	}
	if e.store != nil {
		if counts, err := e.store.Counts(); err == nil {
			st.Checkpoint = counts
		}
	}
	return st
}

func (e *Engine) SceneSnapshot() *model.Scene {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.scene.Clone()
}

func (e *Engine) Bundle() *model.Bundle {
	return e.bundle
}

func (e *Engine) Params() volume.Params {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.params
}

func (e *Engine) LastReport() *RenderReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastReport
}

func (e *Engine) StorePath() string { return e.dbPath }

func cameraIndexByID(scene *model.Scene, id string) int {
	for i := range scene.CameraPath {
		if scene.CameraPath[i].ID == id {
			return i
		}
	}
	return -1
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"maskpipe/internal/model"
	"maskpipe/internal/sample"
	"maskpipe/internal/store"
	"maskpipe/internal/vision"
)

type RunMode string

const (
	ModeIncremental RunMode = "incremental"
	ModeResume      RunMode = "resume"
	ModeFull        RunMode = "full"
)

type ProgressEvent struct {
	Committed int    `json:"committed"`
	Total     int    `json:"total"`
	Stage     string `json:"stage,omitempty"`
	Chunk     string `json:"chunk,omitempty"`
}

type SequenceItem struct {
	ChunkX         int         `json:"chunk_x"`
	ChunkY         int         `json:"chunk_y"`
	Stage          model.Stage `json:"stage"`
	CanonicalOrder int         `json:"canonical_order"`
	WriteOrder     int64       `json:"write_order"`
	ContentHash    string      `json:"content_hash"`
	ParamVersion   string      `json:"param_version"`
	Downsample     float64     `json:"downsample"`
}

type Result struct {
	Mode                RunMode               `json:"mode"`
	SnapshotID          string                `json:"snapshot_id"`
	SnapshotReset       bool                  `json:"snapshot_reset"`
	SnapshotResetReason string                `json:"snapshot_reset_reason,omitempty"`
	ParamVersion        string                `json:"param_version"`
	Downsample          float64               `json:"downsample"`
	Width               int                   `json:"width"`
	Height              int                   `json:"height"`
	MaskHash            string                `json:"mask_hash"`
	GeometryHash        string                `json:"geometry_hash"`
	Mask                []byte                `json:"-"`
	Regions             []model.Region        `json:"regions"`
	Quality             *vision.RegionReport  `json:"quality,omitempty"`
	Sequence            []SequenceItem        `json:"stage_sequence"`
	Metrics             BudgetMetricsSnapshot `json:"budget_metrics"`
	States              []StageState          `json:"states"`
	StartedAt           time.Time             `json:"started_at"`
	ElapsedMS           int64                 `json:"elapsed_ms"`
	FirstError          *model.Error          `json:"first_error,omitempty"`
}

type Service struct {
	Loaded  *sample.LoadedScene
	Store   *store.Store
	Params  model.ParameterSet
	Budget  model.Budget
	Chunks  []Chunk
	Machine *StateMachine
	Metrics *BudgetMetrics
	Control *Control

	ChunkFilter map[int]bool

	paramsMu sync.RWMutex
}

func NewService(loaded *sample.LoadedScene, st *store.Store, params *model.ParameterSet, control *Control) (*Service, error) {
	p := loaded.Scene.Parameters
	if params != nil {
		p = *params
		if len(p.Seeds) == 0 {
			p.Seeds = loaded.Scene.Parameters.Seeds
		}
	}
	if p.ParamVersion == "" {
		p = vision.WithVersion(p)
	}
	if err := vision.ValidateParameters(p); err != nil {
		return nil, err
	}
	budget := loaded.Scene.Declared.Budget
	if budget.WorkerCount <= 0 {
		budget.WorkerCount = 1
	}
	if budget.PendingTaskQueueBudget <= 0 {
		budget.PendingTaskQueueBudget = budget.WorkerCount
	}
	for _, chunk := range PlanChunks(loaded.Scene.Width, loaded.Scene.Height, loaded.Scene.Tile) {
		if chunk.Pixels*BufferUnitsPerPixel > budget.PixelBufferPerThread {
			return nil, model.NewError(model.CodePara, "declared.budget.pixel_buffer_per_thread",
				"chunk %s needs %d buffered pixels but cap is %d",
				fmt.Sprintf("%d,%d", chunk.X, chunk.Y), chunk.Pixels*BufferUnitsPerPixel,
				budget.PixelBufferPerThread)
		}
	}
	chunks := PlanChunks(loaded.Scene.Width, loaded.Scene.Height, loaded.Scene.Tile)
	if control == nil {
		control = NewControl()
	}
	return &Service{
		Loaded: loaded, Store: st, Params: p, Budget: budget,
		Chunks: chunks, Machine: NewStateMachine(chunks),
		Metrics: NewBudgetMetrics(budget), Control: control,
	}, nil
}

func (s *Service) SnapshotID() string {
	return s.Loaded.Scene.ID + "-" + s.Params.ParamVersion
}

func (s *Service) SnapshotInfo() store.SnapshotInfo {
	scene := s.Loaded.Scene
	paramsBytes, _ := json.Marshal(s.CurrentParams())
	return store.SnapshotInfo{
		ID: s.SnapshotID(), SceneID: scene.ID,
		AlgorithmVersion: vision.AlgorithmVersion, ParamVersion: s.Params.ParamVersion,
		Width: scene.Width, Height: scene.Height, TileWidth: scene.Tile.Width,
		TileHeight: scene.Tile.Height, Seed: s.Loaded.Set.Seed,
		TruthFingerprint: scene.TruthFingerprint, ParamsJSON: string(paramsBytes),
	}
}

func (s *Service) UpdateParameters(p model.ParameterSet) error {
	if len(p.Seeds) == 0 {
		p.Seeds = s.CurrentParams().Seeds
	}
	if p.ParamVersion == "" {
		p = vision.WithVersion(p)
	}
	if err := vision.ValidateParameters(p); err != nil {
		return err
	}
	s.paramsMu.Lock()
	s.Params = p
	s.paramsMu.Unlock()
	return nil
}

func (s *Service) CurrentParams() model.ParameterSet {
	s.paramsMu.RLock()
	defer s.paramsMu.RUnlock()
	return s.Params
}

var ErrRunCanceled = errors.New("run canceled")

type stageTaskResult struct {
	blob         []byte
	geometryJSON string
	err          error
}

type StageSummary struct {
	ChunkX           int     `json:"chunk_x"`
	ChunkY           int     `json:"chunk_y"`
	PixelX           int     `json:"pixel_x"`
	PixelY           int     `json:"pixel_y"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	Encoding         string  `json:"encoding"`
	ForegroundPixels int     `json:"foreground_pixels,omitempty"`
	BufferPixels     int     `json:"buffer_pixels"`
	Downsample       float64 `json:"downsample"`
	Note             string  `json:"note,omitempty"`
}

func (s *Service) Run(ctx context.Context, mode RunMode, progress func(ProgressEvent)) (*Result, error) {
	start := time.Now()
	info := s.SnapshotInfo()
	result := &Result{Mode: mode, SnapshotID: info.ID, ParamVersion: info.ParamVersion, Downsample: 1}

	if s.Store != nil {
		if mode == ModeFull {
			if err := s.Store.PurgeSnapshot(ctx, info.ID); err != nil {
				return nil, err
			}
		}
		validation := ValidateSnapshot(ctx, s.Store, info, info)
		if !validation.Valid {
			result.SnapshotReset = true
			result.SnapshotResetReason = validation.Reason
			if validation.Violation != nil {
				b, _ := json.Marshal(validation.Violation)
				result.SnapshotResetReason += ": " + string(b)
			}
			if err := s.Store.PurgeSnapshot(ctx, info.ID); err != nil {
				return nil, err
			}
		}
		if err := s.Store.InitSnapshot(ctx, info); err != nil {
			return nil, err
		}
	}

	existing := map[model.Stage]map[int]store.StageRecord{}
	if mode != ModeFull && s.Store != nil {
		validation := ValidateSnapshot(ctx, s.Store, info, info)
		if validation.Valid {
			byStage := map[model.Stage]map[int]store.StageRecord{}
			coordIndex := map[[2]int]int{}
			for _, c := range s.Chunks {
				coordIndex[[2]int{c.X, c.Y}] = c.Index
			}
			for _, rec := range validation.Records {
				idx := coordIndex[[2]int{rec.ChunkX, rec.ChunkY}]
				if byStage[rec.Stage] == nil {
					byStage[rec.Stage] = map[int]store.StageRecord{}
				}
				byStage[rec.Stage][idx] = rec
				key := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: rec.ChunkX, Y: rec.ChunkY}, Stage: rec.Stage}
				s.Machine.MarkExistingCommitted(key, rec.ParamVersion)
			}
			existing = byStage
		}
	}

	seedErr := s.validateSeeds()
	if seedErr != nil {
		result.FirstError = seedErr
	}

	growBlobs := make([][]byte, len(s.Chunks))
	cleanBlobs := make([][]byte, len(s.Chunks))
	statsJSON := make([]string, len(s.Chunks))
	stageErrs := map[model.Stage][]error{
		model.StageGrow:  make([]error, len(s.Chunks)),
		model.StageClean: make([]error, len(s.Chunks)),
		model.StageStats: make([]error, len(s.Chunks)),
	}

	if recs := existing[model.StageGrow]; recs != nil {
		for idx, rec := range recs {
			growBlobs[idx] = append([]byte(nil), rec.Blob...)
		}
	}
	if recs := existing[model.StageClean]; recs != nil {
		for idx, rec := range recs {
			cleanBlobs[idx] = append([]byte(nil), rec.Blob...)
		}
	}
	if recs := existing[model.StageStats]; recs != nil {
		for idx, rec := range recs {
			statsJSON[idx] = rec.GeometryJSON
		}
	}

	growEligible := map[int]bool{}
	for i := range s.Chunks {
		growEligible[i] = s.ChunkFilter == nil || s.ChunkFilter[i]
	}
	s.executeStage(ctx, model.StageGrow, growEligible, existing[model.StageGrow], growBlobs, stageErrs[model.StageGrow], progress,
		func(chunk Chunk) ([]byte, any, string, error) {
			params := s.CurrentParams()
			if bad := s.seedErrorForChunk(chunk, params.Seeds); bad != nil {
				return nil, nil, "", bad
			}
			raw := vision.RegionGrowTile(s.Loaded.Image, params.Seeds, chunk.Rect, params)
			return raw, nil, "deterministic_fifo_region_growth", nil
		})

	cleanEligible := map[int]bool{}
	for i, blob := range growBlobs {
		if blob != nil && stageErrs[model.StageGrow][i] == nil {
			cleanEligible[i] = true
		}
	}
	s.executeStage(ctx, model.StageClean, cleanEligible, existing[model.StageClean], cleanBlobs, stageErrs[model.StageClean], progress,
		func(chunk Chunk) ([]byte, any, string, error) {
			params := s.CurrentParams()
			raw := growBlobs[chunk.Index]
			thresholded := vision.ThresholdTile(s.Loaded.Image, raw, params.Seeds, chunk.Rect, params)
			clean := vision.MorphClean(thresholded, chunk.Width, chunk.Height, params.StructureSize)
			return clean, nil, "threshold_open_close", nil
		})

	statsEligible := map[int]bool{}
	for i, blob := range cleanBlobs {
		if blob != nil && stageErrs[model.StageClean][i] == nil {
			statsEligible[i] = true
		}
	}
	s.executeStage(ctx, model.StageStats, statsEligible, existing[model.StageStats], nil, stageErrs[model.StageStats], progress,
		func(chunk Chunk) ([]byte, any, string, error) {
			params := s.CurrentParams()
			regions := vision.AnalyzeMask(cleanBlobs[chunk.Index], chunk.Width, chunk.Height, params.MinRegionArea)
			b, err := json.Marshal(regions)
			if err != nil {
				return nil, nil, "", err
			}
			statsJSON[chunk.Index] = string(b)
			return b, regions, "connected_components_geometry", nil
		})

	for _, errs := range stageErrs {
		for _, err := range errs {
			if err != nil {
				if pe, ok := model.AsError(err); ok {
					result.FirstError = pe
				} else if result.FirstError == nil {
					result.FirstError = model.NewError("E_INTERNAL", "pipeline", "%v", err)
				}
				break
			}
		}
		if result.FirstError != nil {
			break
		}
	}

	mask := make([]byte, s.Loaded.Scene.Width*s.Loaded.Scene.Height)
	for _, chunk := range s.Chunks {
		blob := cleanBlobs[chunk.Index]
		if blob == nil {
			continue
		}
		for y := 0; y < chunk.Height; y++ {
			src := y * chunk.Width
			dst := (chunk.PixelY+y)*s.Loaded.Scene.Width + chunk.PixelX
			copy(mask[dst:dst+chunk.Width], blob[src:src+chunk.Width])
		}
	}
	result.Mask = mask
	result.MaskHash = store.HashBytes(mask)
	result.Regions = vision.AnalyzeMask(mask, s.Loaded.Scene.Width, s.Loaded.Scene.Height, s.CurrentParams().MinRegionArea)
	if _, geometryHash, err := store.HashJSON(result.Regions); err == nil {
		result.GeometryHash = geometryHash
	}
	result.States = s.Machine.Snapshot()
	result.Metrics = s.Metrics.Snapshot()
	result.ElapsedMS = time.Since(start).Milliseconds()

	if s.Store != nil {
		records, err := s.Store.ListStages(ctx, info.ID)
		if err == nil {
			for _, rec := range records {
				result.Sequence = append(result.Sequence, SequenceItem{
					ChunkX: rec.ChunkX, ChunkY: rec.ChunkY, Stage: rec.Stage,
					CanonicalOrder: rec.CanonicalOrder, WriteOrder: rec.WriteOrder,
					ContentHash: rec.ContentHash, ParamVersion: rec.ParamVersion,
					Downsample: rec.Downsample,
				})
			}
		}
	}

	if result.FirstError == nil {
		quality := vision.EvaluateRegions(result.Regions, s.Loaded.Scene.TruthRegions, s.Loaded.Scene.Declared.Quality)
		result.Quality = quality
		if len(quality.QualityErrors) > 0 {
			e := quality.QualityErrors[0]
			result.FirstError = &e
		}
	}
	var runErr error
	if result.FirstError != nil {
		runErr = result.FirstError
	}
	return result, runErr
}

func (s *Service) validateSeeds() *model.Error {
	params := s.CurrentParams()
	for _, seed := range params.Seeds {
		if seed.X < 0 || seed.Y < 0 || seed.X >= s.Loaded.Scene.Width || seed.Y >= s.Loaded.Scene.Height {
			return model.NewError(model.CodeSeed, "seeds", "seed (%d,%d) is outside image", seed.X, seed.Y)
		}
		if s.Loaded.TruthMask[seed.Y*s.Loaded.Scene.Width+seed.X] == 0 {
			return model.NewError(model.CodeSeed, "seeds", "seed (%d,%d) is on background or outside truth region", seed.X, seed.Y)
		}
	}
	return nil
}

func (s *Service) seedErrorForChunk(chunk Chunk, seeds []model.IntPoint) *model.Error {
	for _, seed := range seeds {
		if seed.X < chunk.Rect.MinX || seed.X > chunk.Rect.MaxX || seed.Y < chunk.Rect.MinY || seed.Y > chunk.Rect.MaxY {
			continue
		}
		if seed.X < 0 || seed.Y < 0 || seed.X >= s.Loaded.Scene.Width || seed.Y >= s.Loaded.Scene.Height ||
			s.Loaded.TruthMask[seed.Y*s.Loaded.Scene.Width+seed.X] == 0 {
			return model.WithDetails(model.NewError(model.CodeSeed, "seeds",
				"seed (%d,%d) is on background or outside truth region", seed.X, seed.Y),
				map[string]any{"chunk_x": chunk.X, "chunk_y": chunk.Y, "seed": seed})
		}
	}
	return nil
}

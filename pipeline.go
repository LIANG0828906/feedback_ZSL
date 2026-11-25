package iconcut

import (
	"context"
	"errors"
	"fmt"
)

type FaultController struct {
	Remaining map[string]int       `json:"remaining"`
	Codes     map[string]ErrorCode `json:"codes,omitempty"`
}

func NewFaultOnce(tileIndex int, stage string, code ErrorCode) *FaultController {
	key := fmt.Sprintf("%d:%s", tileIndex, stage)
	return &FaultController{Remaining: map[string]int{key: 1}, Codes: map[string]ErrorCode{key: code}}
}

func (f *FaultController) Check(tileIndex int, stage string) error {
	if f == nil {
		return nil
	}
	key := fmt.Sprintf("%d:%s", tileIndex, stage)
	if f.Remaining[key] <= 0 {
		return nil
	}
	f.Remaining[key]--
	code := f.Codes[key]
	if code == "" {
		code = EInternal
	}
	return WithDetails(NewError(code, "fault."+key, "injected failure at tile %d stage %s", tileIndex, stage), map[string]any{
		"tile_index": tileIndex, "stage": stage, "injected": true,
	})
}

type RunOptions struct {
	DBPath           string
	Controller       *Controller
	Faults           *FaultController
	StopAfterCommits int
	Params           *SegmentParams
	SkipTruthCheck   bool
}

type Report struct {
	ImageID         string        `json:"image_id"`
	DBPath          string        `json:"db_path,omitempty"`
	Versions        Versions      `json:"versions"`
	Tiles           []Tile        `json:"tiles"`
	Corrected       *Image        `json:"-"`
	RawMask         *LabelMask    `json:"-"`
	Mask            *LabelMask    `json:"-"`
	Stats           []RegionStats `json:"stats"`
	CommittedKeys   []string      `json:"committed_keys"`
	Waits           []QueueWait   `json:"waits"`
	Interrupted     bool          `json:"interrupted"`
	SnapshotReset   bool          `json:"snapshot_reset"`
	Failures        []*Error      `json:"failures,omitempty"`
	ValidationError *Error        `json:"validation_error,omitempty"`
}

var errStopAfter = errors.New("stop after committed prefix")

func BuildVersions(m *Manifest, p SegmentParams) Versions {
	return Versions{
		Algorithm: AlgorithmVersion,
		Manifest:  HashJSON(m),
		Mapping:   HashJSON(m.Geometry),
		Scheme: HashJSON(struct {
			TileWidth  int `json:"tile_width"`
			TileHeight int `json:"tile_height"`
			BandPX     int `json:"band_px"`
			Order      int `json:"interp_order"`
		}{m.Declared.TileWidth, m.Declared.TileHeight, m.Declared.BandPX, m.Declared.InterpOrder}),
		Param: HashJSON(p),
	}
}

func RunBundle(ctx context.Context, b *Bundle, opts RunOptions) (*Report, error) {
	if b == nil || b.Manifest == nil {
		return nil, NewError(EEmpty, "bundle", "bundle is nil")
	}
	m := b.Manifest
	params := m.Declared.Segment
	if opts.Params != nil {
		params = *opts.Params
	}
	if b.Source == nil || !b.Source.Valid() {
		return nil, NewError(EEmpty, "source", "source image is empty")
	}
	if m.Corrected.W <= 0 || m.Corrected.H <= 0 {
		return nil, NewError(EEmpty, "corrected", "corrected canvas is empty")
	}
	if err := ValidateSegmentParams(params); err != nil {
		return nil, err
	}
	mapper, err := NewMapper(m.Geometry)
	if err != nil {
		return nil, err
	}
	tiles, err := BuildTiles(m.Corrected.W, m.Corrected.H, m.Declared.TileWidth, m.Declared.TileHeight, m.Declared.BandPX, m.Declared.InterpOrder, mapper)
	if err != nil {
		return nil, err
	}
	versions := BuildVersions(m, params)
	snap := &Snapshot{ImageID: m.ImageID, Manifest: m, Tiles: tiles, Versions: versions, Param: &params}
	if b.Reference != nil {
		snap.TruthFingerprint = b.TruthFingerprint()
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = ":memory:"
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	report := &Report{ImageID: m.ImageID, DBPath: opts.DBPath, Versions: versions, Tiles: tiles}
	old, err := store.LoadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	reset := old == nil || old.Versions != versions || old.ImageID != m.ImageID
	if !reset {
		if err := ValidateCheckpoint(ctx, store, old); err != nil {
			reset = true
		}
	}
	if reset {
		if err := store.ResetImage(ctx, m.ImageID); err != nil {
			return nil, err
		}
		if err := store.SaveSnapshot(ctx, snap); err != nil {
			return nil, err
		}
		if err := store.SaveAsset(ctx, m.ImageID, "source.rgb", b.Source.Pix); err != nil {
			return nil, err
		}
	} else {
		if _, err := store.RecoverInterrupted(ctx, m.ImageID); err != nil {
			return nil, err
		}
	}
	report.SnapshotReset = reset

	committed, err := store.LoadCommitSet(ctx, m.ImageID)
	if err != nil {
		return nil, err
	}
	sched := NewScheduler(m.Declared.Budget)
	commitCount := 0

	commitArtifact := func(t Tile, stage string, art *StageArtifact) error {
		enc, err := EncodeArtifact(art)
		if err != nil {
			return err
		}
		summary, _ := jsonMarshalIndent(SummaryFor(art))
		rec := &CommitRecord{
			ImageID: m.ImageID, TileIndex: t.Index, TX: t.TX, TY: t.TY, Stage: stage,
			State: StateCommitted, Digest: SHA256Bytes(summary),
			MappingVersion: versions.Mapping, SchemeVersion: versions.Scheme, ParamVersion: versions.Param,
			ContentHash: SHA256Bytes(enc), AlgorithmVersion: versions.Algorithm,
			SummaryJSON: string(summary), Artifact: enc,
		}
		if err := store.CommitStage(ctx, rec); err != nil {
			return err
		}
		committed[commitKey(t.Index, stage)] = *rec
		report.CommittedKeys = append(report.CommittedKeys, commitKey(t.Index, stage))
		commitCount++
		if opts.StopAfterCommits > 0 && commitCount >= opts.StopAfterCommits {
			return errStopAfter
		}
		return nil
	}

	stageJobs := func(stage string, pixels func(Tile) int, fn func(Tile) (*StageArtifact, error)) []Job[*StageArtifact] {
		jobs := make([]Job[*StageArtifact], 0, len(tiles))
		for _, t := range tiles {
			if _, ok := committed[commitKey(t.Index, stage)]; ok {
				continue
			}
			tile := t
			jobs = append(jobs, Job[*StageArtifact]{
				Tile: tile, Stage: stage, Pixels: pixels(tile),
				Fn: func(ctx context.Context) (*StageArtifact, error) {
					if err := opts.Faults.Check(tile.Index, stage); err != nil {
						return nil, err
					}
					k := TileKey{TileIndex: tile.Index, TX: tile.TX, TY: tile.TY, Stage: stage}
					if err := store.Retry(ctx, m.ImageID, k); err != nil {
						return nil, err
					}
					if err := store.MarkRunning(ctx, m.ImageID, tile, stage); err != nil {
						return nil, err
					}
					return fn(tile)
				},
			})
		}
		return jobs
	}

	runWave := func(stage string, jobs []Job[*StageArtifact]) (map[int]*StageArtifact, error) {
		results := RunJobs(ctx, sched, jobs, opts.Controller)
		resultMap := map[string]JobResult[*StageArtifact]{}
		for _, r := range results {
			resultMap[commitKey(r.Tile.Index, r.Stage)] = r
		}
		arts := map[int]*StageArtifact{}
		var firstErr error
		for _, t := range tiles {
			k := commitKey(t.Index, stage)
			if rec, ok := committed[k]; ok {
				art, err := DecodeArtifact(rec.Artifact)
				if err != nil {
					return nil, err
				}
				arts[t.Index] = art
				continue
			}
			r, ok := resultMap[k]
			if !ok {
				err := NewError(EInternal, "scheduler", "missing result for %s", k)
				_ = store.MarkFailed(ctx, m.ImageID, t, stage, err)
				report.Failures = append(report.Failures, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if r.Err != nil {
				_ = store.MarkFailed(ctx, m.ImageID, t, stage, r.Err)
				e, _ := AsError(r.Err)
				if e == nil {
					e = NewError(EInternal, "", "%v", r.Err)
				}
				report.Failures = append(report.Failures, e)
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			r.Value.ImageID = m.ImageID
			r.Value.TileIndex = t.Index
			r.Value.TX = t.TX
			r.Value.TY = t.TY
			r.Value.Stage = stage
			if r.Value.Core.W == 0 {
				r.Value.Core = t.Core
			}
			if err := commitArtifact(t, stage, r.Value); err != nil {
				if errors.Is(err, errStopAfter) {
					return arts, errStopAfter
				}
				return nil, err
			}
			arts[t.Index] = r.Value
		}
		return arts, firstErr
	}

	mappingJobs := stageJobs(StageMapping, func(t Tile) int { return TilePixelsWithinBuffer(t) }, func(t Tile) (*StageArtifact, error) {
		img, entries, err := CorrectTile(b.Source, mapper, t, m.Declared.InterpOrder, m.Declared.BandPX)
		if err != nil {
			return nil, err
		}
		return &StageArtifact{Core: t.Core, Band: t.Band, SourceRect: t.SourceRect, RGB: img.Pix, MapEntries: entries}, nil
	})
	mapArts, err := runWave(StageMapping, mappingJobs)
	if errors.Is(err, errStopAfter) {
		report.Interrupted = true
		report.Waits = sched.Waits()
		return report, nil
	}
	if err != nil {
		report.Waits = sched.Waits()
		return report, err
	}

	corrected := NewImage(m.Corrected.W, m.Corrected.H, 3)
	for _, t := range tiles {
		art := mapArts[t.Index]
		if art == nil || len(art.RGB) == 0 {
			e := NewError(EState, fmt.Sprintf("tiles[%d].mapping", t.Index), "mapping artifact is unavailable")
			return report, e
		}
		copyCoreRGB(corrected, art.RGB, t.Core)
	}
	report.Corrected = corrected

	growJobs := stageJobs(StageGrow, func(t Tile) int { return m.Corrected.W * m.Corrected.H }, func(t Tile) (*StageArtifact, error) {
		raw, trace, err := RegionGrow(corrected, params, 0)
		if err != nil {
			return nil, err
		}
		return &StageArtifact{Core: t.Core, Labels: labelsToCore(raw, t.Core), LabelW: t.Core.W, LabelH: t.Core.H, Trace: &trace}, nil
	})
	growArts, err := runWave(StageGrow, growJobs)
	if errors.Is(err, errStopAfter) {
		report.Interrupted = true
		report.Waits = sched.Waits()
		return report, nil
	}
	if err != nil {
		report.Waits = sched.Waits()
		return report, err
	}
	rawMask := NewLabelMask(m.Corrected.W, m.Corrected.H)
	for _, t := range tiles {
		if growArts[t.Index] == nil {
			return report, NewError(EState, fmt.Sprintf("tiles[%d].grow", t.Index), "grow artifact unavailable")
		}
		copyCoreLabels(rawMask, growArts[t.Index].Labels, t.Core)
	}
	report.RawMask = rawMask

	cleanJobs := stageJobs(StageClean, func(t Tile) int { return m.Corrected.W * m.Corrected.H * 2 }, func(t Tile) (*StageArtifact, error) {
		clean := CleanLabels(rawMask, params.MorphSize)
		return &StageArtifact{Core: t.Core, Labels: labelsToCore(clean, t.Core), LabelW: t.Core.W, LabelH: t.Core.H}, nil
	})
	cleanArts, err := runWave(StageClean, cleanJobs)
	if errors.Is(err, errStopAfter) {
		report.Interrupted = true
		report.Waits = sched.Waits()
		return report, nil
	}
	if err != nil {
		report.Waits = sched.Waits()
		return report, err
	}
	mask := NewLabelMask(m.Corrected.W, m.Corrected.H)
	for _, t := range tiles {
		if cleanArts[t.Index] == nil {
			return report, NewError(EState, fmt.Sprintf("tiles[%d].clean", t.Index), "clean artifact unavailable")
		}
		copyCoreLabels(mask, cleanArts[t.Index].Labels, t.Core)
	}
	report.Mask = mask

	statsJobs := stageJobs(StageStats, func(t Tile) int { return t.Core.W * t.Core.H }, func(t Tile) (*StageArtifact, error) {
		art := cleanArts[t.Index]
		coreMask := &LabelMask{W: t.Core.W, H: t.Core.H, Labels: append([]byte(nil), art.Labels...)}
		local := AnalyzeLabels(coreMask)
		return &StageArtifact{Core: t.Core, Labels: append([]byte(nil), art.Labels...), LabelW: t.Core.W, LabelH: t.Core.H, TileStats: local}, nil
	})
	statsArts, err := runWave(StageStats, statsJobs)
	if errors.Is(err, errStopAfter) {
		report.Interrupted = true
		report.Waits = sched.Waits()
		return report, nil
	}
	if err != nil {
		report.Waits = sched.Waits()
		return report, err
	}
	for _, t := range tiles {
		if statsArts[t.Index] == nil {
			return report, NewError(EState, fmt.Sprintf("tiles[%d].stats", t.Index), "stats artifact unavailable")
		}
	}
	report.Stats = AnalyzeLabels(mask)
	report.Waits = sched.Waits()

	if !opts.SkipTruthCheck && len(m.Regions) > 0 {
		if verr := CompareToTruth(report.Stats, m.Regions, m.Declared.Tolerance); verr != nil {
			report.ValidationError = verr
			return report, verr
		}
	}
	return report, nil
}

func LoadCommitOrder(ctx context.Context, dbPath, imageID string) ([]CommitRecord, error) {
	st, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.LoadCommits(ctx, imageID)
}

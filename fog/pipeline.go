package fog

import (
	"context"
	"errors"
	"sort"
	"time"
)

var ErrSimulatedCrash = errors.New("E_CRASH: simulated process interruption after durable commit")

type Pipeline struct {
	Scenario          *Scenario
	Store             *CheckpointStore
	CameraID          string
	Faults            *FaultController
	ForceBlocks       map[int]bool
	CrashAfterCommits int
	committedThisRun  int
}

type PipelineResult struct {
	RevisionID     string                `json:"revision_id"`
	Frame          *Frame                `json:"frame"`
	Manifest       *InvalidationManifest `json:"manifest,omitempty"`
	Recovered      []int                 `json:"recovered_blocks"`
	CacheHits      []int                 `json:"cache_hits"`
	Computed       []int                 `json:"computed_blocks"`
	Committed      []int                 `json:"committed_blocks"`
	CommitSequence []int                 `json:"commit_sequence"`
	CrashSimulated bool                  `json:"crash_simulated"`
}

func (p *Pipeline) RunFullFrame(ctx context.Context, mode RenderMode) (*Frame, error) {
	return RenderFrame(ctx, p.Scenario, p.CameraID, mode, p.Faults)
}

// RunCheckpointed performs precise incremental reuse plus atomic block
// checkpoints. Recovered target-revision blocks are not recomputed.
func (p *Pipeline) RunCheckpointed(ctx context.Context, manifest *InvalidationManifest) (*PipelineResult, error) {
	start := time.Now()
	sc := p.Scenario
	cam := sc.ActiveCamera()
	if p.CameraID != "" {
		if c, ok := sc.Camera(p.CameraID); ok {
			cam = c
		}
	}
	vp := NewViewport(sc.Width, sc.Height, sc.Recipe.TileSize)
	revID := TargetRevisionID(sc, cam.ID)
	result := &PipelineResult{RevisionID: revID, Manifest: manifest}

	rev := RevisionRecord{
		ID: revID, Status: "rendering", SnapshotVersion: SnapshotVersion(sc),
		CameraID: cam.ID, CameraVersion: cam.Version,
		RecipeVersion: sc.Recipe.Version, AlgorithmVersion: AlgorithmVersion,
	}
	if err := EnsureRevision(p.Store, rev); err != nil {
		return result, err
	}

	recovered := map[int]*BlockResult{}
	if _, err := GetRevision(p.Store, revID); err == nil {
		if _, verr := p.Store.ValidateRecovery(revID, sc); verr != nil {
			return result, verr
		}
		blocks, lerr := p.Store.LoadCommittedBlocks(revID)
		if lerr != nil {
			return result, lerr
		}
		recovered = blocks
	}

	merged := make([]*BlockResult, len(vp.Tiles))
	pending := []int{}
	for _, tile := range vp.Tiles {
		forced := p.ForceBlocks != nil && p.ForceBlocks[tile.ID]
		if br, ok := recovered[tile.ID]; ok && !forced {
			br.CacheHit = true
			if br.Status == "" {
				br.Status = "success"
			}
			merged[tile.ID] = br
			result.Recovered = append(result.Recovered, tile.ID)
			continue
		}
		if !forced {
			identity, traces, err := BlockCacheIdentity(sc, cam, tile)
			if err != nil {
				return result, err
			}
			if cached, ok, err := p.Store.GetCache(identity.BlockKey); err != nil {
				return result, err
			} else if ok && cached != nil {
				cached.Cache = identity
				cached.Traces = traces
				cached.CacheHit = true
				if cached.Status == "" {
					cached.Status = "success"
				}
				merged[tile.ID] = cached
				result.CacheHits = append(result.CacheHits, tile.ID)
				continue
			}
		}
		pending = append(pending, tile.ID)
	}
	sort.Ints(result.Recovered)
	sort.Ints(result.CacheHits)
	sort.Ints(pending)

	computedFrame, renderErr := RenderBlocks(ctx, sc, cam.ID, ModeFull, pending, p.Faults)
	if computedFrame == nil {
		computedFrame = emptyFrame(sc, ModeFull)
	}
	for _, id := range pending {
		br := computedFrame.Blocks[id]
		if br == nil {
			continue
		}
		if br.Status != "failed" {
			result.Computed = append(result.Computed, id)
		}
		merged[id] = br
	}

	var firstErr error
	if renderErr != nil {
		firstErr = renderErr
	}
	for _, tile := range vp.Tiles {
		br := merged[tile.ID]
		if br == nil || br.Status == "failed" || br.Error != nil {
			if br != nil && br.Error != nil {
				if firstErr == nil {
					firstErr = br.Error
				}
			}
			continue
		}
		_, isRecovered := recovered[tile.ID]
		forcedRecompute := p.ForceBlocks != nil && p.ForceBlocks[tile.ID]
		if isRecovered && !forcedRecompute {
			continue
		}
		if err := p.Store.CommitBlock(revID, br, tile.ID); err != nil {
			return result, err
		}
		p.committedThisRun++
		result.Committed = append(result.Committed, tile.ID)
		if p.CrashAfterCommits > 0 && p.committedThisRun >= p.CrashAfterCommits {
			seq, _ := p.Store.CommitSequence(revID)
			result.CommitSequence = seq
			result.CrashSimulated = true
			return result, ErrSimulatedCrash
		}
	}

	if firstErr == nil {
		_ = SetRevisionStatus(p.Store, revID, "complete")
	} else {
		_ = SetRevisionStatus(p.Store, revID, "block_failure")
	}
	seq, _ := p.Store.CommitSequence(revID)
	result.CommitSequence = seq
	result.Frame = mergeFrame(sc, computedFrame, merged, time.Since(start))
	return result, firstErr
}

func BlockCacheIdentity(sc *Scenario, cam Camera, tile Tile) (CacheIdentity, []TraceResult, error) {
	points := make([]Point, 0, tile.Width*tile.Height)
	for y := tile.Y0; y < tile.Y0+tile.Height; y++ {
		for x := tile.X0; x < tile.X0+tile.Width; x++ {
			points = append(points, Point{X: x, Y: y})
		}
	}
	traces := []TraceResult{}
	for _, chunk := range chunkPoints(points, sc.Recipe.RayBufferPerWorker) {
		for _, pt := range chunk {
			ray := cam.RayForPixel(pt.X, pt.Y, sc.Width, sc.Height, tile.ID)
			traces = append(traces, TraceRay(ray, sc.Media, sc.FixedScale))
		}
	}
	return BuildCacheIdentity(sc, tile, traces), traces, nil
}

func emptyFrame(sc *Scenario, mode RenderMode) *Frame {
	cam := sc.ActiveCamera()
	return &Frame{
		Width: sc.Width, Height: sc.Height, Mode: mode,
		T: make([]float64, sc.Width*sc.Height), Radiance: make([]float64, sc.Width*sc.Height),
		Blocks: []*BlockResult{}, Versions: FrameVersions{
			SnapshotVersion: SnapshotVersion(sc), MediumHashes: map[string]string{},
			CameraID: cam.ID, CameraVersion: cam.Version,
			RecipeVersion: sc.Recipe.Version, AlgorithmVersion: AlgorithmVersion,
		},
	}
}

func mergeFrame(sc *Scenario, base *Frame, blocks []*BlockResult, elapsed time.Duration) *Frame {
	if base == nil {
		base = emptyFrame(sc, ModeFull)
	}
	base.Blocks = blocks
	base.BlockOrder = base.BlockOrder[:0]
	base.Pixels = base.Pixels[:0]
	base.FailedBlocks = base.FailedBlocks[:0]
	for _, br := range blocks {
		if br == nil {
			continue
		}
		base.BlockOrder = append(base.BlockOrder, br.BlockID)
		if br.Status == "failed" || br.Error != nil {
			base.FailedBlocks = append(base.FailedBlocks, br.BlockID)
			continue
		}
		for _, pv := range br.Pixels {
			idx := pv.Y*sc.Width + pv.X
			base.T[idx] = pv.Transmittance
			base.Radiance[idx] = pv.Radiance
			base.Pixels = append(base.Pixels, pv)
		}
	}
	sort.Ints(base.BlockOrder)
	sort.Ints(base.FailedBlocks)
	sort.Slice(base.Pixels, func(i, j int) bool {
		if base.Pixels[i].Y != base.Pixels[j].Y {
			return base.Pixels[i].Y < base.Pixels[j].Y
		}
		return base.Pixels[i].X < base.Pixels[j].X
	})
	if elapsed > 0 {
		base.Quality.ElapsedMS = float64(elapsed.Microseconds()) / 1000
	}
	return base
}

// ComparePreviewAndFull enforces same snapshot/camera/recipe versions and the
// declared preview tolerance.
func ComparePreviewAndFull(preview, full *Frame, recipe Recipe) error {
	if preview.Versions.SnapshotVersion != full.Versions.SnapshotVersion ||
		preview.Versions.CameraVersion != full.Versions.CameraVersion ||
		preview.Versions.RecipeVersion != full.Versions.RecipeVersion {
		return &Error{Code: EDiverge, Field: "versions", Message: "preview and full paths use different parameter versions", Detail: map[string]any{
			"preview": preview.Versions, "full": full.Versions,
		}}
	}
	return CompareFrames(preview, full, recipe.PreviewTransTol, recipe.PreviewRadianceTol)
}

func FullCommitSequence(store *CheckpointStore, sc *Scenario, cameraID string) ([]int, error) {
	rev := TargetRevisionID(sc, cameraID)
	return store.CommitSequence(rev)
}

package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"fixedsampler/internal/geom"
	"fixedsampler/internal/hashutil"
	"fixedsampler/internal/model"
)

type PreviousNodeMeta struct {
	CacheKey     string
	ContentHash  string
	InputHash    string
	UpstreamKeys []string
	Artifact     *model.LevelArtifact
}

type PreviousChunk struct {
	Artifact      *model.ChunkArtifact
	ConfigFP      string
	CacheKeys     []string
	ContentHashes []string
	InputHashes   []string
	UpstreamKeys  [][]string
}

type ChunkCommit struct {
	Chunk         *model.ChunkArtifact
	ConfigFP      string
	CacheKeys     []string
	ContentHashes []string
	InputHashes   []string
	UpstreamKeys  [][]string
}

type Options struct {
	ChunkSize        int
	Workers          int
	BufferLimit      int
	Previous         map[int]*PreviousChunk
	Commit           func(context.Context, *ChunkCommit) error
	OnChunkCommitted func(chunkIndex, committedCount int)
}

type RunResult struct {
	Grid            geom.ChunkGrid
	Artifacts       map[int]*model.ChunkArtifact
	FinalPlanes     map[string]*model.Plane
	LevelPlanes     map[int]map[string]*model.Plane
	Records         map[int]map[int]*NodeRecord
	Invalidations   []model.InvalidationRecord
	QueueEvents     []model.QueueEvent
	CommitSequence  []int
	ConfigFP        string
	DependencyEdges []DependencyEdge
}

type planeState struct {
	plane *model.Plane
	cumX  int
	cumY  int
}

func Run(ctx context.Context, p *model.Project, opts Options) (*RunResult, error) {
	if opts.ChunkSize < 1 {
		opts.ChunkSize = 8
	}
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.BufferLimit < 1 {
		opts.BufferLimit = 1
	}
	if err := loaderValidate(p); err != nil {
		return nil, err
	}

	fp := configFingerprint(p)
	grid := geom.NewChunkGrid(p.FullWidth, p.FullHeight, opts.ChunkSize)
	pool := NewWorkerPool(opts.Workers, opts.BufferLimit)
	defer pool.Close()

	result := &RunResult{
		Grid: grid, Artifacts: map[int]*model.ChunkArtifact{},
		LevelPlanes: map[int]map[string]*model.Plane{},
		Records:     map[int]map[int]*NodeRecord{},
		ConfigFP:    fp,
	}

	current := map[string]*planeState{}
	for _, plane := range p.Planes {
		current[plane.Name] = &planeState{plane: plane, cumX: 1, cumY: 1}
	}

	sourceRecords := map[int]*NodeRecord{}
	for idx := 0; idx < grid.Count(); idx++ {
		cx, cy := grid.Coords(idx)
		fullRect := grid.FullRect(cx, cy)
		chunk := &model.ChunkArtifact{
			ProjectID: p.ProjectID, ChunkIndex: idx, ChunkX: cx, ChunkY: cy,
			ChunkSize: opts.ChunkSize, FullRect: fullRect,
		}
		level0 := model.LevelArtifact{Seq: 0, Type: "source", Config: model.StageConfig{Seq: 0, Type: "source"}}
		var regions []RegionHash
		for _, plane := range p.Planes {
			rect := geom.MapFullRectToPlane(fullRect, plane.SubsampleX, plane.SubsampleY, 1, 1)
			values := plane.RectValues(rect)
			level0.Tiles = append(level0.Tiles, model.Tile{Plane: plane.Name, Rect: rect, Values: values})
			regions = append(regions, RegionHash{Plane: plane.Name, Rect: rect, Hash: HashRegion(plane.Name, rect, values)})
		}
		key := sourceCacheKey(p, cx, cy, opts.ChunkSize, regions)
		rec := &NodeRecord{
			Seq: 0, ChunkIndex: idx, ChunkX: cx, ChunkY: cy, CacheKey: key,
			ContentHash: HashTiles(level0.Tiles), InputHash: hashutil.CanonicalHash(regions),
			Artifact: &level0,
		}
		sourceRecords[idx] = rec
		chunk.Levels = append(chunk.Levels, level0)
		result.Artifacts[idx] = chunk
	}
	result.Records[0] = sourceRecords
	result.LevelPlanes[0] = planeMap(current)

	for seq := 1; seq <= len(p.Config); seq++ {
		stage := p.Config[seq-1]
		stageRecords := map[int]*NodeRecord{}
		futures := map[int]<-chan nodeResult{}
		next := cloneStates(current)

		outputWidths := map[string]struct{ w, h int }{}
		for idx := 0; idx < grid.Count(); idx++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cx, cy := grid.Coords(idx)
			fullRect := grid.FullRect(cx, cy)
			rects := map[string]model.Rect{}
			var inputRegions []RegionHash
			upstreamSet := map[string]struct{}{}

			addUpstream := func(planeName string, support, outputRegion model.Rect) error {
				prev := result.Records[seq-1]
				for j := 0; j < grid.Count(); j++ {
					oldRec := prev[j]
					if oldRec == nil || oldRec.Artifact == nil {
						continue
					}
					for _, tile := range oldRec.Artifact.Tiles {
						if tile.Plane != planeName {
							continue
						}
						if !tile.Rect.Intersect(support).Empty() {
							upstreamSet[oldRec.CacheKey] = struct{}{}
							result.DependencyEdges = append(result.DependencyEdges, DependencyEdge{
								FromStage: seq - 1, FromChunk: j, ToStage: seq, ToChunk: idx,
								Plane: planeName, InputRegion: support, OutputRegion: outputRegion,
								Reason:   "persisted_tile_region_intersects_kernel_support",
								CacheKey: oldRec.CacheKey,
							})
						}
					}
				}
				return nil
			}

			switch stage.Type {
			case "resample":
				fx, fy := stage.FactorX, stage.FactorY
				for _, name := range stage.Planes {
					ps := current[name]
					if ps == nil {
						return nil, model.SchemaError("stage %d unknown plane %q", seq, name)
					}
					outRect := geom.MapFullRectToPlane(fullRect, ps.plane.SubsampleX, ps.plane.SubsampleY, ps.cumX*fx, ps.cumY*fy)
					rects[name] = outRect
					support := geom.BoxSupport(outRect, fx, fy)
					if stage.Kernel == "nearest" {
						support = geom.NearestSupport(outRect, fx, fy)
					}
					bounds := model.Rect{X1: ps.plane.Width, Y1: ps.plane.Height}
					support = support.Intersect(bounds)
					values := ps.plane.RectValues(support)
					inputRegions = append(inputRegions, RegionHash{Plane: name, Rect: support, Hash: HashRegion(name, support, values)})
					if err := addUpstream(name, support, outRect); err != nil {
						return nil, err
					}
					outputWidths[name] = struct{ w, h int }{ps.plane.Width / fx, ps.plane.Height / fy}
				}
			case "color":
				first := current[stage.InputNames[0]]
				outRect := geom.MapFullRectToPlane(fullRect, first.plane.SubsampleX, first.plane.SubsampleY, first.cumX, first.cumY)
				for _, name := range stage.InputNames {
					ps := current[name]
					rect := geom.MapFullRectToPlane(fullRect, ps.plane.SubsampleX, ps.plane.SubsampleY, ps.cumX, ps.cumY)
					values := ps.plane.RectValues(rect)
					inputRegions = append(inputRegions, RegionHash{Plane: name, Rect: rect, Hash: HashRegion(name, rect, values)})
					if err := addUpstream(name, rect, outRect); err != nil {
						return nil, err
					}
				}
				for _, name := range stage.OutputNames {
					rects[name] = outRect
					outputWidths[name] = struct{ w, h int }{first.plane.Width, first.plane.Height}
				}
			case "quantize":
				for _, name := range stage.Planes {
					ps := current[name]
					rect := geom.MapFullRectToPlane(fullRect, ps.plane.SubsampleX, ps.plane.SubsampleY, ps.cumX, ps.cumY)
					rects[name] = rect
					values := ps.plane.RectValues(rect)
					inputRegions = append(inputRegions, RegionHash{Plane: name, Rect: rect, Hash: HashRegion(name, rect, values)})
					if err := addUpstream(name, rect, rect); err != nil {
						return nil, err
					}
					outputWidths[name] = struct{ w, h int }{ps.plane.Width, ps.plane.Height}
				}
			default:
				return nil, model.SchemaError("stage %d unsupported type %q", seq, stage.Type)
			}

			upstreamKeys := make([]string, 0, len(upstreamSet))
			for key := range upstreamSet {
				upstreamKeys = append(upstreamKeys, key)
			}
			slices.Sort(upstreamKeys)
			inputHash := hashutil.CanonicalHash(inputRegions)
			key := ComputeCacheKey(p, stage, cx, cy, opts.ChunkSize, inputRegions, upstreamKeys)
			rec := &NodeRecord{
				Seq: seq, ChunkIndex: idx, ChunkX: cx, ChunkY: cy, CacheKey: key,
				InputHash: inputHash, UpstreamKeys: upstreamKeys,
			}
			stageRecords[idx] = rec

			old := opts.Previous[idx]
			oldKey, oldMeta := previousNode(old, seq)
			if old != nil && oldKey == key && oldMeta != nil && oldMeta.Artifact != nil {
				contentHash := HashTiles(oldMeta.Artifact.Tiles)
				if len(old.ContentHashes) > seq && old.ContentHashes[seq] != "" && contentHash != old.ContentHashes[seq] {
					result.Invalidations = append(result.Invalidations, invalidationRecord(idx, cx, cy, seq, "persisted_content_hash_mismatch", oldKey, key))
				} else {
					rec.Artifact = oldMeta.Artifact
					rec.ContentHash = contentHash
					continue
				}
			}

			reason := invalidReason(old, seq, stage, inputHash, upstreamKeys)
			result.Invalidations = append(result.Invalidations, invalidationRecord(idx, cx, cy, seq, reason, oldKey, key))

			jobRects := maps.Clone(rects)
			jobInputs := current
			done, err := pool.Submit(ctx, idx, seq, cx, cy, func() (*model.LevelArtifact, error) {
				return ComputeNode(ComputeInput{
					Project: p, Stage: stage, ChunkIndex: idx, ChunkX: cx, ChunkY: cy,
					Rects: jobRects, PrePlanes: planeMap(jobInputs),
				})
			})
			if err != nil {
				return nil, err
			}
			futures[idx] = done
		}

		outPlanes := map[string]*model.Plane{}
		for name, wh := range outputWidths {
			base := current[name]
			np := &model.Plane{Name: name, Width: wh.w, Height: wh.h, Values: make([]int64, wh.w*wh.h)}
			if base != nil && base.plane != nil {
				np.Role = base.plane.Role
				np.SubsampleX = base.plane.SubsampleX
				np.SubsampleY = base.plane.SubsampleY
				np.BitDepth = base.plane.BitDepth
				np.ColorSpace = base.plane.ColorSpace
			}
			if stage.Type == "color" {
				first := current[stage.InputNames[0]].plane
				np.Role = "color"
				np.SubsampleX = first.SubsampleX
				np.SubsampleY = first.SubsampleY
				np.BitDepth = first.BitDepth
				np.ColorSpace = stage.TargetSpace
			}
			if stage.Type == "quantize" {
				np.BitDepth = stage.BitDepth
			}
			outPlanes[name] = np
		}

		for idx := 0; idx < grid.Count(); idx++ {
			rec := stageRecords[idx]
			if done, ok := futures[idx]; ok {
				select {
				case res := <-done:
					if res.err != nil {
						return nil, res.err
					}
					rec.Artifact = res.artifact
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if rec.Artifact == nil {
				return nil, fmt.Errorf("stage %d chunk %d has no artifact", seq, idx)
			}
			rec.ContentHash = HashTiles(rec.Artifact.Tiles)
			result.Artifacts[idx].Levels = append(result.Artifacts[idx].Levels, *rec.Artifact)
			for _, tile := range rec.Artifact.Tiles {
				plane := outPlanes[tile.Plane]
				if plane == nil {
					return nil, fmt.Errorf("stage %d produced undeclared tile plane %q", seq, tile.Plane)
				}
				placeTile(plane, tile)
			}
		}

		for name, np := range outPlanes {
			ps := &planeState{plane: np, cumX: 1, cumY: 1}
			if old := current[name]; old != nil {
				ps.cumX = old.cumX
				ps.cumY = old.cumY
			}
			if stage.Type == "resample" && slices.Contains(stage.Planes, name) {
				ps.cumX = current[name].cumX * stage.FactorX
				ps.cumY = current[name].cumY * stage.FactorY
			}
			if stage.Type == "color" && slices.Contains(stage.OutputNames, name) {
				first := current[stage.InputNames[0]]
				ps.cumX = first.cumX
				ps.cumY = first.cumY
			}
			next[name] = ps
		}
		current = next
		result.Records[seq] = stageRecords
		result.LevelPlanes[seq] = planeMap(current)
	}

	result.FinalPlanes = planeMap(current)
	result.QueueEvents = pool.Events()

	for idx := 0; idx < grid.Count(); idx++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		commit, err := buildCommit(p, result, idx)
		if err != nil {
			return result, err
		}
		if opts.Commit != nil {
			if err := opts.Commit(ctx, commit); err != nil {
				return result, err
			}
		}
		result.CommitSequence = append(result.CommitSequence, idx)
		if opts.OnChunkCommitted != nil {
			opts.OnChunkCommitted(idx, len(result.CommitSequence))
		}
	}
	return result, nil
}

func buildCommit(p *model.Project, result *RunResult, idx int) (*ChunkCommit, error) {
	chunk := result.Artifacts[idx]
	n := len(p.Config) + 1
	commit := &ChunkCommit{Chunk: chunk, ConfigFP: result.ConfigFP}
	for seq := 0; seq < n; seq++ {
		rec := result.Records[seq][idx]
		if rec == nil {
			return nil, fmt.Errorf("missing record seq=%d chunk=%d", seq, idx)
		}
		commit.CacheKeys = append(commit.CacheKeys, rec.CacheKey)
		commit.ContentHashes = append(commit.ContentHashes, rec.ContentHash)
		commit.InputHashes = append(commit.InputHashes, rec.InputHash)
		commit.UpstreamKeys = append(commit.UpstreamKeys, append([]string(nil), rec.UpstreamKeys...))
	}
	return commit, nil
}

func previousNode(old *PreviousChunk, seq int) (string, *PreviousNodeMeta) {
	if old == nil || old.Artifact == nil || seq >= len(old.Artifact.Levels) {
		return "", nil
	}
	meta := &PreviousNodeMeta{Artifact: &old.Artifact.Levels[seq]}
	if seq < len(old.CacheKeys) {
		meta.CacheKey = old.CacheKeys[seq]
	}
	if seq < len(old.ContentHashes) {
		meta.ContentHash = old.ContentHashes[seq]
	}
	if seq < len(old.InputHashes) {
		meta.InputHash = old.InputHashes[seq]
	}
	if seq < len(old.UpstreamKeys) {
		meta.UpstreamKeys = old.UpstreamKeys[seq]
	}
	return meta.CacheKey, meta
}

func invalidReason(old *PreviousChunk, seq int, stage model.StageConfig, newInputHash string, newUpstream []string) string {
	if old == nil || old.Artifact == nil {
		return "missing_persisted_chunk"
	}
	_, meta := previousNode(old, seq)
	if meta == nil || meta.Artifact == nil {
		return "missing_level_node"
	}
	if hashutil.CanonicalHash(meta.Artifact.Config) != hashutil.CanonicalHash(stage) {
		return "config_parameter_or_order_changed"
	}
	if meta.InputHash != "" && meta.InputHash != newInputHash {
		return "input_region_content_changed"
	}
	if !sameStringSet(meta.UpstreamKeys, newUpstream) {
		return "upstream_dependency_changed"
	}
	return "cache_key_versions_or_metadata_changed"
}

func invalidationRecord(idx, cx, cy, seq int, reason, oldKey, newKey string) model.InvalidationRecord {
	return model.InvalidationRecord{
		ChunkIndex: idx, ChunkX: cx, ChunkY: cy, StageSeq: seq,
		Reason: reason, OldKey: oldKey, NewKey: newKey,
	}
}

func sameStringSet(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	slices.Sort(aa)
	slices.Sort(bb)
	return slices.Equal(aa, bb)
}

func cloneStates(in map[string]*planeState) map[string]*planeState {
	out := make(map[string]*planeState, len(in))
	maps.Copy(out, in)
	return out
}

func planeMap(states map[string]*planeState) map[string]*model.Plane {
	out := make(map[string]*model.Plane, len(states))
	for k, v := range states {
		out[k] = v.plane
	}
	return out
}

func placeTile(dst *model.Plane, tile model.Tile) {
	i := 0
	for y := tile.Rect.Y0; y < tile.Rect.Y1; y++ {
		for x := tile.Rect.X0; x < tile.Rect.X1; x++ {
			dst.Values[y*dst.Width+x] = tile.Values[i]
			i++
		}
	}
}

func loaderValidate(p *model.Project) error {
	if p == nil {
		return errors.New("nil project")
	}
	if len(p.Config) == 0 {
		return model.SchemaError("config sequence is empty")
	}
	return nil
}

func configFingerprint(p *model.Project) string {
	return hashutil.CanonicalHash(struct {
		ProjectID  string              `json:"project_id"`
		ValueScale int64               `json:"value_scale"`
		Depths     []int               `json:"supported_depths"`
		Config     []model.StageConfig `json:"config"`
	}{p.ProjectID, p.ValueScale, p.SupportedDepths, p.Config})
}

func ConfigFingerprint(p *model.Project) string { return configFingerprint(p) }

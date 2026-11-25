package maskqc

import (
	"fmt"
	"sort"
)

type RunOptions struct {
	Path        string
	Store       *CheckpointStore
	Commit      bool
	Scheduler   *Scheduler
	FailTileID  string
	PanicTileID string
}

type RunResult struct {
	Path             string              `json:"path"`
	ProjectID        string              `json:"projectId"`
	Mask             *Mask               `json:"-"`
	AlphaExact       bool                `json:"alphaExact"`
	PixelExact       bool                `json:"pixelExact"`
	Sequence         []string            `json:"sequence"`
	Decisions        []TileOrderDecision `json:"decisions"`
	Topology         TopologyReport      `json:"topology"`
	Directions       DirectionReport     `json:"directions"`
	Graph            *DependencyGraph    `json:"graph,omitempty"`
	SchedulerResults []JobResult         `json:"schedulerResults,omitempty"`
	SchedulerMetrics *SchedulerMetrics   `json:"schedulerMetrics,omitempty"`
	CommitCount      int                 `json:"commitCount"`
	RecomputedTiles  []string            `json:"recomputedTiles,omitempty"`
}

type Engine struct {
	Project *Project
	tiles   []Tile
	tileMap map[string]Tile
}

func NewEngine(p *Project) *Engine {
	tiles := Tiles(p.Width, p.Height, DefaultTileSize)
	tileMap := make(map[string]Tile, len(tiles))
	for _, t := range tiles {
		tileMap[t.ID] = t
	}
	return &Engine{Project: p, tiles: tiles, tileMap: tileMap}
}

func (e *Engine) OrderedStrokes() []Stroke { return OrderedStrokes(e.Project.Strokes) }

func (e *Engine) BaseUpstream() map[string]string {
	up := make(map[string]string, len(e.tiles))
	for _, tile := range e.tiles {
		inputHash := HashMaskRegion(e.Project.Candidate, tile.Rect)
		up[tile.ID] = "candidate:" + inputHash
	}
	return up
}

func (e *Engine) BuildCommit(orderIndex int, stroke Stroke, upstream map[string]string) (*CommitRecord, []*Component, error) {
	if err := ValidateStroke(stroke); err != nil {
		return nil, nil, err
	}
	rec := &CommitRecord{
		ProjectID: e.Project.ID, StrokeID: stroke.ID, OrderIndex: orderIndex,
		ResidualDigest: e.Project.ConvertReport.ResidualDigest(),
	}
	var comps []*Component
	bounds := stroke.Bounds()
	for _, tile := range e.tiles {
		if _, ok := bounds.Intersect(tile.Rect); !ok {
			continue
		}
		comp, err := RasterizeStrokeTile(stroke, tile)
		if err != nil {
			return nil, nil, err
		}
		inputHash := HashMaskRegion(e.Project.Candidate, tile.Rect)
		key := ComponentCacheKey(CacheKeyInput{
			TileID: tile.ID, TileInputHash: inputHash, StrokeID: stroke.ID,
			UpstreamKey: upstream[tile.ID],
		})
		comp.CacheKey = key
		comps = append(comps, comp)
		rec.Tiles = append(rec.Tiles, TileComponentRecord{
			TileID: tile.ID, CacheKey: key, ContentHash: comp.ContentHash,
			Width: comp.Width, Height: comp.Height, Op: comp.Op, Alpha: comp.Alpha,
		})
		upstream[tile.ID] = key
	}
	return rec, comps, nil
}

// RunSequential is the deterministic incremental/checkpoint-capable path.
func (e *Engine) RunSequential(opts RunOptions) (*RunResult, error) {
	mask := e.Project.Candidate.Clone()
	upstream := e.BaseUpstream()
	ordered := e.OrderedStrokes()
	result := &RunResult{Path: opts.Path, ProjectID: e.Project.ID, Mask: mask}

	for i, stroke := range ordered {
		rec, comps, err := e.BuildCommit(i, stroke, upstream)
		if err != nil {
			return nil, err
		}
		for _, comp := range comps {
			tile := e.tileMap[comp.TileID]
			ApplyComponent(mask, tile, comp)
		}
		if opts.Store != nil && opts.Commit {
			if _, err := opts.Store.CommitStroke(rec); err != nil {
				return nil, err
			}
			result.CommitCount++
		}
		result.Sequence = append(result.Sequence, stroke.ID)
	}
	e.fillVerification(result)
	result.Graph = BuildDependencyGraph(e.Project)
	return result, nil
}

func (e *Engine) RunPrefix(store *CheckpointStore, count int) (*RunResult, error) {
	mask := e.Project.Candidate.Clone()
	upstream := e.BaseUpstream()
	ordered := e.OrderedStrokes()
	if count > len(ordered) {
		count = len(ordered)
	}
	result := &RunResult{Path: "prefix", ProjectID: e.Project.ID, Mask: mask}
	for i := 0; i < count; i++ {
		stroke := ordered[i]
		rec, comps, err := e.BuildCommit(i, stroke, upstream)
		if err != nil {
			return nil, err
		}
		for _, comp := range comps {
			ApplyComponent(mask, e.tileMap[comp.TileID], comp)
		}
		if _, err := store.CommitStroke(rec); err != nil {
			return nil, err
		}
		result.Sequence = append(result.Sequence, stroke.ID)
		result.CommitCount++
	}
	return result, nil
}

// Resume reconstructs from SQLite, verifies cached keys, then commits only the
// suffix that was missing at the breakpoint.
func (e *Engine) Resume(store *CheckpointStore) (*RunResult, error) {
	commits, err := store.LoadCommits(e.Project.ID)
	if err != nil {
		return nil, err
	}
	ordered := e.OrderedStrokes()
	if len(commits) > len(ordered) {
		return nil, &QCError{Code: CodeCheckpoint, Field: "seq", Message: "checkpoint contains more strokes than project"}
	}
	mask := e.Project.Candidate.Clone()
	upstream := e.BaseUpstream()
	committedIDs := map[string]bool{}
	sequence := make([]string, 0, len(ordered))

	for i, rec := range commits {
		if i >= len(ordered) || rec.StrokeID != ordered[i].ID {
			return nil, &QCError{Code: CodeCheckpoint, Field: fmt.Sprintf("stroke_commits.seq=%d", rec.Seq), Message: "checkpoint order does not match adjudicated order"}
		}
		sequence = append(sequence, rec.StrokeID)
		committedIDs[rec.StrokeID] = true
		for _, tr := range rec.Tiles {
			tile, ok := e.tileMap[tr.TileID]
			if !ok {
				return nil, &QCError{Code: CodeCheckpoint, Field: "tileId", Message: "checkpoint tile not in current project"}
			}
			inputHash := HashMaskRegion(e.Project.Candidate, tile.Rect)
			expectedKey := ComponentCacheKey(CacheKeyInput{
				TileID: tile.ID, TileInputHash: inputHash, StrokeID: rec.StrokeID,
				UpstreamKey: upstream[tile.ID],
			})
			if expectedKey != tr.CacheKey {
				return nil, &QCError{Code: CodeCheckpoint, Field: "cacheKey", Message: "cache key chain mismatch on resume", Details: map[string]any{"tileId": tile.ID}}
			}
			if HashBytes(tr.Alpha) != tr.ContentHash {
				return nil, &QCError{Code: CodeCheckpoint, Field: "contentHash", Message: "component hash mismatch"}
			}
			comp := &Component{
				StrokeID: rec.StrokeID, TileID: tr.TileID, CacheKey: tr.CacheKey,
				ContentHash: tr.ContentHash, Width: tr.Width, Height: tr.Height, Op: tr.Op, Alpha: tr.Alpha,
			}
			ApplyComponent(mask, tile, comp)
			upstream[tile.ID] = tr.CacheKey
		}
	}

	result := &RunResult{Path: "resume", ProjectID: e.Project.ID, Mask: mask, Sequence: sequence, CommitCount: len(commits)}
	for i, stroke := range ordered {
		if committedIDs[stroke.ID] {
			continue
		}
		rec, comps, err := e.BuildCommit(i, stroke, upstream)
		if err != nil {
			return nil, err
		}
		for _, comp := range comps {
			ApplyComponent(mask, e.tileMap[comp.TileID], comp)
		}
		if _, err := store.CommitStroke(rec); err != nil {
			return nil, err
		}
		result.Sequence = append(result.Sequence, stroke.ID)
		result.CommitCount++
	}
	e.fillVerification(result)
	result.Graph = BuildDependencyGraph(e.Project)
	return result, nil
}

// RecomputeAffected keeps all unaffected tile pixels untouched and rebuilds
// only the listed tiles through their adjudicated stroke chain.
func (e *Engine) RecomputeAffected(previous *Mask, tileIDs []string) (*RunResult, error) {
	if previous == nil || previous.Width != e.Project.Width || previous.Height != e.Project.Height {
		return nil, schemaError("previousMask", "a same-dimension previous mask is required for precise invalid recomputation")
	}
	ids := append([]string(nil), tileIDs...)
	sort.Strings(ids)
	idSet := map[string]bool{}
	mask := previous.Clone()
	for _, tileID := range ids {
		if idSet[tileID] {
			continue
		}
		idSet[tileID] = true
		tile, ok := e.tileMap[tileID]
		if !ok {
			return nil, schemaError("tileId", "tile %q not found", tileID)
		}
		local := NewMask(tile.Rect.Width(), tile.Rect.Height())
		for yy := 0; yy < tile.Rect.Height(); yy++ {
			for xx := 0; xx < tile.Rect.Width(); xx++ {
				local.Alpha[yy*tile.Rect.Width()+xx] = e.Project.Candidate.Get(tile.Rect.MinX+xx, tile.Rect.MinY+yy)
			}
		}
		localTile := Tile{Rect: BBox{MaxX: tile.Rect.Width() - 1, MaxY: tile.Rect.Height() - 1}}
		decision := AdjudicateTile(e.Project.Strokes, tile)
		for _, participant := range decision.Participants {
			stroke := findStrokeByID(e.Project.Strokes, participant.StrokeID)
			comp, err := RasterizeStrokeTile(stroke, tile)
			if err != nil {
				return nil, err
			}
			ApplyComponent(local, localTile, comp)
		}
		for yy := 0; yy < tile.Rect.Height(); yy++ {
			for xx := 0; xx < tile.Rect.Width(); xx++ {
				mask.Set(tile.Rect.MinX+xx, tile.Rect.MinY+yy, local.Alpha[yy*tile.Rect.Width()+xx])
			}
		}
	}
	result := &RunResult{Path: "precise-incremental", ProjectID: e.Project.ID, Mask: mask, RecomputedTiles: ids}
	for _, stroke := range e.OrderedStrokes() {
		result.Sequence = append(result.Sequence, stroke.ID)
	}
	e.fillVerification(result)
	result.Graph = BuildDependencyGraph(e.Project)
	return result, nil
}

// RunFullRecompute uses the independent simple composition routine.
func (e *Engine) RunFullRecompute() (*RunResult, error) {
	mask, decisions, err := ComposeStrokesFull(e.Project.Width, e.Project.Height, e.Project.Candidate, e.Project.Strokes)
	if err != nil {
		return nil, err
	}
	result := &RunResult{Path: "full", ProjectID: e.Project.ID, Mask: mask, Decisions: decisions}
	for _, s := range e.OrderedStrokes() {
		result.Sequence = append(result.Sequence, s.ID)
	}
	e.fillVerification(result)
	result.Graph = BuildDependencyGraph(e.Project)
	return result, nil
}

func (e *Engine) fillVerification(result *RunResult) {
	result.Decisions = make([]TileOrderDecision, 0, len(e.tiles))
	for _, tile := range e.tiles {
		result.Decisions = append(result.Decisions, AdjudicateTile(e.Project.Strokes, tile))
	}
	sort.Slice(result.Decisions, func(i, j int) bool { return result.Decisions[i].TileID < result.Decisions[j].TileID })
	if e.Project.Truth.Reference != nil {
		result.AlphaExact = result.Mask.EqualAlpha(e.Project.Truth.Reference)
		result.PixelExact = result.Mask.EqualBinary(e.Project.Truth.Reference, ForegroundThreshold)
	}
	result.Topology = VerifyTopology(e.Project, result.Mask.Binary(ForegroundThreshold))
	result.Directions = VerifyDirections(e.Project, result.Mask.Binary(ForegroundThreshold))
}

// RunScheduled computes each tile's ordered alpha chain independently in a
// bounded worker pool. A failed/panicking tile is reported and not assembled.
func (e *Engine) RunScheduled(sched *Scheduler, failTileID, panicTileID string) (*RunResult, error) {
	if sched == nil {
		sched = NewScheduler(2, 4096, 8)
	}
	type tileOutput struct {
		mask       []byte
		components []*Component
	}
	outputs := make([]tileOutput, len(e.tiles))
	jobs := make([]Job, 0, len(e.tiles))
	for idx, tile := range e.tiles {
		idx, tile := idx, tile
		jobs = append(jobs, Job{
			Index: idx, ID: "tile-job-" + tile.ID, TileID: tile.ID, Pixels: tile.Pixels,
			Fn: func(buffer []byte) error {
				if tile.ID == panicTileID {
					panic("injected tile panic")
				}
				if tile.ID == failTileID {
					return &QCError{Code: CodeInternal, Field: "tile", Message: "injected tile failure", Details: map[string]any{"tileId": tile.ID}}
				}
				local := NewMask(tile.Rect.Width(), tile.Rect.Height())
				for yy := 0; yy < tile.Rect.Height(); yy++ {
					for xx := 0; xx < tile.Rect.Width(); xx++ {
						local.Alpha[yy*tile.Rect.Width()+xx] = e.Project.Candidate.Get(tile.Rect.MinX+xx, tile.Rect.MinY+yy)
					}
				}
				localTile := Tile{ID: tile.ID, Row: tile.Row, Col: tile.Col, Rect: BBox{MinX: 0, MinY: 0, MaxX: tile.Rect.Width() - 1, MaxY: tile.Rect.Height() - 1}, Pixels: tile.Pixels}
				decision := AdjudicateTile(e.Project.Strokes, tile)
				var comps []*Component
				for _, p := range decision.Participants {
					stroke := findStrokeByID(e.Project.Strokes, p.StrokeID)
					comp, err := RasterizeStrokeTile(stroke, tile)
					if err != nil {
						return err
					}
					// Rebase component into local tile coordinates.
					localComp := comp.CloneForLocal(tile)
					ApplyComponent(local, localTile, localComp)
					comps = append(comps, comp)
				}
				outputs[idx] = tileOutput{mask: append([]byte(nil), local.Alpha...), components: comps}
				return nil
			},
		})
	}
	results := sched.Run(jobs)
	metrics := sched.Metrics
	out := &RunResult{Path: "scheduled", ProjectID: e.Project.ID, SchedulerResults: results, SchedulerMetrics: &metrics}
	for _, r := range results {
		if !r.OK {
			return out, &QCError{Code: CodeInternal, Field: "scheduler", Message: "one or more tiles failed; successful checkpoints remain untouched", Details: map[string]any{"failed": failedTiles(results)}}
		}
	}

	mask := e.Project.Candidate.Clone()
	for idx, tile := range e.tiles {
		output := outputs[idx]
		for yy := 0; yy < tile.Rect.Height(); yy++ {
			for xx := 0; xx < tile.Rect.Width(); xx++ {
				v := output.mask[yy*tile.Rect.Width()+xx]
				mask.Set(tile.Rect.MinX+xx, tile.Rect.MinY+yy, v)
			}
		}
	}
	out.Mask = mask
	for _, s := range e.OrderedStrokes() {
		out.Sequence = append(out.Sequence, s.ID)
	}
	e.fillVerification(out)
	return out, nil
}

func (c *Component) CloneForLocal(globalTile Tile) *Component {
	width := globalTile.Rect.Width()
	height := globalTile.Rect.Height()
	out := &Component{StrokeID: c.StrokeID, TileID: c.TileID, CacheKey: c.CacheKey, ContentHash: c.ContentHash, Width: width, Height: height, Op: c.Op, Alpha: make([]byte, width*height)}
	copy(out.Alpha, c.Alpha)
	return out
}

func findStrokeByID(strokes []Stroke, id string) Stroke {
	idx := findStrokeIndex(strokes, id)
	if idx < 0 {
		return Stroke{}
	}
	return strokes[idx]
}

func failedTiles(results []JobResult) []string {
	var out []string
	for _, r := range results {
		if !r.OK {
			out = append(out, r.TileID)
		}
	}
	return out
}

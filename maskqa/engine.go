package maskqa

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type VerificationReport struct {
	ProjectID        string                `json:"project_id"`
	OK               bool                  `json:"ok"`
	AlgorithmVersion string                `json:"algorithm_version"`
	ParameterVersion string                `json:"parameter_version"`
	OrderVersion     string                `json:"order_version"`
	MaskWidth        int                   `json:"mask_width"`
	MaskHeight       int                   `json:"mask_height"`
	MaskHash         string                `json:"mask_hash"`
	Topology         *TopologyVerification `json:"topology,omitempty"`
	Direction        *DirectionReport      `json:"direction,omitempty"`
}

type TileComputation struct {
	Tile          Tile               `json:"tile"`
	InputHash     string             `json:"input_hash"`
	LocalMask     []byte             `json:"-"`
	Alpha         []uint16           `json:"-"`
	AccumKey      string             `json:"accum_key"`
	OutputHash    string             `json:"output_hash"`
	ComponentKeys map[int]string     `json:"component_keys"`
	Records       []CheckpointRecord `json:"-"`
}

type RunResult struct {
	Mask            *Mask               `json:"mask"`
	Report          *VerificationReport `json:"report"`
	Order           *OrderDecision      `json:"order"`
	Graph           *DepGraph           `json:"graph,omitempty"`
	Tiles           []TileComputation   `json:"-"`
	CommitSequence  []CheckpointRecord  `json:"commit_sequence"`
	Invalidation    *InvalidationPlan   `json:"invalidation,omitempty"`
	SchedulerTrace  *SchedulerTrace     `json:"scheduler_trace,omitempty"`
	Recovery        *RecoveryReport     `json:"recovery,omitempty"`
	RecomputedTiles []int               `json:"recomputed_tiles"`
	ReusedTiles     []int               `json:"reused_tiles"`
	LoadedTiles     []int               `json:"loaded_tiles"`
}

type RunOptions struct {
	Recover      bool
	Invalidation *InvalidationPlan
	Label        string
}

type Engine struct {
	Project *Project
	Truth   *TruthManifest
	Tiles   []Tile
	Config  SchedulerConfig
	Store   *CheckpointStore
	Order   *OrderDecision
	Graph   *DepGraph

	failMu       sync.Mutex
	tileFailures map[int]int
}

func NewEngine(p *Project, truth *TruthManifest, cfg SchedulerConfig, store *CheckpointStore) (*Engine, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	tiles := BuildTiles(p.Width, p.Height, DefaultTileSize)
	order, err := AdjudicateOrder(p, tiles)
	if err != nil {
		return nil, err
	}
	graph := BuildDependencyGraph(p, order, tiles)
	return &Engine{
		Project: p, Truth: truth, Tiles: tiles, Config: cfg, Store: store,
		Order: order, Graph: graph, tileFailures: map[int]int{},
	}, nil
}

// InjectTileFailure makes the next execution of tileID fail once; retry then succeeds.
func (e *Engine) InjectTileFailure(tileID int) {
	e.failMu.Lock()
	defer e.failMu.Unlock()
	e.tileFailures[tileID]++
}

func (e *Engine) maybeInjectedFailure(tileID int) error {
	e.failMu.Lock()
	defer e.failMu.Unlock()
	if e.tileFailures[tileID] > 0 {
		e.tileFailures[tileID]--
		return fmt.Errorf("injected tile %d raster/accumulation failure", tileID)
	}
	return nil
}

func (e *Engine) computeTile(tile Tile, committed map[string]CheckpointRecord) (TileComputation, error) {
	if err := e.maybeInjectedFailure(tile.ID); err != nil {
		return TileComputation{}, err
	}
	r := tile.Rect
	n := r.Dx() * r.Dy()
	if n > e.Config.MaxPixelsPerWorker {
		return TileComputation{}, paramError("scheduler.max_pixels_per_worker", fmt.Sprintf("<=%d", e.Config.MaxPixelsPerWorker), "tile %d pixel buffer %d exceeds cap", tile.ID, n)
	}
	inputHash := TileInputHash(e.Project.Candidate, tile)
	tileOrder, ok := e.Order.Tile(tile.ID)
	if !ok {
		tileOrder = TileOrder{TileID: tile.ID, Basis: e.Order.Policy}
	}

	alpha := make([]uint16, n)
	support := make([]byte, n)
	for i := range alpha {
		x := r.X0 + i%r.Dx()
		y := r.Y0 + i/r.Dx()
		alpha[i] = 0
		if e.Project.Candidate.Get(x, y) != 0 {
			alpha[i] = WeightBasisScale
		}
	}

	out := TileComputation{
		Tile: tile, InputHash: inputHash, Alpha: alpha,
		ComponentKeys: map[int]string{},
	}
	upstreamKey := inputHash
	previousAccumKey := inputHash
	strokeByIndex := map[int]Stroke{}
	for _, s := range e.Project.Strokes {
		strokeByIndex[s.Index] = s
	}

	for _, idx := range tileOrder.Order {
		s := strokeByIndex[idx]
		comp, err := RasterStrokeTile(e.Project, s, tile)
		if err != nil {
			return TileComputation{}, err
		}
		strokeHash := StrokeHash(s)
		key := ComponentCacheKey(inputHash, s.ID, strokeHash, e.Order.OrderVersion, ParameterVersion, upstreamKey)
		out.ComponentKeys[idx] = key

		if committed != nil && e.Store != nil {
			if old, ok := committed[checkpointKey(e.Project.ProjectID, idx, tile.ID)]; ok && old.CacheKey == key && old.ContentHash == comp.ContentHash {
				if len(old.Alpha) == len(comp.Alpha) {
					comp.Alpha = old.Alpha
				}
			}
		}

		out.Records = append(out.Records, CheckpointRecord{
			ProjectID: e.Project.ProjectID, StrokeIndex: idx, TileID: tile.ID,
			Alpha: comp.Alpha, CacheKey: key, ContentHash: comp.ContentHash,
			ResidualDigest: e.Project.ResidualDigest, OrderVersion: e.Order.OrderVersion,
			AlgoVersion: AlgorithmVersion, ParamVersion: ParameterVersion,
		})
		for i, src := range comp.Alpha {
			if src > 0 {
				support[i] = 1
			}
			alpha[i] = CompositeOver(alpha[i], src)
		}
		upstreamKey = key
		previousAccumKey = AccumCacheKey(key, previousAccumKey, e.Order.OrderVersion, ParameterVersion)
	}

	localMask := make([]byte, n)
	for i := range alpha {
		if support[i] == 0 {
			alpha[i] = 0
		}
		if alpha[i] >= uint16(e.Project.CutAlphaBPS) {
			localMask[i] = 1
		}
	}
	out.Alpha = alpha
	out.LocalMask = localMask
	out.AccumKey = previousAccumKey
	out.OutputHash = HashBytes(localMask)
	return out, nil
}

func (e *Engine) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	result := &RunResult{Order: e.Order, Graph: e.Graph, Invalidation: opts.Invalidation}
	var committed map[string]CheckpointRecord
	if e.Store != nil && opts.Recover {
		rec, cps, err := e.Store.Recover(ctx, e.Project.ProjectID, e.Project.ResidualDigest, e.Order.OrderVersion)
		if err != nil {
			return result, err
		}
		result.Recovery = rec
		committed = cps
	}
	if e.Store != nil {
		if err := e.Store.EnsureMeta(ctx, e.Project.ProjectID, e.Project.ResidualDigest, e.Order.OrderVersion); err != nil {
			return result, err
		}
	}

	workTiles := e.Tiles
	if opts.Invalidation != nil {
		workTiles = tilesByIDs(e.Tiles, opts.Invalidation.AffectedTiles)
	}

	computedByTile := map[int]TileComputation{}
	if len(workTiles) > 0 {
		results, trace := RunTileTasks(ctx, workTiles, e.Config, func(ctx context.Context, tile Tile, workerID int) (TileComputation, error) {
			return e.computeTile(tile, committed)
		})
		result.SchedulerTrace = trace
		for _, tr := range results {
			if tr.Status == "failed" || tr.Err != nil {
				return result, tr.Err
			}
			computedByTile[tr.Tile.ID] = tr.Value
			result.RecomputedTiles = append(result.RecomputedTiles, tr.Tile.ID)
		}
	}
	sort.Ints(result.RecomputedTiles)

	mask := NewMask(e.Project.Width, e.Project.Height)
	for _, tile := range e.Tiles {
		var local []byte
		if comp, ok := computedByTile[tile.ID]; ok {
			local = comp.LocalMask
			if e.Store != nil {
				err := e.Store.SaveTileOutput(ctx, TileOutputRecord{
					ProjectID: e.Project.ProjectID, TileID: tile.ID, AccumKey: comp.AccumKey,
					InputHash: comp.InputHash, ContentHash: comp.OutputHash,
					Mask: comp.LocalMask, Alpha: comp.Alpha,
					OrderVersion: e.Order.OrderVersion, ParamVersion: ParameterVersion, AlgoVersion: AlgorithmVersion,
				})
				if err != nil {
					return result, err
				}
			}
		} else {
			if e.Store == nil {
				return result, &Error{Code: CodeDIFF, FirstTile: tile.ID, Message: "incremental run has no persistent tile output store"}
			}
			out, err := e.Store.LoadTileOutput(ctx, e.Project.ProjectID, tile.ID)
			if err != nil {
				return result, err
			}
			if out == nil || out.OrderVersion != e.Order.OrderVersion || out.AlgoVersion != AlgorithmVersion || out.ParamVersion != ParameterVersion {
				return result, &Error{Code: CodeDIFF, FirstTile: tile.ID, Message: "missing or stale persistent tile output"}
			}
			local = out.Mask
			result.ReusedTiles = append(result.ReusedTiles, tile.ID)
			result.LoadedTiles = append(result.LoadedTiles, tile.ID)
		}
		copyTileMask(mask, tile, local)
	}
	sort.Ints(result.ReusedTiles)
	result.Mask = mask

	if e.Store != nil {
		seq, err := e.Store.LoadCommitSequence(ctx, e.Project.ProjectID)
		if err != nil {
			return result, err
		}
		result.CommitSequence = seq
		skipBoundary := 0
		if result.Recovery != nil && !result.Recovery.Fresh && !result.Recovery.Invalidated {
			skipBoundary = result.Recovery.CommittedStrokeBoundary
		}
		if opts.Invalidation != nil {
			skipBoundary = 0
		}
		for _, idx := range e.Order.OrderedStrokes {
			if idx <= skipBoundary {
				continue
			}
			var recs []CheckpointRecord
			for tileID, comp := range computedByTile {
				_ = tileID
				for _, rec := range comp.Records {
					if rec.StrokeIndex == idx {
						recs = append(recs, rec)
					}
				}
			}
			sort.Slice(recs, func(i, j int) bool { return recs[i].TileID < recs[j].TileID })
			if len(recs) == 0 {
				continue
			}
			committedRecs, err := e.Store.CommitStroke(ctx, recs)
			if err != nil {
				return result, err
			}
			for _, rec := range committedRecs {
				result.CommitSequence = append(result.CommitSequence, rec)
			}
		}
	}

	report := &VerificationReport{
		ProjectID: e.Project.ProjectID, AlgorithmVersion: AlgorithmVersion,
		ParameterVersion: ParameterVersion, OrderVersion: e.Order.OrderVersion,
		MaskWidth: mask.Width, MaskHeight: mask.Height, MaskHash: HashBytes(mask.Pix),
		OK: true,
	}
	topo, topoErr := VerifyTopology(mask, e.Truth)
	if topo != nil {
		report.Topology = topo
		if !topo.OK {
			report.OK = false
		}
	}
	dir, dirErr := VerifyDirections(mask, e.Truth)
	if dir != nil {
		report.Direction = dir
		if !dir.OK {
			report.OK = false
		}
	}
	result.Report = report
	if topoErr != nil {
		return result, topoErr
	}
	if dirErr != nil {
		// Direction mismatches are fully represented in report; return the structured error too.
		return result, dirErr
	}
	return result, nil
}

// RunPrefix computes all tiles but commits only the first k adjudicated strokes. Used to simulate a crash boundary.
func (e *Engine) RunPrefix(ctx context.Context, k int) (*RunResult, error) {
	if e.Store == nil {
		return nil, fmt.Errorf("prefix run requires checkpoint store")
	}
	if err := e.Store.EnsureMeta(ctx, e.Project.ProjectID, e.Project.ResidualDigest, e.Order.OrderVersion); err != nil {
		return nil, err
	}
	results, _ := RunTileTasks(ctx, e.Tiles, e.Config, func(ctx context.Context, tile Tile, workerID int) (TileComputation, error) {
		return e.computeTile(tile, nil)
	})
	byTile := map[int]TileComputation{}
	for _, tr := range results {
		if tr.Err != nil {
			return nil, tr.Err
		}
		byTile[tr.Tile.ID] = tr.Value
	}
	for n, idx := range e.Order.OrderedStrokes {
		if n >= k {
			break
		}
		var recs []CheckpointRecord
		for _, comp := range byTile {
			for _, rec := range comp.Records {
				if rec.StrokeIndex == idx {
					recs = append(recs, rec)
				}
			}
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].TileID < recs[j].TileID })
		if _, err := e.Store.CommitStroke(ctx, recs); err != nil {
			return nil, err
		}
	}
	seq, err := e.Store.LoadCommitSequence(ctx, e.Project.ProjectID)
	if err != nil {
		return nil, err
	}
	return &RunResult{Order: e.Order, CommitSequence: seq}, nil
}

func tilesByIDs(all []Tile, ids []int) []Tile {
	wanted := map[int]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []Tile
	for _, t := range all {
		if wanted[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

func copyTileMask(global *Mask, tile Tile, local []byte) {
	r := tile.Rect
	w := r.Dx()
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < w; x++ {
			v := local[y*w+x]
			global.Set(r.X0+x, r.Y0+y, v != 0)
		}
	}
}

// LogicalCommit is the comparable stable commit identity without database sequence/timestamps.
type LogicalCommit struct {
	Seq            int64  `json:"seq"`
	StrokeIndex    int    `json:"stroke_index"`
	TileID         int    `json:"tile_id"`
	CacheKey       string `json:"cache_key"`
	ContentHash    string `json:"content_hash"`
	ResidualDigest string `json:"residual_digest"`
	OrderVersion   string `json:"order_version"`
	AlgoVersion    string `json:"algo_version"`
	ParamVersion   string `json:"param_version"`
}

func LogicalSequence(recs []CheckpointRecord) []LogicalCommit {
	out := make([]LogicalCommit, 0, len(recs))
	for _, r := range recs {
		out = append(out, LogicalCommit{
			Seq: r.Seq, StrokeIndex: r.StrokeIndex, TileID: r.TileID,
			CacheKey: r.CacheKey, ContentHash: r.ContentHash,
			ResidualDigest: r.ResidualDigest, OrderVersion: r.OrderVersion,
			AlgoVersion: r.AlgoVersion, ParamVersion: r.ParamVersion,
		})
	}
	return out
}

// FirstSequenceDiff returns the first differing tile/sequence position.
func FirstSequenceDiff(a, b []LogicalCommit) (tileID int, index int, message string) {
	if len(a) != len(b) {
		tile := -1
		if len(a) > 0 {
			tile = a[minInt(len(a), len(b))].TileID
		}
		return tile, minInt(len(a), len(b)), fmt.Sprintf("sequence length %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i].TileID, i, fmt.Sprintf("commit %d differs: %+v != %+v", i, a[i], b[i])
		}
	}
	return -1, -1, ""
}

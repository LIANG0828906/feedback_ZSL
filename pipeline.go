package maskqc

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type OrderDecision struct {
	Order    int    `json:"order"`
	StrokeID string `json:"stroke_id"`
	PenSeq   int    `json:"pen_seq"`
	Priority int    `json:"priority"`
	Basis    string `json:"basis"`
}

type TileAudit struct {
	TileID     string          `json:"tile_id"`
	StrokeIDs  []string        `json:"stroke_ids"`
	PenSeqs    []int           `json:"pen_seqs"`
	Decisions  []OrderDecision `json:"decisions"`
	OrderBasis string          `json:"order_basis"`
}

type TileOutput struct {
	ID            string    `json:"id"`
	TX            int       `json:"tx"`
	TY            int       `json:"ty"`
	Rect          Rect      `json:"rect"`
	Mask          *Mask     `json:"mask,omitempty"`
	Key           string    `json:"key"`
	ComponentKeys []string  `json:"component_keys"`
	Reused        bool      `json:"reused"`
	Audit         TileAudit `json:"audit"`
	Attempt       int       `json:"attempt"`
}

type IconResult struct {
	IconID       string                 `json:"icon_id"`
	Width        int                    `json:"width"`
	Height       int                    `json:"height"`
	Accumulated  *Mask                  `json:"-"`
	Tiles        map[string]*TileOutput `json:"tiles"`
	TileOrder    []string               `json:"tile_order"`
	Order        []OrderDecision        `json:"order"`
	Analysis     *Analysis              `json:"analysis"`
	Topology     *TopologyReport        `json:"topology"`
	TopologyCode ErrorCode              `json:"topology_code,omitempty"`
	TileErrors   []JobResult            `json:"tile_errors,omitempty"`
}

type VerificationStep struct {
	EditIndex      int                 `json:"edit_index"`
	StrokeID       string              `json:"stroke_id"`
	Invalidation   *InvalidationReport `json:"invalidation"`
	TopologyIssues int                 `json:"topology_issues"`
	MaskHash       string              `json:"mask_hash"`
}

type PathResult struct {
	Path          string                 `json:"path"`
	DBPath        string                 `json:"db_path,omitempty"`
	ProjectHash   string                 `json:"project_hash"`
	EventSequence []string               `json:"event_sequence"`
	Icons         map[string]*IconResult `json:"icons"`
	IconOrder     []string               `json:"icon_order"`
	Steps         []VerificationStep     `json:"steps,omitempty"`
	Budget        BudgetSnapshot         `json:"budget"`
	Invalidations []*InvalidationReport  `json:"invalidations,omitempty"`
	Commits       []CommitRecord         `json:"commits,omitempty"`
}

type Engine struct {
	store     *Store
	scheduler *Scheduler
	traces    []FieldTrace

	failMu        sync.Mutex
	failOnceTiles map[string]bool
}

func NewEngine(store *Store, cfg BudgetConfig, traces []FieldTrace) *Engine {
	return &Engine{store: store, scheduler: NewScheduler(cfg), traces: traces, failOnceTiles: map[string]bool{}}
}

func (e *Engine) SchedulerSnapshot() BudgetSnapshot { return e.scheduler.Snapshot() }

// FailTileOnce makes the next execution of tileID fail, then automatically
// clears the injection so an explicit retry can succeed.
func (e *Engine) FailTileOnce(tileID string) {
	e.failMu.Lock()
	e.failOnceTiles[tileID] = true
	e.failMu.Unlock()
}

func (e *Engine) consumeFailOnce(tileID string) bool {
	e.failMu.Lock()
	defer e.failMu.Unlock()
	if e.failOnceTiles[tileID] {
		delete(e.failOnceTiles, tileID)
		return true
	}
	return false
}

func Adjudicate(icon *Icon) []OrderDecision {
	strokes := make([]*Stroke, len(icon.Strokes))
	copy(strokes, icon.Strokes)
	SortStrokes(strokes)
	out := make([]OrderDecision, 0, len(strokes))
	for i, s := range strokes {
		out = append(out, OrderDecision{
			Order: i + 1, StrokeID: s.ID, PenSeq: s.Seq, Priority: s.Priority,
			Basis: fmt.Sprintf("process_priority=%d ascending, then pen_no=%d ascending, then stable id", s.Priority, s.Seq),
		})
	}
	return out
}

func (e *Engine) residualDigest(strokeID string) string {
	var filtered []FieldTrace
	for _, tr := range e.traces {
		if tr.StrokeID == strokeID || tr.StrokeID == "" {
			filtered = append(filtered, tr)
		}
	}
	if len(filtered) == 0 {
		return HashBytes([]byte("no-conversion-remainder"))
	}
	return ResidualDigest(filtered)
}

type tileKeyInfo struct {
	output        *TileOutput
	componentKeys []string
}

func buildTileKeyInfo(icon *Icon, order []*Stroke, tx, ty int) tileKeyInfo {
	id := TileID(icon.ID, tx, ty)
	rect := TileRect(icon, tx, ty)
	out := &TileOutput{ID: id, TX: tx, TY: ty, Rect: rect}
	audit := TileAudit{TileID: id, OrderBasis: "global priority+seq adjudication filtered to raster-bounds participants"}
	var compKeys []string
	var decisions []OrderDecision

	candidateTile, _ := CropMask(icon.Candidate, rect)
	inputHash := candidateTile.Hash()
	for rank, s := range order {
		if StrokeBounds(*s, icon.Width, icon.Height).Intersect(rect).Empty() {
			continue
		}
		key := BuildCacheKey(CacheKeyParts{
			TileID: id, TileContentHash: inputHash, StrokeID: s.ID, StrokeContentHash: s.ContentHash(),
			SynthesisOrder: rank + 1, UpstreamKeys: []string{"candidate:" + inputHash},
		})
		compKeys = append(compKeys, key)
		audit.StrokeIDs = append(audit.StrokeIDs, s.ID)
		audit.PenSeqs = append(audit.PenSeqs, s.Seq)
		decisions = append(decisions, OrderDecision{Order: rank + 1, StrokeID: s.ID, PenSeq: s.Seq, Priority: s.Priority, Basis: "priority then pen sequence"})
	}
	audit.Decisions = decisions
	out.Audit = audit
	out.ComponentKeys = compKeys
	out.Key = BuildCacheKey(CacheKeyParts{
		TileID: id, TileContentHash: inputHash, SynthesisOrder: len(audit.StrokeIDs),
		UpstreamKeys: append([]string{"candidate:" + inputHash}, compKeys...),
	})
	return tileKeyInfo{output: out, componentKeys: compKeys}
}

// ComputeTiles evaluates all tiles. Persisted tile keys are reused when the
// store is present and the key matches. Failed jobs are returned, not thrown.
func (e *Engine) ComputeTiles(ctx context.Context, icon *Icon) (map[string]*TileOutput, []JobResult, error) {
	orderDecisions := Adjudicate(icon)
	order := make([]*Stroke, 0, len(orderDecisions))
	byID := map[string]*Stroke{}
	for _, s := range icon.Strokes {
		byID[s.ID] = s
	}
	for _, d := range orderDecisions {
		order = append(order, byID[d.StrokeID])
	}

	tileIDs := icon.TileIDs()
	outputs := make(map[string]*TileOutput, len(tileIDs))
	infos := make([]tileKeyInfo, len(tileIDs))
	jobs := make([]TileJob, 0, len(tileIDs))
	for i, id := range tileIDs {
		tx, ty, ok := TileCoords(id)
		if !ok {
			return nil, nil, newError(ESchema, "tile_id", "cannot parse tile coordinates from %q", id)
		}
		info := buildTileKeyInfo(icon, order, tx, ty)
		infos[i] = info
		outputs[id] = info.output
		idx := i
		jobs = append(jobs, TileJob{ID: id, Pixels: info.output.Rect.Width() * info.output.Rect.Height(), Run: func(ctx context.Context) error {
			out := infos[idx].output
			out.Attempt++
			if e.consumeFailOnce(id) {
				return newError(EBudget, id, "injected tile failure")
			}
			if e.store != nil {
				rec, err := e.store.GetTileResult(out.Key)
				if err != nil {
					return err
				}
				if rec != nil && rec.Mask != nil && rec.Mask.Hash() == rec.Hash {
					out.Mask = rec.Mask
					out.Reused = true
					return nil
				}
			}
			mask, participants, err := ComposeTile(icon, out.Rect, order)
			if err != nil {
				return err
			}
			out.Mask = mask
			out.Reused = false
			if len(participants) != len(out.Audit.StrokeIDs) {
				// ComposeTile uses the same intersection rule; guard against drift.
				out.Audit.StrokeIDs = participants
			}
			if e.store != nil {
				if err := e.store.PutTileResult(TileResultRecord{CacheKey: out.Key, IconID: icon.ID, TileID: id, Hash: mask.Hash(), Mask: mask, EventKey: "tile-compute"}); err != nil {
					return err
				}
			}
			return nil
		}})
	}
	results := e.scheduler.Run(ctx, jobs)
	var jobErrs []JobResult
	for _, r := range results {
		if !r.OK {
			jobErrs = append(jobErrs, r)
		}
	}
	return outputs, jobErrs, nil
}

func (e *Engine) BuildIconResult(ctx context.Context, icon *Icon, compareTruth bool) (*IconResult, error) {
	outputs, errs, err := e.ComputeTiles(ctx, icon)
	if err != nil {
		return nil, err
	}
	res := &IconResult{IconID: icon.ID, Width: icon.Width, Height: icon.Height, Tiles: outputs, Order: Adjudicate(icon), TileErrors: errs}
	for _, id := range icon.TileIDs() {
		res.TileOrder = append(res.TileOrder, id)
	}
	if len(errs) > 0 {
		return res, &Error{Code: EBudget, FieldPath: "tiles", Message: fmt.Sprintf("%d tile(s) failed", len(errs)), Details: map[string]any{"failures": errs}}
	}

	acc := NewMask(icon.Width, icon.Height)
	for _, id := range res.TileOrder {
		t := outputs[id]
		if t.Mask == nil {
			return res, newError(EBudget, id, "tile has no mask after execution")
		}
		for y := 0; y < t.Rect.Height(); y++ {
			for x := 0; x < t.Rect.Width(); x++ {
				gx, gy := t.Rect.MinX+x, t.Rect.MinY+y
				acc.Alpha[gy*icon.Width+gx] = t.Mask.Alpha[y*t.Rect.Width()+x]
			}
		}
	}
	res.Accumulated = acc
	analysis, err := AnalyzeMask(acc, icon.Strokes)
	if err != nil {
		return res, err
	}
	res.Analysis = analysis
	if compareTruth {
		topo, err := CompareTopology(acc, icon.Truth, icon.Strokes, DefaultTopologyTolerance())
		if err != nil {
			return res, err
		}
		res.Topology = topo
		if terr := topo.Error(); terr != nil {
			if de, ok := AsError(terr); ok {
				res.TopologyCode = de.Code
			}
		}
	}
	return res, nil
}

func EventKey(ordinal int, s *Stroke) string {
	return fmt.Sprintf("evt-%04d:%s:%s", ordinal, s.ID, s.ContentHash()[:12])
}

// CommitStrokeComponents rasterizes one stroke on all affected tiles and
// persists all components in one SQLite transaction.
func (e *Engine) CommitStrokeComponents(ctx context.Context, p *Project, icon *Icon, s *Stroke, synthesisRank, ordinal int) (string, error) {
	eventKey := EventKey(ordinal, s)
	if e.store == nil {
		return eventKey, nil
	}
	tileIDs := AffectedTileIDs(icon, s)
	comps := make([]*Mask, len(tileIDs))
	jobs := make([]TileJob, 0, len(tileIDs))
	for i, tileID := range tileIDs {
		tx, ty, ok := TileCoords(tileID)
		if !ok {
			return "", newError(ESchema, "tile_id", "cannot parse %q", tileID)
		}
		rect := TileRect(icon, tx, ty)
		idx := i
		jobs = append(jobs, TileJob{ID: tileID, Pixels: rect.Width() * rect.Height(), Run: func(ctx context.Context) error {
			comp, err := RasterizeStrokeRect(s, rect)
			if err != nil {
				return err
			}
			comps[idx] = comp
			return nil
		}})
	}
	results := e.scheduler.Run(ctx, jobs)
	for _, r := range results {
		if !r.OK {
			return eventKey, r.Error
		}
	}

	records := make([]ComponentRecord, 0, len(tileIDs))
	for i, tileID := range tileIDs {
		tx, ty, _ := TileCoords(tileID)
		rect := TileRect(icon, tx, ty)
		candidateTile, err := CropMask(icon.Candidate, rect)
		if err != nil {
			return "", err
		}
		inputHash := candidateTile.Hash()
		key := BuildCacheKey(CacheKeyParts{
			TileID: tileID, TileContentHash: inputHash, StrokeID: s.ID, StrokeContentHash: s.ContentHash(),
			SynthesisOrder: synthesisRank, UpstreamKeys: []string{"candidate:" + inputHash},
		})
		records = append(records, ComponentRecord{
			TileID: tileID, StrokeID: s.ID, CacheKey: key, ComponentHash: comps[i].Hash(), InputHash: inputHash,
			ResidualDigest: e.residualDigest(s.ID), OrderVersion: OrderRulesVersion,
			ConversionVersion: ConversionParamsVersion, AlgorithmVersion: AlgorithmVersion,
			Width: rect.Width(), Height: rect.Height(), Alpha: comps[i].Alpha,
		})
	}
	_, err := e.store.CommitStroke(ctx, StrokeCommit{
		EventKey: eventKey, IconID: icon.ID, StrokeID: s.ID, StrokeVersion: s.ContentHash(),
		OrderIndex: ordinal, Components: records,
	})
	return eventKey, err
}

func sortedTileIDs(m map[string]*TileOutput) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

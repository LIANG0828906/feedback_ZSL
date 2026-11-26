package maskqc

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
)

const HarnessIconID = "glyph-bars-48"

type ThreePathOptions struct {
	WorkDir    string
	Budget     BudgetConfig
	CrashAfter int
	Seed       int64
}

type ThreePathReport struct {
	Incremental *PathResult `json:"incremental"`
	Recovery    *PathResult `json:"recovery"`
	Full        *PathResult `json:"full"`
	Diff        *Error      `json:"diff,omitempty"`
}

func HarnessProject(seed int64) (*Project, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	canonical, err := GenerateSample(seed)
	if err != nil {
		return nil, err
	}
	// Exercise all integer/schema conversion traces in the acceptance harness.
	data, _, err := ExportProject(canonical, SchemaV2)
	if err != nil {
		return nil, err
	}
	return LoadProject(data)
}

func StandardEdits() []Edit {
	weight := 8200
	start := Point{10, 24}
	seq := 9
	return []Edit{
		{Type: EditUpdate, StrokeID: "bars-mid", NewWeight: &weight},
		{Type: EditUpdate, StrokeID: "bars-bottom", NewStart: &start},
		{Type: EditInsert, StrokeID: "bars-extra", Insert: &Stroke{
			ID: "bars-extra", Seq: seq, Priority: 30, Group: "bars",
			Start: Point{10, 29}, End: Point{38, 29}, Radius: 1,
			HardnessPermille: HardnessScale, WeightPermyriad: WeightScale,
			DeclaredDirection: DirAlong, CompositionOrder: DefaultCompositionOrder,
		}},
	}
}

func metaFor(p *Project) RecoveryMeta {
	return RecoveryMeta{
		ProjectHash:       ProjectIdentityHash(p),
		OrderVersion:      OrderRulesVersion,
		ConversionVersion: ConversionParamsVersion,
		AlgorithmVersion:  AlgorithmVersion,
		TopologyVersion:   AlgorithmVersion,
	}
}

func RunThreePaths(ctx context.Context, opts ThreePathOptions) (*ThreePathReport, error) {
	if opts.Seed == 0 {
		opts.Seed = DefaultSeed
	}
	if opts.Budget.MaxParallel == 0 {
		opts.Budget = DefaultBudgetConfig()
	}
	if opts.CrashAfter <= 0 {
		opts.CrashAfter = 1
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}

	inc, err := runIncrementalPath(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("incremental path: %w", err)
	}
	rec, err := runRecoveryPath(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("recovery path: %w", err)
	}
	full, err := runFullPath(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("full path: %w", err)
	}
	rep := &ThreePathReport{Incremental: inc, Recovery: rec, Full: full}
	if de := ComparePathResults(full, inc); de != nil {
		rep.Diff = de
		return rep, de
	}
	if de := ComparePathResults(full, rec); de != nil {
		rep.Diff = de
		return rep, de
	}
	return rep, nil
}

func runIncrementalPath(ctx context.Context, opts ThreePathOptions) (*PathResult, error) {
	p, err := HarnessProject(opts.Seed)
	if err != nil {
		return nil, err
	}
	icon := p.Icon(HarnessIconID)
	dbPath := filepath.Join(opts.WorkDir, "incremental-checkpoint.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	meta := metaFor(p)
	if err := store.Rebuild(meta); err != nil {
		return nil, err
	}
	engine := NewEngine(store, opts.Budget, p.ConvertTrace)
	pr := &PathResult{Path: "realtime-incremental", DBPath: dbPath, ProjectHash: meta.ProjectHash, Icons: map[string]*IconResult{}, IconOrder: []string{HarnessIconID}}

	order := p.SortedStrokes(HarnessIconID)
	for i, s := range order {
		key, err := engine.CommitStrokeComponents(ctx, p, icon, s, i+1, i+1)
		if err != nil {
			return nil, err
		}
		pr.EventSequence = append(pr.EventSequence, key)
	}
	iconResult, err := engine.BuildIconResult(ctx, icon, true)
	if err != nil {
		return nil, err
	}
	pr.Icons[icon.ID] = iconResult

	ordinal := len(order)
	for editIndex, edit := range StandardEdits() {
		old, current, err := ApplyEdit(p, HarnessIconID, edit)
		if err != nil {
			return nil, err
		}
		inv := PlanInvalidation(icon, edit, old, current)
		pr.Invalidations = append(pr.Invalidations, inv)
		ordinal++
		rank := currentSynthesisRank(p, icon.ID, current.ID)
		key, err := engine.CommitStrokeComponents(ctx, p, icon, current, rank, ordinal)
		if err != nil {
			return nil, err
		}
		pr.EventSequence = append(pr.EventSequence, key)
		iconResult, err = engine.BuildIconResult(ctx, icon, true)
		if err != nil {
			return nil, err
		}
		pr.Icons[icon.ID] = iconResult
		issueCount := 0
		if iconResult.Topology != nil {
			issueCount = len(iconResult.Topology.Issues)
		}
		pr.Steps = append(pr.Steps, VerificationStep{EditIndex: editIndex, StrokeID: current.ID, Invalidation: inv, TopologyIssues: issueCount, MaskHash: iconResult.Accumulated.Hash()})
	}
	pr.Budget = engine.SchedulerSnapshot()
	pr.Commits, err = store.ListCommits(HarnessIconID)
	if err != nil {
		return nil, err
	}
	return pr, nil
}

func runRecoveryPath(ctx context.Context, opts ThreePathOptions) (*PathResult, error) {
	p, err := HarnessProject(opts.Seed)
	if err != nil {
		return nil, err
	}
	icon := p.Icon(HarnessIconID)
	dbPath := filepath.Join(opts.WorkDir, "recovery-checkpoint.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	meta := metaFor(p)
	if err := store.Rebuild(meta); err != nil {
		_ = store.Close()
		return nil, err
	}
	order := p.SortedStrokes(HarnessIconID)
	crashAfter := opts.CrashAfter
	if crashAfter > len(order) {
		crashAfter = len(order)
	}
	crashEngine := NewEngine(store, opts.Budget, p.ConvertTrace)
	for i := 0; i < crashAfter; i++ {
		if _, err := crashEngine.CommitStrokeComponents(ctx, p, icon, order[i], i+1, i+1); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	// Hard termination boundary: close without any in-memory/tile state. SQLite
	// transactions guarantee that only the completed stroke commits survive.
	if err := store.Close(); err != nil {
		return nil, err
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer store2.Close()
	if err := store2.ValidateRecovery(meta); err != nil {
		return nil, err
	}
	last, err := store2.LastCommit(HarnessIconID)
	if err != nil {
		return nil, err
	}
	startOrdinal := crashAfter
	if last != nil {
		startOrdinal = int(last.OrderIndex)
	}
	engine := NewEngine(store2, opts.Budget, p.ConvertTrace)
	pr := &PathResult{Path: "checkpoint-recovery", DBPath: dbPath, ProjectHash: meta.ProjectHash, Icons: map[string]*IconResult{}, IconOrder: []string{HarnessIconID}}
	for i := 0; i < startOrdinal; i++ {
		pr.EventSequence = append(pr.EventSequence, EventKey(i+1, order[i]))
	}
	for i := startOrdinal; i < len(order); i++ {
		ordinal := i + 1
		key, err := engine.CommitStrokeComponents(ctx, p, icon, order[i], ordinal, ordinal)
		if err != nil {
			return nil, err
		}
		pr.EventSequence = append(pr.EventSequence, key)
	}

	iconResult, err := engine.BuildIconResult(ctx, icon, true)
	if err != nil {
		return nil, err
	}
	pr.Icons[icon.ID] = iconResult
	ordinal := len(order)
	for editIndex, edit := range StandardEdits() {
		old, current, err := ApplyEdit(p, HarnessIconID, edit)
		if err != nil {
			return nil, err
		}
		inv := PlanInvalidation(icon, edit, old, current)
		pr.Invalidations = append(pr.Invalidations, inv)
		ordinal++
		rank := currentSynthesisRank(p, icon.ID, current.ID)
		key, err := engine.CommitStrokeComponents(ctx, p, icon, current, rank, ordinal)
		if err != nil {
			return nil, err
		}
		pr.EventSequence = append(pr.EventSequence, key)
		iconResult, err = engine.BuildIconResult(ctx, icon, true)
		if err != nil {
			return nil, err
		}
		pr.Icons[icon.ID] = iconResult
		pr.Steps = append(pr.Steps, VerificationStep{EditIndex: editIndex, StrokeID: current.ID, Invalidation: inv, TopologyIssues: len(iconResult.Topology.Issues), MaskHash: iconResult.Accumulated.Hash()})
	}
	pr.Budget = engine.SchedulerSnapshot()
	pr.Commits, err = store2.ListCommits(HarnessIconID)
	if err != nil {
		return nil, err
	}
	return pr, nil
}

func runFullPath(ctx context.Context, opts ThreePathOptions) (*PathResult, error) {
	p, err := HarnessProject(opts.Seed)
	if err != nil {
		return nil, err
	}
	meta := metaFor(p)
	engine := NewEngine(nil, opts.Budget, p.ConvertTrace)
	pr := &PathResult{Path: "one-shot-full-recompute", ProjectHash: meta.ProjectHash, Icons: map[string]*IconResult{}, IconOrder: []string{HarnessIconID}}
	order := p.SortedStrokes(HarnessIconID)
	ordinal := 0
	for _, s := range order {
		ordinal++
		pr.EventSequence = append(pr.EventSequence, EventKey(ordinal, s))
	}
	icon := p.Icon(HarnessIconID)
	for editIndex, edit := range StandardEdits() {
		old, current, err := ApplyEdit(p, HarnessIconID, edit)
		if err != nil {
			return nil, err
		}
		inv := PlanInvalidation(icon, edit, old, current)
		pr.Invalidations = append(pr.Invalidations, inv)
		ordinal++
		pr.EventSequence = append(pr.EventSequence, EventKey(ordinal, current))
		_ = old
		_ = editIndex
	}
	result, err := engine.BuildIconResult(ctx, icon, true)
	if err != nil {
		return nil, err
	}
	pr.Icons[icon.ID] = result
	pr.Budget = engine.SchedulerSnapshot()
	return pr, nil
}

func currentSynthesisRank(p *Project, iconID, strokeID string) int {
	for i, s := range p.SortedStrokes(iconID) {
		if s.ID == strokeID {
			return i + 1
		}
	}
	return 0
}

func (r *IconResult) SequenceMap() map[string][]string {
	out := map[string][]string{}
	for id, tile := range r.Tiles {
		out[id] = append([]string(nil), tile.Audit.StrokeIDs...)
	}
	return out
}

func ComparePathResults(expected, actual *PathResult) *Error {
	if !reflect.DeepEqual(expected.EventSequence, actual.EventSequence) {
		for i := 0; i < maxInt(len(expected.EventSequence), len(actual.EventSequence)); i++ {
			var e, a string
			if i < len(expected.EventSequence) {
				e = expected.EventSequence[i]
			}
			if i < len(actual.EventSequence) {
				a = actual.EventSequence[i]
			}
			if e != a {
				return &Error{Code: EDiff, FieldPath: fmt.Sprintf("event_sequence[%d]", i), Message: "commit event sequence differs", Details: map[string]any{"index": i, "expected": e, "actual": a}}
			}
		}
	}
	for _, iconID := range expected.IconOrder {
		ei := expected.Icons[iconID]
		ai := actual.Icons[iconID]
		if ai == nil {
			return &Error{Code: EDiff, FieldPath: "icons." + iconID, Message: "missing icon result"}
		}
		if de := compareSequenceMaps(iconID, ei.SequenceMap(), ai.SequenceMap()); de != nil {
			return de
		}
		for _, tileID := range ei.TileOrder {
			et := ei.Tiles[tileID]
			at := ai.Tiles[tileID]
			if et == nil || at == nil || et.Mask == nil || at.Mask == nil {
				return &Error{Code: EDiff, FieldPath: "tiles." + tileID, Message: "missing tile mask"}
			}
			if len(et.Mask.Alpha) != len(at.Mask.Alpha) {
				return &Error{Code: EDiff, FieldPath: "tiles." + tileID + ".alpha.length", Message: "tile mask length differs", Details: map[string]any{"tile_id": tileID}}
			}
			for i := range et.Mask.Alpha {
				if et.Mask.Alpha[i] == at.Mask.Alpha[i] {
					continue
				}
				localX := i % et.Rect.Width()
				localY := i / et.Rect.Width()
				tx, ty, _ := TileCoords(tileID)
				return &Error{
					Code: EDiff, FieldPath: fmt.Sprintf("tiles.%s.alpha[%d]", tileID, i),
					Message: "first differing tile pixel",
					Details: map[string]any{
						"tile_id": tileID, "tile_x": tx, "tile_y": ty,
						"x": et.Rect.MinX + localX, "y": et.Rect.MinY + localY,
						"local_x": localX, "local_y": localY, "field": "alpha",
						"expected": et.Mask.Alpha[i], "actual": at.Mask.Alpha[i],
					},
				}
			}
		}
		if de := compareAnalysis(iconID, ei.Analysis, ai.Analysis); de != nil {
			return de
		}
	}
	return nil
}

func compareSequenceMaps(iconID string, expected, actual map[string][]string) *Error {
	for tileID, eSeq := range expected {
		aSeq := actual[tileID]
		if !reflect.DeepEqual(eSeq, aSeq) {
			return &Error{Code: EDiff, FieldPath: fmt.Sprintf("icons.%s.tiles.%s.participants", iconID, tileID), Message: "tile synthesis sequence differs", Details: map[string]any{"tile_id": tileID, "expected": eSeq, "actual": aSeq}}
		}
	}
	return nil
}

func compareAnalysis(iconID string, e, a *Analysis) *Error {
	if e == nil || a == nil {
		return &Error{Code: EDiff, FieldPath: "icons." + iconID + ".analysis", Message: "missing analysis"}
	}
	if len(e.Domains) != len(a.Domains) {
		return &Error{Code: EDiff, FieldPath: fmt.Sprintf("icons.%s.analysis.domain_count", iconID), Message: "domain count differs", Details: map[string]any{"expected": len(e.Domains), "actual": len(a.Domains)}}
	}
	for i := range e.Domains {
		ed, ad := e.Domains[i], a.Domains[i]
		path := fmt.Sprintf("icons.%s.domains[%d]", iconID, i)
		checks := []struct {
			field string
			eq    bool
			e, a  any
		}{
			{"pixel_count", ed.PixelCount == ad.PixelCount, ed.PixelCount, ad.PixelCount},
			{"centroid_x1000", ed.CentroidX1000 == ad.CentroidX1000, ed.CentroidX1000, ad.CentroidX1000},
			{"centroid_y1000", ed.CentroidY1000 == ad.CentroidY1000, ed.CentroidY1000, ad.CentroidY1000},
			{"bounds", ed.Bounds == ad.Bounds, ed.Bounds, ad.Bounds},
			{"hole_count", ed.HoleCount == ad.HoleCount, ed.HoleCount, ad.HoleCount},
			{"band_width_px", ed.BandWidth == ad.BandWidth, ed.BandWidth, ad.BandWidth},
			{"direction", ed.Direction == ad.Direction, ed.Direction, ad.Direction},
		}
		for _, c := range checks {
			if !c.eq {
				return &Error{Code: EDiff, FieldPath: path + "." + c.field, Message: "domain statistic differs", Details: map[string]any{"domain_no": ed.No, "expected": c.e, "actual": c.a}}
			}
		}
	}
	return nil
}

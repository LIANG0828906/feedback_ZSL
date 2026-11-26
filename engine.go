package strokelab

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

type RunMode string

const (
	RunFull        RunMode = "full"
	RunIncremental RunMode = "incremental"
	RunResume      RunMode = "resume"
)

var ErrSimulatedAbort = errors.New("simulated abort after committed strokes")

type RunOptions struct {
	Mode              RunMode
	RunID             string
	Checkpoint        *Checkpoint
	Scheduler         SchedulerConfig
	FailureInjector   func(task BlockTask, attempt int) error
	StopAfterCommits  int
	ReuseCache        *BlockCache
	CacheOut          *BlockCache
	TopologyTolerance TopologyTolerance
}

type BlockResult struct {
	BlockID          int               `json:"block_id"`
	Rect             Rect              `json:"rect"`
	Mask             []uint8           `json:"mask"`
	CacheKey         string            `json:"cache_key"`
	Reused           bool              `json:"reused"`
	Recomputed       bool              `json:"recomputed"`
	OrderTrace       BlockOrderTrace   `json:"order_trace"`
	ComponentDigests map[string]string `json:"component_digests"`
}

type Evaluation struct {
	ProjectID        string             `json:"project_id"`
	Mode             RunMode            `json:"mode"`
	OrderVersion     string             `json:"order_version"`
	OrderedStrokes   []OrderedStroke    `json:"ordered_strokes"`
	Composite        []uint8            `json:"composite"`
	Blocks           []BlockResult      `json:"blocks"`
	Topology         *TopologyReport    `json:"topology"`
	Attribution      *AttributionReport `json:"attribution"`
	Checkpoint       CheckpointStatus   `json:"checkpoint"`
	Queue            []QueueRecord      `json:"queue"`
	ReusedBlocks     []int              `json:"reused_blocks"`
	RecomputedBlocks []int              `json:"recomputed_blocks"`
	CommittedStrokes int                `json:"committed_strokes"`
	Stopped          bool               `json:"stopped"`
	Cache            *BlockCache        `json:"-"`
}

type Engine struct {
	Project  *Project
	Ordered  []OrderedStroke
	Blocks   []Block
	Identity CheckpointIdentity
}

func NewEngine(project *Project) (*Engine, error) {
	if err := project.Validate(); err != nil {
		return nil, err
	}
	ordered, err := OrderStrokes(project.Strokes)
	if err != nil {
		return nil, err
	}
	return &Engine{
		Project: project, Ordered: ordered,
		Blocks:   BuildBlocks(project.Width, project.Height, project.BlockSize),
		Identity: ProjectIdentity(project, ordered),
	}, nil
}

// Run is the common engine used by all three verification paths. Full runs
// never read or write a journal. Incremental runs append one atomic record per
// stroke. Resume runs replay only committed version-matching records before
// continuing at the first uncommitted stroke.
func (e *Engine) Run(ctx context.Context, opts RunOptions) (*Evaluation, error) {
	if opts.Mode == "" {
		opts.Mode = RunFull
	}
	if opts.Mode != RunFull && opts.Mode != RunIncremental && opts.Mode != RunResume {
		return nil, coded(CodeVersion, "unknown run mode %q", opts.Mode)
	}
	if opts.TopologyTolerance.MinIoU == 0 {
		opts.TopologyTolerance = DefaultTopologyTolerance()
	}
	cfg, err := opts.Scheduler.normalized()
	if err != nil {
		return nil, err
	}
	scheduler, err := NewScheduler(cfg)
	if err != nil {
		return nil, err
	}
	scheduler.FailureInjector = opts.FailureInjector
	if opts.Mode != RunFull && opts.Checkpoint == nil {
		return nil, coded(CodeCheckpoint, "%s mode requires a checkpoint", opts.Mode)
	}
	if opts.ReuseCache != nil && opts.Mode != RunFull {
		return nil, coded(CodeCheckpoint, "block-cache reuse is supported on full recomputation only")
	}
	evaluation := &Evaluation{
		ProjectID: e.Project.ID, Mode: opts.Mode,
		OrderVersion: e.Identity.OrderVersion, OrderedStrokes: e.Ordered,
	}
	blockMasks := make(map[int][]uint8, len(e.Blocks))
	blockLocalComponents := make(map[int]map[string][]uint8, len(e.Blocks))
	for _, b := range e.Blocks {
		blockMasks[b.ID] = candidateBlock(e.Project, b.Rect)
		blockLocalComponents[b.ID] = make(map[string][]uint8)
	}
	componentsByStroke := make(map[string][]uint8, len(e.Ordered))
	frozen := make(map[int]bool)
	startStroke := 0
	committedTotal := 0

	if opts.Mode == RunResume {
		records, err := opts.Checkpoint.ResumePrefix(e.Identity)
		if err != nil {
			return nil, err
		}
		if len(records) > len(e.Ordered) {
			return nil, coded(CodeCheckpoint, "checkpoint has %d strokes but project has %d",
				len(records), len(e.Ordered))
		}
		for i, rec := range records {
			os := e.Ordered[i]
			if rec.StrokeID != os.Stroke.ID {
				return nil, coded(CodeCheckpoint,
					"checkpoint stroke %d is %s, project order expects %s",
					i, rec.StrokeID, os.Stroke.ID)
			}
			full := make([]uint8, e.Project.Width*e.Project.Height)
			for _, cr := range rec.Components {
				if cr.BlockID < 0 || cr.BlockID >= len(e.Blocks) {
					return nil, coded(CodeCheckpoint, "checkpoint references unknown block %d", cr.BlockID)
				}
				block := e.Blocks[cr.BlockID]
				if block.Rect != cr.Rect {
					return nil, coded(CodeCheckpoint,
						"checkpoint block %d rect mismatch: %+v vs %+v", cr.BlockID, cr.Rect, block.Rect)
				}
				if componentDigest(cr.Component) != cr.ComponentDigest {
					return nil, coded(CodeCheckpoint, "checkpoint block %d component digest mismatch", cr.BlockID)
				}
				parts := participantsForBlock(e.Ordered, block.Rect)
				wantKey := ComputeBlockCacheKey(e.Project, block.Rect, parts)
				if cr.CacheKey != wantKey {
					return nil, coded(CodeCheckpoint, "checkpoint block %d cache version mismatch", cr.BlockID)
				}
				applyComponent(blockMasks[block.ID], cr.Component)
				if contentHash(blockMasks[block.ID]) != cr.ContentHash {
					return nil, coded(CodeCheckpoint, "checkpoint block %d content hash mismatch", cr.BlockID)
				}
				blockLocalComponents[block.ID][rec.StrokeID] = append([]uint8(nil), cr.Component...)
				copyIntoFull(full, e.Project.Width, block.Rect, cr.Component)
			}
			componentsByStroke[rec.StrokeID] = full
		}
		startStroke = len(records)
		committedTotal = len(records)
	} else if opts.Mode == RunIncremental {
		if len(opts.Checkpoint.Records()) != 0 {
			return nil, coded(CodeCheckpoint,
				"incremental run requires an empty checkpoint; use resume for existing records")
		}
	}

	// A complete cache entry freezes a block whose full local input and local
	// composition order are unchanged. Only non-cached blocks enter the
	// stroke-wise task pipeline.
	if opts.ReuseCache != nil {
		for _, b := range e.Blocks {
			parts := participantsForBlock(e.Ordered, b.Rect)
			key := ComputeBlockCacheKey(e.Project, b.Rect, parts)
			entry, ok := opts.ReuseCache.Entries[key]
			if !ok || entry.BlockID != b.ID || entry.Rect != b.Rect ||
				len(entry.Mask) != b.Rect.PixelCount() {
				continue
			}
			blockMasks[b.ID] = append([]uint8(nil), entry.Mask...)
			for strokeID, comp := range entry.Components {
				if len(comp) != b.Rect.PixelCount() {
					continue
				}
				blockLocalComponents[b.ID][strokeID] = append([]uint8(nil), comp...)
				full := componentsByStroke[strokeID]
				if full == nil {
					full = make([]uint8, e.Project.Width*e.Project.Height)
				}
				copyIntoFull(full, e.Project.Width, b.Rect, comp)
				componentsByStroke[strokeID] = full
			}
			frozen[b.ID] = true
		}
	}

	newCommits := 0
	for ordinal := startStroke; ordinal < len(e.Ordered); ordinal++ {
		if err := ctx.Err(); err != nil {
			evaluation.Stopped = true
			evaluation.CommittedStrokes = committedTotal
			return finalizePartial(e, evaluation, blockMasks, blockLocalComponents,
					componentsByStroke, frozen, opts, scheduler), &CodedError{
					Code: CodeCanceled, Message: err.Error(), Err: err,
				}
		}
		os := e.Ordered[ordinal]
		fullComponent := componentsByStroke[os.Stroke.ID]
		if fullComponent == nil {
			fullComponent = make([]uint8, e.Project.Width*e.Project.Height)
		}
		var tasks []BlockTask
		blocksToProcess := make([]Block, 0, len(e.Blocks))
		for _, b := range e.Blocks {
			if frozen[b.ID] {
				continue
			}
			if !os.Stroke.Bounds().Intersects(b.Rect) {
				continue
			}
			blocksToProcess = append(blocksToProcess, b)
			block := b
			tasks = append(tasks, BlockTask{
				ID:            fmt.Sprintf("s%02d-b%03d", ordinal, b.ID),
				StrokeID:      os.Stroke.ID,
				StrokeOrdinal: ordinal,
				BlockID:       b.ID,
				Block:         b.Rect,
				Run: func() ([]uint8, error) {
					return RasterizeStrokeRect(os.Stroke, block.Rect), nil
				},
			})
		}
		results, err := scheduler.RunBatch(ctx, tasks)
		if err != nil {
			evaluation.CommittedStrokes = committedTotal
			return finalizePartial(e, evaluation, blockMasks, blockLocalComponents,
				componentsByStroke, frozen, opts, scheduler), err
		}
		var records []BlockComponentRecord
		for _, res := range results {
			block := e.Blocks[res.Task.BlockID]
			copyIntoFull(fullComponent, e.Project.Width, block.Rect, res.Component)
			applyComponent(blockMasks[block.ID], res.Component)
			blockLocalComponents[block.ID][os.Stroke.ID] = append([]uint8(nil), res.Component...)
			if opts.Checkpoint != nil {
				parts := participantsForBlock(e.Ordered, block.Rect)
				records = append(records, BlockComponentRecord{
					BlockID: block.ID, Rect: block.Rect,
					CacheKey:        ComputeBlockCacheKey(e.Project, block.Rect, parts),
					Component:       append([]byte(nil), res.Component...),
					ComponentDigest: componentDigest(res.Component),
					ContentHash:     contentHash(blockMasks[block.ID]),
					ResidualDigest:  e.Identity.ResidualDigest,
				})
			}
		}
		componentsByStroke[os.Stroke.ID] = fullComponent
		if opts.Checkpoint != nil {
			rec := StrokeCommit{
				RunID: opts.RunID, ProjectHash: e.Identity.ProjectHash,
				OrderVersion:      e.Identity.OrderVersion,
				ConversionVersion: e.Identity.ConversionVersion,
				AlgorithmVersion:  e.Identity.AlgorithmVersion,
				ResidualDigest:    e.Identity.ResidualDigest,
				StrokeID:          os.Stroke.ID, StrokeOrdinal: ordinal,
				Stage: os.Stroke.Stage, Seq: os.Stroke.Seq,
				BlockOrderVersion: BlockOrderVersionForCommit(records),
				Components:        records,
			}
			if _, err := opts.Checkpoint.AppendStroke(rec); err != nil {
				evaluation.CommittedStrokes = committedTotal
				return finalizePartial(e, evaluation, blockMasks, blockLocalComponents,
					componentsByStroke, frozen, opts, scheduler), err
			}
			committedTotal++
			newCommits++
			if opts.StopAfterCommits > 0 && newCommits >= opts.StopAfterCommits {
				evaluation.Stopped = true
				evaluation.CommittedStrokes = committedTotal
				partial := finalizePartial(e, evaluation, blockMasks, blockLocalComponents,
					componentsByStroke, frozen, opts, scheduler)
				return partial, ErrSimulatedAbort
			}
		}
	}
	evaluation.CommittedStrokes = committedTotal
	if opts.Checkpoint != nil {
		evaluation.Checkpoint = opts.Checkpoint.Status()
		evaluation.Checkpoint.CommittedStrokes = committedTotal
		evaluation.Checkpoint.Complete = committedTotal == len(e.Ordered)
	}
	cache := NewBlockCache()
	composite := assembleComposite(e.Project, e.Blocks, blockMasks)
	for _, b := range e.Blocks {
		parts := participantsForBlock(e.Ordered, b.Rect)
		key := ComputeBlockCacheKey(e.Project, b.Rect, parts)
		components := make(map[string][]uint8, len(parts))
		for _, p := range parts {
			if comp := blockLocalComponents[b.ID][p.Stroke.ID]; comp != nil {
				components[p.Stroke.ID] = append([]uint8(nil), comp...)
			}
		}
		trace := buildBlockOrderTrace(b.ID, parts, components)
		cache.Put(key, BlockCacheEntry{
			ProjectID: e.Project.ID, BlockID: b.ID, Rect: b.Rect,
			Mask:       append([]uint8(nil), blockMasks[b.ID]...),
			Components: components, OrderTrace: trace,
		})
		reused := frozen[b.ID]
		evaluation.Blocks = append(evaluation.Blocks, BlockResult{
			BlockID: b.ID, Rect: b.Rect, Mask: append([]uint8(nil), blockMasks[b.ID]...),
			CacheKey: key, Reused: reused, Recomputed: !reused, OrderTrace: trace,
			ComponentDigests: localComponentDigests(b.ID, parts, blockLocalComponents),
		})
		if reused {
			evaluation.ReusedBlocks = append(evaluation.ReusedBlocks, b.ID)
		} else {
			evaluation.RecomputedBlocks = append(evaluation.RecomputedBlocks, b.ID)
		}
	}
	evaluation.Composite = composite
	evaluation.Queue = scheduler.Records()
	evaluation.Cache = cache
	if opts.CacheOut != nil {
		*opts.CacheOut = *cache
	}
	topology, topoErr := CompareTopology(composite, e.Project.Truth, e.Project.Width, e.Project.Height, opts.TopologyTolerance)
	evaluation.Topology = topology
	evaluation.Attribution = AttributeDifferences(
		composite, e.Project.Truth.ReferenceMask, e.Project.CandidateMask,
		e.Project.Width, e.Project.BlockSize, e.Ordered,
		componentsInOrder(e.Ordered, componentsByStroke))
	if topoErr != nil {
		return evaluation, withDetails(CodeTopology,
			"topology verification failed: %v", map[string]any{
				"topology": evaluation.Topology, "attribution": evaluation.Attribution,
				"evaluation": evaluation,
			}, topoErr)
	}
	return evaluation, nil
}

func finalizePartial(e *Engine, eval *Evaluation, blockMasks map[int][]uint8,
	blockLocal map[int]map[string][]uint8, componentsByStroke map[string][]uint8,
	frozen map[int]bool, opts RunOptions, scheduler *Scheduler) *Evaluation {
	eval.Composite = assembleComposite(e.Project, e.Blocks, blockMasks)
	eval.Queue = scheduler.Records()
	for _, b := range e.Blocks {
		parts := participantsForBlock(e.Ordered, b.Rect)
		key := ComputeBlockCacheKey(e.Project, b.Rect, parts)
		components := make(map[string][]uint8, len(parts))
		for _, p := range parts {
			if comp := blockLocal[b.ID][p.Stroke.ID]; comp != nil {
				components[p.Stroke.ID] = append([]uint8(nil), comp...)
			}
		}
		reused := frozen[b.ID]
		eval.Blocks = append(eval.Blocks, BlockResult{
			BlockID: b.ID, Rect: b.Rect, Mask: append([]uint8(nil), blockMasks[b.ID]...),
			CacheKey: key, Reused: reused, Recomputed: !reused,
			OrderTrace:       buildBlockOrderTrace(b.ID, parts, components),
			ComponentDigests: localComponentDigests(b.ID, parts, blockLocal),
		})
		if reused {
			eval.ReusedBlocks = append(eval.ReusedBlocks, b.ID)
		} else {
			eval.RecomputedBlocks = append(eval.RecomputedBlocks, b.ID)
		}
	}
	if opts.Checkpoint != nil {
		eval.Checkpoint = opts.Checkpoint.Status()
	}
	return eval
}

func candidateBlock(p *Project, rect Rect) []uint8 {
	out := make([]uint8, rect.PixelCount())
	for y := 0; y < rect.H; y++ {
		src := (rect.Y+y)*p.Width + rect.X
		dst := y * rect.W
		copy(out[dst:dst+rect.W], p.CandidateMask[src:src+rect.W])
	}
	return out
}

func copyIntoFull(full []uint8, width int, rect Rect, component []uint8) {
	for y := 0; y < rect.H; y++ {
		dst := (rect.Y+y)*width + rect.X
		src := y * rect.W
		copy(full[dst:dst+rect.W], component[src:src+rect.W])
	}
}

func assembleComposite(p *Project, blocks []Block, blockMasks map[int][]uint8) []uint8 {
	out := make([]uint8, p.Width*p.Height)
	for _, b := range blocks {
		copyIntoFull(out, p.Width, b.Rect, blockMasks[b.ID])
	}
	return out
}

func componentsInOrder(ordered []OrderedStroke, byStroke map[string][]uint8) [][]uint8 {
	out := make([][]uint8, len(ordered))
	for i, os := range ordered {
		comp := byStroke[os.Stroke.ID]
		if comp == nil {
			comp = make([]uint8, 0)
		}
		out[i] = comp
	}
	return out
}

type PathMismatch struct {
	Kind          string `json:"kind"`
	BlockID       int    `json:"block_id,omitempty"`
	StrokeOrdinal int    `json:"stroke_ordinal,omitempty"`
	StrokeID      string `json:"stroke_id,omitempty"`
	Reason        string `json:"reason"`
}

type PathComparison struct {
	Equal         bool           `json:"equal"`
	Mismatches    []PathMismatch `json:"mismatches"`
	TopologyOK    bool           `json:"topology_ok"`
	AttributionOK bool           `json:"attribution_ok"`
}

// CompareEvaluations verifies pixel-identical block masks first, then topology
// statistics and attribution. Mismatch locations include the first differing
// block and, when available, the stroke order that contributed there.
func CompareEvaluations(a, b *Evaluation) *PathComparison {
	out := &PathComparison{Equal: true}
	if a == nil || b == nil {
		out.Equal = false
		out.Mismatches = append(out.Mismatches, PathMismatch{Kind: "evaluation", Reason: "nil evaluation"})
		return out
	}
	if len(a.Composite) != len(b.Composite) {
		out.Equal = false
		out.Mismatches = append(out.Mismatches, PathMismatch{Kind: "composite_length"})
		return out
	}
	am := make(map[int]BlockResult, len(a.Blocks))
	for _, block := range a.Blocks {
		am[block.BlockID] = block
	}
	for _, bb := range b.Blocks {
		aa, ok := am[bb.BlockID]
		if !ok {
			out.Equal = false
			out.Mismatches = append(out.Mismatches, PathMismatch{
				Kind: "block_missing", BlockID: bb.BlockID,
			})
			continue
		}
		if !masksEqual(aa.Mask, bb.Mask) {
			out.Equal = false
			m := PathMismatch{Kind: "block_mask", BlockID: bb.BlockID,
				Reason: "pixel content differs"}
			for _, strokeID := range aa.OrderTrace.Participants {
				if aa.ComponentDigests[strokeID] != bb.ComponentDigests[strokeID] {
					m.StrokeID = strokeID
					for _, os := range a.OrderedStrokes {
						if os.Stroke.ID == strokeID {
							m.StrokeOrdinal = os.Position
							break
						}
					}
					break
				}
			}
			if m.StrokeID == "" && aa.OrderTrace.WinnerID != "" {
				m.StrokeID = aa.OrderTrace.WinnerID
				m.StrokeOrdinal = aa.OrderTrace.WinnerOrder
			}
			out.Mismatches = append(out.Mismatches, m)
		}
	}
	if !masksEqual(a.Composite, b.Composite) {
		out.Equal = false
		found := false
		for i := range a.Composite {
			if a.Composite[i] != b.Composite[i] {
				_ = i
				found = true
				break
			}
		}
		if !found {
			out.Mismatches = append(out.Mismatches, PathMismatch{Kind: "composite"})
		}
	}
	out.TopologyOK = topologyStatsEqual(a.Topology, b.Topology)
	if !out.TopologyOK {
		out.Equal = false
		out.Mismatches = append(out.Mismatches, PathMismatch{Kind: "topology", Reason: "domain statistics differ"})
	}
	out.AttributionOK = attributionEqual(a.Attribution, b.Attribution)
	if !out.AttributionOK {
		out.Equal = false
		out.Mismatches = append(out.Mismatches, PathMismatch{Kind: "attribution", Reason: "first responsibility differs"})
	}
	if len(out.Mismatches) > 1 {
		sort.SliceStable(out.Mismatches, func(i, j int) bool {
			if out.Mismatches[i].Kind != out.Mismatches[j].Kind {
				return out.Mismatches[i].Kind < out.Mismatches[j].Kind
			}
			return out.Mismatches[i].BlockID < out.Mismatches[j].BlockID
		})
	}
	return out
}

func localComponentDigests(blockID int, parts []OrderedStroke, local map[int]map[string][]uint8) map[string]string {
	out := make(map[string]string, len(parts))
	for _, os := range parts {
		if comp := local[blockID][os.Stroke.ID]; comp != nil {
			out[os.Stroke.ID] = componentDigest(comp)
		}
	}
	return out
}

func topologyStatsEqual(a, b *TopologyReport) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Actual) != len(b.Actual) || len(a.Expected) != len(b.Expected) ||
		len(a.Issues) != len(b.Issues) || a.DifferencePixels != b.DifferencePixels {
		return false
	}
	for i := range a.Actual {
		if a.Actual[i] != b.Actual[i] {
			return false
		}
	}
	for i := range a.Expected {
		if a.Expected[i] != b.Expected[i] {
			return false
		}
	}
	return true
}

func attributionEqual(a, b *AttributionReport) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Differences) != len(b.Differences) || len(a.Culprits) != len(b.Culprits) {
		return false
	}
	for i := range a.Differences {
		if a.Differences[i] != b.Differences[i] {
			return false
		}
	}
	for i := range a.Culprits {
		if a.Culprits[i] != b.Culprits[i] {
			return false
		}
	}
	return true
}

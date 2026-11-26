package maskqc

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
)

var ErrSimulatedCrash = errors.New("simulated forced process termination after durable commit boundary")

type Injection struct {
	CandidateSizeMismatch bool   `json:"candidate_size_mismatch"`
	OutOfRangeStrokeID    string `json:"out_of_range_stroke_id,omitempty"`
	FailEnabled           bool   `json:"fail_enabled"`
	FailChunkID           int    `json:"fail_chunk_id,omitempty"`
	FailStrokeID          string `json:"fail_stroke_id,omitempty"`
}

type PipelineConfig struct {
	Label                      string
	Project                    *Project
	Store                      *Store
	Scheduler                  SchedulerOptions
	Injection                  Injection
	ReusePersistent            bool
	InvalidChunks              map[int]bool
	CrashAfterComponentCommits int
}

type ChunkDiagnostic struct {
	ChunkID        int        `json:"chunk_id"`
	Rect           Chunk      `json:"rect"`
	CacheKey       string     `json:"cache_key"`
	InputHash      string     `json:"input_hash"`
	StrokeIDs      []string   `json:"stroke_ids"`
	Reasons        []string   `json:"reasons"`
	CoverageState  StageState `json:"coverage_state"`
	TopologyState  StageState `json:"topology_state"`
	Reused         bool       `json:"reused"`
	MaskSHA        string     `json:"mask_sha"`
	ComponentCount int        `json:"component_count"`
}

type RunResult struct {
	Label            string                `json:"label"`
	Grid             ChunkGrid             `json:"grid"`
	Mask             *GrayMask             `json:"mask,omitempty"`
	Report           *TopologyReport       `json:"report,omitempty"`
	TopologyError    *BizError             `json:"topology_error,omitempty"`
	Fatal            *BizError             `json:"fatal,omitempty"`
	Diagnostics      []ChunkDiagnostic     `json:"diagnostics"`
	QueueEvents      []QueueEvent          `json:"queue_events"`
	Orders           map[int]OrderDecision `json:"orders"`
	Store            *Store                `json:"-"`
	Commits          []StrokeCommitRecord  `json:"commits,omitempty"`
	Sequence         []string              `json:"sequence,omitempty"`
	ReusedChunks     []int                 `json:"reused_chunks"`
	RecomputedChunks []int                 `json:"recomputed_chunks"`
}

func ConversionResidualDigest(p *Project) string {
	h := sha256.New()
	for _, s := range p.SortedStrokes() {
		_, rem, _ := MyriadToPermille(s.WeightMyriad)
		fmt.Fprintf(h, "%s:wrem=%d\n", s.ID, rem)
		so, eo, _, err := AbsoluteToOffsets(s.Start, s.End, s.Seq, s.ID, DefaultCoordinateRule)
		if err == nil {
			fmt.Fprintf(h, "%s:so=%v eo=%v\n", s.ID, so, eo)
		}
	}
	for _, tr := range p.Trace {
		if tr.Remainder != 0 {
			fmt.Fprintf(h, "trace:%s:%s:%d\n", tr.Path, tr.Field, tr.Remainder)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func candidateChunkFrom(candidate *GrayMask, p *Project, c Chunk) ([]byte, error) {
	if candidate == nil {
		return make([]byte, c.Width*c.Height), nil
	}
	if candidate.Width != p.Width || candidate.Height != p.Height {
		return nil, &BizError{
			Code: CodeSize, Message: "candidate dimensions do not match project canvas",
			Field: "candidate", ObjectID: p.ID, ChunkID: c.ID,
			Want: fmt.Sprintf("%dx%d", p.Width, p.Height),
			Got:  fmt.Sprintf("%dx%d", candidate.Width, candidate.Height),
		}
	}
	buf := make([]byte, c.Width*c.Height)
	for y := 0; y < c.Height; y++ {
		srcStart := (c.Y+y)*candidate.Width + c.X
		copy(buf[y*c.Width:(y+1)*c.Width], candidate.Pix[srcStart:srcStart+c.Width])
	}
	return buf, nil
}

func cloneProjectForInjection(p *Project, inj Injection) *Project {
	cp := *p
	cp.Strokes = append([]Stroke(nil), p.Strokes...)
	if inj.OutOfRangeStrokeID != "" {
		for i := range cp.Strokes {
			if cp.Strokes[i].ID == inj.OutOfRangeStrokeID {
				cp.Strokes[i].End.X = p.Width + 3
				if cp.Strokes[i].End.Y < p.Height {
					cp.Strokes[i].End.Y = minInt(cp.Strokes[i].End.Y+2, p.Height-1)
				}
			}
		}
	}
	if inj.CandidateSizeMismatch && p.Candidate != nil {
		cp.Candidate = NewGrayMask(maxInt(1, p.Candidate.Width-7), p.Candidate.Height)
	}
	return &cp
}

type computedChunk struct {
	cid        int
	mask       []byte
	components []StrokeComponent
	keyInputs  CacheKeyInputs
	inputHash  string
}

func RunPipeline(cfg PipelineConfig) (*RunResult, error) {
	if cfg.Project == nil {
		err := NewError(CodeSchema, "missing project")
		return &RunResult{Fatal: err}, err
	}
	p := cloneProjectForInjection(cfg.Project, cfg.Injection)
	g := NewChunkGrid(p.Width, p.Height, p.TileWidth, p.TileHeight)
	orders := PlanOrders(p, g)
	result := &RunResult{Label: cfg.Label, Grid: g, Orders: orders, Store: cfg.Store}

	candidate := p.Candidate
	if cfg.Injection.CandidateSizeMismatch {
		candidate = NewGrayMask(maxInt(1, p.Width-7), p.Height)
	}
	if candidate != nil && (candidate.Width != p.Width || candidate.Height != p.Height) {
		be := &BizError{
			Code: CodeSize, Message: "candidate dimensions do not match project canvas",
			Field: "candidate", ObjectID: p.ID, ChunkID: 0,
			Want: fmt.Sprintf("%dx%d", p.Width, p.Height),
			Got:  fmt.Sprintf("%dx%d", candidate.Width, candidate.Height),
		}
		if d := orders[0]; len(d.StrokeIDs) > 0 {
			be.StrokeID = d.StrokeIDs[0]
		}
		result.Fatal = be
		return result, be
	}
	for _, s := range p.Strokes {
		if err := ValidateStrokeParameters(s); err != nil {
			if be, ok := AsBizError(err); ok {
				result.Fatal = be
				return result, be
			}
			return result, err
		}
		if err := ValidateStrokeBounds(s, p.Width, p.Height, g); err != nil {
			if be, ok := AsBizError(err); ok {
				result.Fatal = be
				return result, be
			}
			return result, err
		}
	}

	opts := cfg.Scheduler
	if opts.Parallelism == 0 {
		opts = DefaultSchedulerOptions()
	}
	sched := NewScheduler(opts)
	residualDigest := ConversionResidualDigest(p)
	chunkMasks := map[int][]byte{}
	diagnostics := map[int]*ChunkDiagnostic{}

	var computeIDs []int
	for cid := 0; cid < g.Count; cid++ {
		c, _ := g.Chunk(cid)
		diag := &ChunkDiagnostic{ChunkID: cid, Rect: c, CoverageState: StagePending, TopologyState: StagePending}
		diagnostics[cid] = diag
		order := orders[cid]
		diag.StrokeIDs = append([]string(nil), order.StrokeIDs...)
		diag.Reasons = append([]string(nil), order.Reasons...)

		reuse := cfg.ReusePersistent && (cfg.InvalidChunks == nil || !cfg.InvalidChunks[cid])
		if reuse && cfg.Store != nil {
			if rec, ok := cfg.Store.ChunkResult(cid); ok && len(rec.Mask) == c.Width*c.Height {
				chunkMasks[cid] = append([]byte(nil), rec.Mask...)
				diag.Reused = true
				diag.CacheKey = rec.CacheKey
				diag.InputHash = rec.InputHash
				diag.MaskSHA = HashBytes(rec.Mask)
				diag.CoverageState = cfg.Store.StageState(cid, PhaseCoverage)
				diag.TopologyState = cfg.Store.StageState(cid, PhaseTopology)
				result.ReusedChunks = append(result.ReusedChunks, cid)
				continue
			}
		}
		computeIDs = append(computeIDs, cid)
		result.RecomputedChunks = append(result.RecomputedChunks, cid)
	}

	outputs := make([]computedChunk, len(computeIDs))
	tasks := make([]Task, 0, len(computeIDs))
	for taskIndex, cid := range computeIDs {
		cid := cid
		taskIndex := taskIndex
		c, _ := g.Chunk(cid)
		tasks = append(tasks, Task{
			Index: taskIndex, ChunkID: cid, PixelCount: c.Width * c.Height,
			StrokeID: firstOrEmpty(orders[cid].StrokeIDs),
			Run: func() error {
				if err := beginStage(cfg.Store, cid, PhaseCoverage, "scheduler started chunk"); err != nil {
					return err
				}
				out, err := computeChunk(p, g, cid, candidate, orders[cid].StrokeIDs, residualDigest, cfg.Injection)
				if err != nil {
					if cfg.Store != nil {
						_, _ = cfg.Store.TransitionStage(cid, PhaseCoverage, StageFailed, err.Error())
					}
					return err
				}
				outputs[taskIndex] = out
				return nil
			},
		})
	}
	errs := sched.Run(tasks)
	result.QueueEvents = sched.Events()
	for _, err := range errs {
		if err != nil {
			if be, ok := AsBizError(err); ok {
				result.Fatal = be
			} else {
				result.Fatal = NewError(CodeBlock, err.Error())
			}
			result.Diagnostics = sortedDiagnostics(diagnostics)
			return result, result.Fatal
		}
	}
	computed := map[int]computedChunk{}
	for taskIndex, cid := range computeIDs {
		if outputs[taskIndex].mask != nil {
			computed[cid] = outputs[taskIndex]
		}
	}

	// Stable durable commit order: chunk ID ascending, then composition order.
	commitCount := 0
	for _, cid := range sortedComputedIDs(computed) {
		out := computed[cid]
		c, _ := g.Chunk(cid)
		for _, comp := range out.components {
			rec := StrokeCommitRecord{
				StrokeID: comp.StrokeID, ChunkID: cid, Coverage: comp.Coverage,
				CacheKey: comp.CacheKey, ContentHash: comp.ContentHash,
				ResidualDigest: residualDigest, OrderVersion: OrderVersion,
				AlgorithmVersion: AlgorithmVersion,
			}
			if cfg.Store != nil {
				if existing, ok := cfg.Store.StrokeComponent(rec.StrokeID, rec.ChunkID); ok && existing.CacheKey == rec.CacheKey {
					result.Commits = append(result.Commits, existing)
					continue
				}
				committed, err := cfg.Store.CommitStrokeComponent(rec)
				if err != nil {
					return result, err
				}
				rec = committed
			}
			result.Commits = append(result.Commits, rec)
			commitCount++
			if cfg.CrashAfterComponentCommits > 0 && commitCount >= cfg.CrashAfterComponentCommits {
				result.Diagnostics = sortedDiagnostics(diagnostics)
				return result, ErrSimulatedCrash
			}
		}
		chunkKey := out.keyInputs.FinalCacheKey
		if cfg.Store != nil {
			if _, err := cfg.Store.CommitChunkResult(ChunkResultRecord{
				ChunkID: cid, Mask: out.mask, CacheKey: chunkKey, InputHash: out.inputHash,
			}); err != nil {
				return result, err
			}
			if _, err := cfg.Store.TransitionStage(cid, PhaseCoverage, StageCommitted, "coverage alpha accumulation durable"); err != nil {
				return result, err
			}
		}
		chunkMasks[cid] = out.mask
		diag := diagnostics[cid]
		diag.CacheKey = chunkKey
		diag.InputHash = out.inputHash
		diag.MaskSHA = HashBytes(out.mask)
		diag.ComponentCount = len(out.components)
		diag.CoverageState = StageCommitted
		if cfg.Store != nil {
			diag.CoverageState = cfg.Store.StageState(cid, PhaseCoverage)
		}
		_ = c
	}

	mask := AssembleMask(g, chunkMasks)
	result.Mask = mask

	// Global topology/direction verification is downstream of every chunk.
	for cid := 0; cid < g.Count; cid++ {
		if err := beginStage(cfg.Store, cid, PhaseTopology, "analyze assembled mask"); err != nil {
			return result, err
		}
	}
	report, topoErr := VerifyTopology(mask, p.Truth, p.Strokes, DefaultTolerance())
	result.Report = report
	result.TopologyError = topoErr
	topologyState := StageCommitted
	if topoErr != nil {
		topologyState = StageFailed
	}
	for cid := 0; cid < g.Count; cid++ {
		if cfg.Store != nil {
			current := cfg.Store.StageState(cid, PhaseTopology)
			if current == StageProcessing {
				_, _ = cfg.Store.TransitionStage(cid, PhaseTopology, topologyState, topologyReason(topoErr))
			}
		}
		diag := diagnostics[cid]
		diag.TopologyState = topologyState
		if cfg.Store != nil {
			diag.TopologyState = cfg.Store.StageState(cid, PhaseTopology)
		}
		if diag.MaskSHA == "" && chunkMasks[cid] != nil {
			diag.MaskSHA = HashBytes(chunkMasks[cid])
		}
	}

	if cfg.Store != nil {
		result.Sequence = CanonicalLogicalSequence(cfg.Store.StrokeCommits())
	} else {
		result.Sequence = CanonicalLogicalSequence(result.Commits)
	}
	result.Diagnostics = sortedDiagnostics(diagnostics)
	return result, nil
}

func beginStage(store *Store, cid int, phase, reason string) error {
	if store == nil {
		return nil
	}
	_, err := store.TransitionStage(cid, phase, StageProcessing, reason)
	if err == nil {
		return nil
	}
	if IsCode(err, CodeState) && store.StageState(cid, phase) == StageProcessing {
		// A forced kill can leave an in-progress stage; the durable prefix is
		// valid and the same generation may resume.
		return nil
	}
	return err
}

func topologyReason(err *BizError) string {
	if err == nil {
		return "topology and direction within tolerance"
	}
	return err.Error()
}

func firstOrEmpty(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

func sortedComputedIDs(m map[int]computedChunk) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func sortedDiagnostics(m map[int]*ChunkDiagnostic) []ChunkDiagnostic {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]ChunkDiagnostic, 0, len(ids))
	for _, id := range ids {
		out = append(out, *m[id])
	}
	return out
}

func computeChunk(p *Project, g ChunkGrid, cid int, candidate *GrayMask, orderIDs []string, residualDigest string, inj Injection) (computedChunk, error) {
	c, ok := g.Chunk(cid)
	if !ok {
		return computedChunk{}, NewError(CodeBlock, "invalid chunk id")
	}
	input, err := candidateChunkFrom(candidate, p, c)
	if err != nil {
		return computedChunk{}, err
	}
	inputHash := HashBytes(input)
	byID := map[string]Stroke{}
	for _, s := range p.Strokes {
		byID[s.ID] = s
	}
	components := make([]StrokeComponent, 0, len(orderIDs))
	for _, sid := range orderIDs {
		s := byID[sid]
		if inj.FailEnabled && inj.FailChunkID == cid && (inj.FailStrokeID == "" || inj.FailStrokeID == sid) {
			return computedChunk{}, &BizError{
				Code: CodeBlock, Message: "injected internal chunk failure",
				ChunkID: cid, StrokeID: sid, ObjectID: sid,
				Cause: "Injection.FailChunkID",
			}
		}
		cov, err := RasterStrokeChunk(s, c)
		if err != nil {
			if be, ok := AsBizError(err); ok {
				be.ChunkID = cid
				return computedChunk{}, be
			}
			return computedChunk{}, err
		}
		contentHash := ComponentContentHash(s, c)
		cacheKey := ComponentCacheKey(s, c, inputHash, "")
		components = append(components, StrokeComponent{
			StrokeID: sid, ChunkID: cid, Coverage: cov,
			CoverageSHA: HashBytes(cov), ContentHash: contentHash, CacheKey: cacheKey,
		})
	}
	mask, err := CompositeChunk(input, components, orderIDs, c.Width, c.Height)
	if err != nil {
		if be, ok := AsBizError(err); ok {
			be.ChunkID = cid
			if len(orderIDs) > 0 {
				be.StrokeID = orderIDs[0]
			}
			return computedChunk{}, be
		}
		return computedChunk{}, err
	}
	keyInputs := BuildChunkCacheKey(cid, inputHash, components, orderIDs)
	_ = residualDigest
	return computedChunk{cid: cid, mask: mask, components: components, keyInputs: keyInputs, inputHash: inputHash}, nil
}

// CompareResults locates the first chunk-level pixel or field mismatch between
// two independent path results.
func CompareResults(a, b *RunResult) *Mismatch {
	if a == nil || b == nil {
		return &Mismatch{Domain: -1, Item: "result", Want: "non-nil", Got: "nil"}
	}
	if a.Grid.Count != b.Grid.Count || a.Grid.Width != b.Grid.Width || a.Grid.Height != b.Grid.Height {
		return &Mismatch{Domain: -1, Item: "grid", Want: fmt.Sprintf("%+v", a.Grid), Got: fmt.Sprintf("%+v", b.Grid)}
	}
	diagA := map[int]ChunkDiagnostic{}
	diagB := map[int]ChunkDiagnostic{}
	for _, d := range a.Diagnostics {
		diagA[d.ChunkID] = d
	}
	for _, d := range b.Diagnostics {
		diagB[d.ChunkID] = d
	}
	for cid := 0; cid < a.Grid.Count; cid++ {
		ca, oka := diagA[cid]
		cb, okb := diagB[cid]
		if !oka || !okb {
			return &Mismatch{Domain: cid, Item: "diagnostic_present", Want: fmt.Sprint(oka), Got: fmt.Sprint(okb)}
		}
		if ca.MaskSHA != cb.MaskSHA {
			// Pixel-level localization within the chunk.
			if pa, pb := chunkPixels(a.Mask, ca.Rect), chunkPixels(b.Mask, cb.Rect); pa != nil && pb != nil {
				for i := 0; i < len(pa) && i < len(pb); i++ {
					if pa[i] != pb[i] {
						lx := i % ca.Rect.Width
						ly := i / ca.Rect.Width
						return &Mismatch{
							Domain: cid, Item: fmt.Sprintf("mask.pixel[%d,%d]", ca.Rect.X+lx, ca.Rect.Y+ly),
							Want: fmt.Sprint(pa[i]), Got: fmt.Sprint(pb[i]),
						}
					}
				}
			}
			return &Mismatch{Domain: cid, Item: "mask_sha256", Want: ca.MaskSHA, Got: cb.MaskSHA}
		}
		if ca.CacheKey != cb.CacheKey {
			return &Mismatch{Domain: cid, Item: "cache_key", Want: ca.CacheKey, Got: cb.CacheKey}
		}
		if stringsEqual(ca.StrokeIDs, cb.StrokeIDs) == false {
			return &Mismatch{Domain: cid, Item: "stroke_ids", Want: fmt.Sprint(ca.StrokeIDs), Got: fmt.Sprint(cb.StrokeIDs)}
		}
	}
	if !stringsEqual(a.Sequence, b.Sequence) {
		for i := 0; i < maxInt(len(a.Sequence), len(b.Sequence)); i++ {
			var av, bv string
			if i < len(a.Sequence) {
				av = a.Sequence[i]
			}
			if i < len(b.Sequence) {
				bv = b.Sequence[i]
			}
			if av != bv {
				return &Mismatch{Domain: -1, Item: fmt.Sprintf("commit_sequence[%d]", i), Want: av, Got: bv}
			}
		}
	}
	if (a.Report == nil) != (b.Report == nil) {
		return &Mismatch{Domain: -1, Item: "topology_report", Want: "present", Got: "absent"}
	}
	if a.Report != nil && len(a.Report.Domains) != len(b.Report.Domains) {
		return &Mismatch{Domain: -1, Item: "domain_count", Want: fmt.Sprint(len(a.Report.Domains)), Got: fmt.Sprint(len(b.Report.Domains))}
	}
	if a.Report != nil {
		for i := 0; i < minInt(len(a.Report.Domains), len(b.Report.Domains)); i++ {
			da, db := a.Report.Domains[i], b.Report.Domains[i]
			if stableJSONHash(da) != stableJSONHash(db) {
				return &Mismatch{Domain: da.DomainID, Item: "domain_stats", Want: fmt.Sprintf("%+v", da), Got: fmt.Sprintf("%+v", db)}
			}
		}
	}
	return nil
}

func chunkPixels(m *GrayMask, c Chunk) []byte {
	if m == nil {
		return nil
	}
	out := make([]byte, 0, c.Width*c.Height)
	for y := 0; y < c.Height; y++ {
		start := (c.Y+y)*m.Width + c.X
		out = append(out, m.Pix[start:start+c.Width]...)
	}
	return out
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

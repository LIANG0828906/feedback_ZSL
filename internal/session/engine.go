package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"qkernel/internal/attribution"
	"qkernel/internal/chain"
	"qkernel/internal/errs"
	"qkernel/internal/imagex"
	"qkernel/internal/metrics"
	"qkernel/internal/sched"
	"qkernel/internal/synth"
)

var ErrInterrupted = errors.New("simulated generation interruption")

type Engine struct {
	Root  string
	Store *Store
}

func NewEngine(root string) (*Engine, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, errs.Wrap(errs.EIO, err, "create output root")
	}
	store, err := OpenStore(root)
	if err != nil {
		return nil, err
	}
	return &Engine{Root: root, Store: store}, nil
}

func (e *Engine) Close() error { return e.Store.Close() }

type Segment struct {
	Index int         `json:"index"`
	Rect  imagex.Rect `json:"rect"`
}

func TileSegments(bounds imagex.Rect, size int) []Segment {
	if size <= 0 {
		size = 64
	}
	var out []Segment
	idx := 0
	for y := bounds.MinY; y < bounds.MaxY; y += size {
		for x := bounds.MinX; x < bounds.MaxX; x += size {
			r := imagex.R(x, y, minInt(x+size, bounds.MaxX), minInt(y+size, bounds.MaxY))
			out = append(out, Segment{Index: idx, Rect: r})
			idx++
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type RunOptions struct {
	Benchmark             *synth.Benchmark
	Chain                 chain.Chain
	ParamVersion          int
	SegmentSize           int
	MetricBlockSize       int
	Workers               int
	BufferBytesPerWorker  int
	Seed                  int64
	RunID                 string
	BaseRunID             string
	InterruptAfterCommits int
	FailureHook           func(stepNo, segmentIndex int) error
}

type ReuseRecord struct {
	StepIndex            int    `json:"step_index"`
	StepType             string `json:"step_type"`
	Status               string `json:"status"`
	Reason               string `json:"reason"`
	TotalUnits           int    `json:"total_units"`
	RecoveredUnits       int    `json:"recovered_units"`
	CacheHitUnits        int    `json:"cache_hit_units"`
	DependencyReuseUnits int    `json:"dependency_reuse_units"`
	DomainReuseUnits     int    `json:"domain_reuse_units"`
	RecomputedUnits      int    `json:"recomputed_units"`
	PropagatedUnits      int    `json:"propagated_units"`
}

type RunResult struct {
	RunID           string              `json:"run_id"`
	ParamVersion    int                 `json:"param_version"`
	Segments        []Segment           `json:"segments"`
	Final           *imagex.Frame       `json:"-"`
	StepFrames      []*imagex.Frame     `json:"-"`
	MetricStates    []*metrics.Report   `json:"metric_states,omitempty"`
	Metrics         *metrics.Report     `json:"metrics,omitempty"`
	Attribution     *attribution.Report `json:"attribution,omitempty"`
	Reuse           []ReuseRecord       `json:"reuse"`
	Quality         QualityInvalidation `json:"quality"`
	QueueEvents     []sched.QueueEvent  `json:"queue_events"`
	Commits         []CommitRecord      `json:"commits"`
	MetricsPath     string              `json:"metrics_path,omitempty"`
	AttributionPath string              `json:"attribution_path,omitempty"`
	FinalPNGPath    string              `json:"final_png_path,omitempty"`
	FinalRAWPath    string              `json:"final_raw_path,omitempty"`
}

type BlockCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type QualityInvalidation struct {
	Status         string       `json:"status"`
	Reason         string       `json:"reason"`
	BlockSize      int          `json:"block_size"`
	AffectedBlocks []BlockCoord `json:"affected_blocks"`
}

func DefaultRunID(b *synth.Benchmark, c chain.Chain, paramVersion, segmentSize, metricBlock int) string {
	material := struct {
		TruthFingerprint string
		ChainFingerprint string
		ParamVersion     int
		SegmentSize      int
		MetricBlock      int
	}{b.Truth.Fingerprint, c.Fingerprint(), paramVersion, segmentSize, metricBlock}
	raw, _ := json.Marshal(material)
	sum := sha256.Sum256(raw)
	return "run_" + hex.EncodeToString(sum[:16])
}

type tileSummary struct {
	Hash          string     `json:"hash"`
	Bytes         int        `json:"bytes"`
	MeanRGB       [3]float64 `json:"mean_rgb"`
	MeanLuma      float64    `json:"mean_luma"`
	ChangedPixels int        `json:"changed_pixels"`
}

type cacheKeyMaterial struct {
	AlgoVersion  string      `json:"algo_version"`
	FrameID      string      `json:"frame_id"`
	StepIndex    int         `json:"step_index"`
	Segment      imagex.Rect `json:"segment"`
	Step         chain.Step  `json:"step"`
	InputHash    string      `json:"input_hash"`
	UpstreamKeys []string    `json:"upstream_keys"`
}

func computeCacheKey(m cacheKeyMaterial) string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) RunFrame(opts RunOptions) (*RunResult, error) {
	if opts.Benchmark == nil || opts.Benchmark.Frame == nil || opts.Benchmark.Truth == nil {
		return nil, errs.New(errs.ESchema, "benchmark with frame and truth is required")
	}
	b := opts.Benchmark
	f := b.Frame
	if err := chain.Validate(opts.Chain, f.Width, f.Height); err != nil {
		return nil, err
	}
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = 64
	}
	if opts.MetricBlockSize <= 0 {
		opts.MetricBlockSize = 32
	}
	if f.Width%opts.MetricBlockSize != 0 || f.Height%opts.MetricBlockSize != 0 {
		return nil, &errs.Error{Code: errs.EPara, Message: "metric block size must divide frame", Field: "metric_block_size", StepIndex: -1, BlockX: -1, BlockY: -1}
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Seed == 0 {
		opts.Seed = b.Truth.Seed
	}
	if opts.ParamVersion <= 0 {
		opts.ParamVersion = 1
	}
	if opts.RunID == "" {
		opts.RunID = DefaultRunID(b, opts.Chain, opts.ParamVersion, opts.SegmentSize, opts.MetricBlockSize)
	}

	chainJSON, _ := json.Marshal(opts.Chain)
	chainFP := opts.Chain.Fingerprint()
	if err := e.Store.RegisterParamVersion(opts.ParamVersion, string(chainJSON), chainFP); err != nil {
		return nil, err
	}
	runRec := RunRecord{
		RunID: opts.RunID, FrameID: b.Truth.Frame.ID, ParamVersion: opts.ParamVersion,
		SegmentSize: opts.SegmentSize, MetricBlockSize: opts.MetricBlockSize, Workers: opts.Workers,
		ChainFingerprint: chainFP, AlgoVersion: chain.AlgoVersion, Root: e.Root,
	}
	if err := e.Store.EnsureRun(runRec); err != nil {
		return nil, err
	}
	segments := TileSegments(f.Bounds(), opts.SegmentSize)
	if err := e.Store.EnsurePendingStates(opts.RunID, len(opts.Chain.Steps), len(segments)); err != nil {
		return nil, err
	}
	if _, err := e.Store.RecoverProcessing(opts.RunID, "recover interrupted processing units"); err != nil {
		return nil, err
	}
	committed, err := e.Store.CommittedKeySet(opts.RunID)
	if err != nil {
		return nil, err
	}

	priorCommits, oldChain := e.loadPriorChain(opts, b.Truth.Frame.ID)
	scheduler := sched.New(opts.Workers, opts.BufferBytesPerWorker)
	result := &RunResult{RunID: opts.RunID, ParamVersion: opts.ParamVersion, Segments: segments}

	frames := make([]*imagex.Frame, len(opts.Chain.Steps)+1)
	frames[0] = f
	stepKeys := make([][]string, len(opts.Chain.Steps)+1)
	stepKeys[0] = []string{"base:" + b.Truth.Fingerprint}
	commitWrites := 0
	dirtyOutputSegments := map[int]bool{}

	for i, step := range opts.Chain.Steps {
		stepNo := i + 1
		prev := frames[i]
		if prev == nil {
			return nil, errs.New(errs.EState, "previous step frame %d is unavailable", i)
		}
		out := imagex.NewFrame(f.Width, f.Height, f.Channels)
		keys := make([]string, len(segments))
		inputHashes := make([]string, len(segments))
		tiles := make([][]byte, len(segments))
		kinds := make([]string, len(segments))
		activeUnits := make([]bool, len(segments))
		priorRecords := make([]*CommitRecord, len(segments))
		cacheEntries := make([]*CacheEntry, len(segments))
		upstreamKeysBySeg := make([][]string, len(segments))
		workUnits := make([]sched.Unit, 0, len(segments))
		workSegs := make([]int, 0, len(segments))
		reuse := ReuseRecord{StepIndex: stepNo, StepType: string(step.Type), TotalUnits: len(segments)}
		effectRegion, regionResolveErr := step.Region.Resolve(f.Bounds())
		if regionResolveErr != nil {
			return nil, regionResolveErr
		}

		for si, seg := range segments {
			order := (stepNo-1)*len(segments) + si
			_ = order
			if rec, ok := committed[[2]int{stepNo, si}]; ok {
				tile, err := e.readVerified(rec.OutputPath, rec.OutputHash)
				if err != nil {
					return nil, err
				}
				tiles[si] = tile
				keys[si] = rec.CacheKey
				kinds[si] = "committed"
				reuse.RecoveredUnits++
				continue
			}

			margin := chain.DomainMargin(step)
			segmentActive := effectRegion.Intersects(seg.Rect)
			activeUnits[si] = segmentActive
			if !segmentActive {
				margin = 0
			}
			domain := seg.Rect.Expand(margin, prev.Bounds())
			inputHash := prev.RectHash(domain)
			inputHashes[si] = inputHash

			upstreamKeys := []string{}
			if i == 0 {
				upstreamKeys = append(upstreamKeys, stepKeys[0][0])
			} else {
				depDomain := domain
				for _, upSeg := range segments {
					if upSeg.Rect.Intersects(depDomain) && stepKeys[i] != nil && si < len(stepKeys[i]) {
						if key := stepKeys[i][upSeg.Index]; key != "" {
							upstreamKeys = append(upstreamKeys, key)
						}
					}
				}
				if len(upstreamKeys) == 0 && si < len(stepKeys[i]) {
					upstreamKeys = append(upstreamKeys, stepKeys[i][si])
				}
			}
			upstreamKeysBySeg[si] = upstreamKeys

			if oldChain != nil && priorCommits != nil {
				if oldRec, ok := priorCommits[[2]int{stepNo, si}]; ok {
					if e.canReuseDependency(step, i, seg.Index, seg.Rect, oldChain, upstreamKeys, priorCommits, b.Truth.Fingerprint) {
						tile, err := e.readVerified(oldRec.OutputPath, oldRec.OutputHash)
						if err == nil {
							tiles[si] = tile
							keys[si] = oldRec.CacheKey
							kinds[si] = "prior_reuse"
							priorRecords[si] = &oldRec
							reuse.DependencyReuseUnits++
							continue
						}
					}
				}
			}

			key := computeCacheKey(cacheKeyMaterial{
				AlgoVersion: chain.AlgoVersion, FrameID: b.Truth.Frame.ID, StepIndex: stepNo,
				Segment: seg.Rect, Step: step, InputHash: inputHash, UpstreamKeys: upstreamKeys,
			})
			keys[si] = key
			if oldRec, ok := priorCommits[[2]int{stepNo, si}]; ok && oldChain != nil && canReuseByDomain(step, i, *oldChain, oldRec, inputHash) {
				tile, readErr := e.readVerified(oldRec.OutputPath, oldRec.OutputHash)
				if readErr == nil {
					tiles[si] = tile
					kinds[si] = "domain_reuse"
					priorRecords[si] = &oldRec
					reuse.DomainReuseUnits++
					continue
				}
			}
			entry, err := e.Store.GetCache(key)
			if err != nil {
				return nil, err
			}
			if entry != nil {
				tile, err := e.readVerified(entry.OutputPath, entry.OutputHash)
				if err == nil {
					tiles[si] = tile
					kinds[si] = "cache_hit"
					cacheEntries[si] = entry
					reuse.CacheHitUnits++
					continue
				}
			}

			kinds[si] = "miss"
			segIndex := si
			segRect := seg.Rect
			weight := seg.Rect.Dx() * seg.Rect.Dy() * f.Channels * 2
			workSegs = append(workSegs, si)
			workUnits = append(workUnits, sched.Unit{
				ID:           opts.RunID + ":s" + itoa(stepNo) + ":seg" + itoa(si),
				BufferWeight: weight,
				Work: func() error {
					if opts.FailureHook != nil {
						if err := opts.FailureHook(stepNo, segIndex); err != nil {
							return err
						}
					}
					tiles[segIndex] = chain.RenderTile(prev, step, i, opts.Seed, segRect)
					return nil
				},
			})
		}

		for si := range segments {
			if kinds[si] == "committed" {
				continue
			}
			if err := e.Store.TransitionState(opts.RunID, si, stepNo, StateProcessing, "scheduled unit"); err != nil {
				return nil, err
			}
		}

		results := scheduler.Run(workUnits)
		errBySeg := map[int]error{}
		for j, res := range results {
			if res.Err != nil {
				errBySeg[workSegs[j]] = res.Err
			}
		}

		var firstErr error
		for si, seg := range segments {
			if kinds[si] == "committed" {
				out.CopyRectFrom(seg.Rect, tiles[si])
				continue
			}
			if err, failed := errBySeg[si]; failed {
				_ = e.Store.TransitionState(opts.RunID, si, stepNo, StateFailed, err.Error())
				if firstErr == nil {
					firstErr = err
				}
				break
			}

			var tile []byte
			var cacheKey, outputPath, outputHash, inputHash string
			var summary tileSummary

			switch kinds[si] {
			case "prior_reuse":
				old := priorRecords[si]
				tile = tiles[si]
				cacheKey = old.CacheKey
				outputPath = old.OutputPath
				outputHash = old.OutputHash
				inputHash = old.InputHash
				summaryJSON := json.RawMessage(old.Summary)
				_ = json.Unmarshal(summaryJSON, &summary)
			case "cache_hit":
				entry := cacheEntries[si]
				tile = tiles[si]
				cacheKey = entry.CacheKey
				outputPath = entry.OutputPath
				outputHash = entry.OutputHash
				inputHash = entry.InputHash
				_ = json.Unmarshal([]byte(entry.SummaryJSON), &summary)
			case "domain_reuse":
				old := priorRecords[si]
				tile = tiles[si]
				cacheKey = keys[si]
				outputPath = old.OutputPath
				outputHash = old.OutputHash
				inputHash = inputHashes[si]
				_ = json.Unmarshal([]byte(old.Summary), &summary)
			default:
				tile = tiles[si]
				if tile == nil {
					firstErr = errs.New(errs.EBuild, "unit produced no tile")
					break
				}
				cacheKey = keys[si]
				outputHash = imagex.HashBytes(tile)
				outputPath = filepath.ToSlash(filepath.Join("objects", cacheKey+".bin"))
				abs := e.resolve(outputPath)
				if err := writeFileAtomic(abs, tile); err != nil {
					return nil, err
				}
				inputHash = inputHashes[si]
				summary = buildSummary(tile, outputHash, prev, seg.Rect)
				if activeUnits[si] {
					reuse.RecomputedUnits++
				} else {
					reuse.PropagatedUnits++
				}
			}

			summaryBytes, _ := json.Marshal(summary)
			order := (stepNo-1)*len(segments) + si
			if kinds[si] == "miss" {
				old, hadOld := priorCommits[[2]int{stepNo, si}]
				if !hadOld || old.OutputHash != outputHash {
					dirtyOutputSegments[si] = true
				}
			}
			rec := CommitRecord{
				RunID: opts.RunID, CommitOrder: order, FrameID: b.Truth.Frame.ID,
				SegmentIndex: si, StepIndex: stepNo, CacheKey: cacheKey, InputHash: inputHash,
				OutputHash: outputHash, OutputPath: outputPath, Summary: summaryBytes,
				Region: effectRegion, UpstreamKeys: upstreamKeysBySeg[si],
				ChainFingerprint: chainFP, ParamVersion: opts.ParamVersion, AlgoVersion: chain.AlgoVersion,
			}
			if err := e.Store.CommitUnit(rec); err != nil {
				return nil, err
			}
			committed[[2]int{stepNo, si}] = rec
			commitWrites++
			keys[si] = cacheKey
			out.CopyRectFrom(seg.Rect, tile)

			if opts.InterruptAfterCommits > 0 && commitWrites >= opts.InterruptAfterCommits {
				result.QueueEvents = scheduler.Events()
				result.Reuse = append(result.Reuse, reuse)
				result.Commits, _ = e.Store.ListCommits(opts.RunID)
				return result, ErrInterrupted
			}
		}
		if firstErr != nil {
			result.QueueEvents = scheduler.Events()
			result.Reuse = append(result.Reuse, reuse)
			result.Commits, _ = e.Store.ListCommits(opts.RunID)
			return result, firstErr
		}

		status, reason := classifyReuse(reuse)
		reuse.Status = status
		reuse.Reason = reason
		result.Reuse = append(result.Reuse, reuse)
		stepKeys[i+1] = keys

		stepPNG := e.resolve(filepath.ToSlash(filepath.Join("images", b.Truth.Frame.ID, fmtStepName(stepNo, string(step.Type))+".png")))
		if err := imagex.SavePNG(out, stepPNG); err != nil {
			return nil, err
		}
		frames[i+1] = out
	}

	final := frames[len(opts.Chain.Steps)]
	result.Final = final
	result.StepFrames = frames[1:]
	result.QueueEvents = scheduler.Events()

	finalPNG := e.resolve(filepath.ToSlash(filepath.Join("images", b.Truth.Frame.ID, "final.png")))
	if err := imagex.SavePNG(final, finalPNG); err != nil {
		return nil, err
	}
	finalRAW := e.resolve(filepath.ToSlash(filepath.Join("images", b.Truth.Frame.ID, "final.rgb")))
	if err := writeFileAtomic(finalRAW, final.Pix); err != nil {
		return nil, err
	}
	result.FinalPNGPath = finalPNG
	result.FinalRAWPath = finalRAW

	baseMetrics, err := metrics.Compute(f, opts.MetricBlockSize)
	if err != nil {
		return nil, err
	}
	result.MetricStates = []*metrics.Report{baseMetrics}
	for _, sf := range frames[1:] {
		mr, err := metrics.Compute(sf, opts.MetricBlockSize)
		if err != nil {
			return nil, err
		}
		result.MetricStates = append(result.MetricStates, mr)
	}
	compared, err := metrics.Compare(baseMetrics, result.MetricStates[len(result.MetricStates)-1])
	if err != nil {
		return nil, err
	}
	result.Metrics = compared
	metricsPath := e.resolve(filepath.ToSlash(filepath.Join("metrics", b.Truth.Frame.ID+".json")))
	if err := writeJSONAtomic(metricsPath, compared); err != nil {
		return nil, err
	}
	result.MetricsPath = metricsPath
	result.Quality = buildQualityInvalidation(segments, dirtyOutputSegments, opts.MetricBlockSize)

	stepTypes := make([]string, len(opts.Chain.Steps))
	for i, st := range opts.Chain.Steps {
		stepTypes[i] = string(st.Type)
	}
	attrReport, attrErr := attribution.BuildReport(result.MetricStates, stepTypes, attribution.DefaultDeclaredShare)
	attrPath := e.resolve(filepath.ToSlash(filepath.Join("attribution", b.Truth.Frame.ID+".json")))
	if err := writeJSONAtomic(attrPath, attrReport); err != nil {
		return nil, err
	}
	result.Attribution = attrReport
	result.AttributionPath = attrPath
	if attrErr == nil {
		attrErr = attribution.CheckTruth(attrReport, b.Truth)
	}

	reusePath := e.resolve(filepath.ToSlash(filepath.Join("reuse", b.Truth.Frame.ID+".json")))
	_ = writeJSONAtomic(reusePath, result.Reuse)
	invalidationPath := e.resolve(filepath.ToSlash(filepath.Join("invalidation", b.Truth.Frame.ID+".json")))
	_ = writeJSONAtomic(invalidationPath, map[string]any{"steps": result.Reuse, "quality": result.Quality})
	queuePath := e.resolve(filepath.ToSlash(filepath.Join("queue", b.Truth.Frame.ID+".json")))
	_ = writeJSONAtomic(queuePath, result.QueueEvents)

	result.Commits, err = e.Store.ListCommits(opts.RunID)
	if err != nil {
		return nil, err
	}
	if attrErr != nil {
		return result, attrErr
	}
	return result, nil
}

func (e *Engine) loadPriorChain(opts RunOptions, frameID string) (map[[2]int]CommitRecord, *chain.Chain) {
	var prior *RunRecord
	if opts.BaseRunID != "" {
		r, err := e.Store.GetRun(opts.BaseRunID)
		if err == nil {
			prior = r
		}
	} else {
		r, err := e.Store.LatestCommittedRun(frameID, opts.RunID)
		if err == nil {
			prior = r
		}
	}
	if prior == nil || prior.SegmentSize != opts.SegmentSize {
		return nil, nil
	}
	commits, err := e.Store.CommittedKeySet(prior.RunID)
	if err != nil || len(commits) == 0 {
		return nil, nil
	}
	chainJSON, _, err := e.Store.ChainForVersion(prior.ParamVersion)
	if err != nil {
		return commits, nil
	}
	var old chain.Chain
	if err := json.Unmarshal([]byte(chainJSON), &old); err != nil {
		return commits, nil
	}
	return commits, &old
}

func (e *Engine) canReuseDependency(step chain.Step, i int, segIndex int, seg imagex.Rect, oldChain *chain.Chain, upstreamKeys []string, priorCommits map[[2]int]CommitRecord, truthFP string) bool {
	if oldChain == nil || i >= len(oldChain.Steps) {
		return false
	}
	oldStep := oldChain.Steps[i]
	if oldStep.Type != step.Type {
		return false
	}
	bounds := imagex.R(0, 0, 1<<30, 1<<30)
	newRegion, err := step.Region.Resolve(bounds)
	if err != nil || newRegion.Intersects(seg) {
		return false
	}
	oldRegion, err := oldStep.Region.Resolve(bounds)
	if err != nil || oldRegion.Intersects(seg) {
		return false
	}
	stepNo := i + 1
	if i == 0 {
		return len(upstreamKeys) == 1 && upstreamKeys[0] == "base:"+truthFP
	}
	prevOld, ok := priorCommits[[2]int{stepNo - 1, segIndex}]
	if !ok || len(upstreamKeys) == 0 {
		return false
	}
	for _, key := range upstreamKeys {
		if key != prevOld.CacheKey {
			return false
		}
	}
	return true
}

func canReuseByDomain(step chain.Step, index int, oldChain chain.Chain, oldRec CommitRecord, inputHash string) bool {
	if index >= len(oldChain.Steps) || oldRec.AlgoVersion != chain.AlgoVersion {
		return false
	}
	oldBytes, err := json.Marshal(oldChain.Steps[index])
	if err != nil {
		return false
	}
	newBytes, err := json.Marshal(step)
	if err != nil {
		return false
	}
	return string(oldBytes) == string(newBytes) && oldRec.InputHash == inputHash
}

func classifyReuse(r ReuseRecord) (string, string) {
	switch {
	case r.RecomputedUnits == 0 && (r.CacheHitUnits > 0 || r.DependencyReuseUnits > 0 || r.DomainReuseUnits > 0 || r.RecoveredUnits > 0):
		if r.PropagatedUnits > 0 {
			return "reused", "operator effect domains matched; changed pass-through segments were propagated without operator recomputation"
		}
		return "reused", "all effective input-domain hashes and upstream keys matched"
	case r.RecomputedUnits == r.TotalUnits:
		return "recomputed", "all unit input domains were invalidated"
	default:
		return "partial", "only spatially affected input domains were invalidated"
	}
}

func buildQualityInvalidation(segments []Segment, dirty map[int]bool, blockSize int) QualityInvalidation {
	q := QualityInvalidation{BlockSize: blockSize}
	if len(dirty) == 0 {
		q.Status = "reused"
		q.Reason = "no content-addressed segment output changed; metric and attribution cache remains valid"
		return q
	}
	q.Status = "invalidated"
	q.Reason = "one or more segment output hashes changed; overlapping block metrics and downstream attribution are invalidated"
	seen := map[BlockCoord]bool{}
	for segIndex := range dirty {
		if segIndex < 0 || segIndex >= len(segments) || blockSize <= 0 {
			continue
		}
		r := segments[segIndex].Rect
		x0 := r.MinX / blockSize
		y0 := r.MinY / blockSize
		x1 := (r.MaxX - 1) / blockSize
		y1 := (r.MaxY - 1) / blockSize
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				c := BlockCoord{X: x, Y: y}
				if !seen[c] {
					seen[c] = true
					q.AffectedBlocks = append(q.AffectedBlocks, c)
				}
			}
		}
	}
	return q
}

func buildSummary(tile []byte, outputHash string, prev *imagex.Frame, seg imagex.Rect) tileSummary {
	s := tileSummary{Hash: outputHash, Bytes: len(tile)}
	var sum [3]int
	var luma float64
	ch := prev.Channels
	pixelCount := seg.Dx() * seg.Dy()
	for p := 0; p < pixelCount; p++ {
		for c := 0; c < 3; c++ {
			v := int(tile[p*ch+c])
			sum[c] += v
			if c == 0 {
				luma += 0.299 * float64(v)
			} else if c == 1 {
				luma += 0.587 * float64(v)
			} else {
				luma += 0.114 * float64(v)
			}
		}
		absX := seg.MinX + p%seg.Dx()
		absY := seg.MinY + p/seg.Dx()
		old := prev.Pixel(absX, absY)
		for c := 0; c < ch; c++ {
			if tile[p*ch+c] != old[c] {
				s.ChangedPixels++
				break
			}
		}
	}
	if pixelCount > 0 {
		s.MeanRGB = [3]float64{float64(sum[0]) / float64(pixelCount), float64(sum[1]) / float64(pixelCount), float64(sum[2]) / float64(pixelCount)}
		s.MeanLuma = luma / float64(pixelCount)
	}
	return s
}

func (e *Engine) resolve(rel string) string {
	return filepath.Join(e.Root, filepath.FromSlash(rel))
}

func (e *Engine) readVerified(rel, wantHash string) ([]byte, error) {
	b, err := os.ReadFile(e.resolve(rel))
	if err != nil {
		return nil, errs.Wrap(errs.ECache, err, "read cached segment blob %s", rel)
	}
	if got := imagex.HashBytes(b); got != wantHash {
		return nil, errs.New(errs.ECache, "cached segment hash mismatch for %s: want %s got %s", rel, wantHash, got)
	}
	return b, nil
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errs.Wrap(errs.EIO, err, "create parent directory")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return errs.Wrap(errs.EIO, err, "write temp file")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.Wrap(errs.EIO, err, "atomic rename")
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errs.Wrap(errs.EBuild, err, "marshal json")
	}
	return writeFileAtomic(path, b)
}

func fmtStepName(step int, typ string) string {
	return "step_" + itoa2(step) + "_" + typ
}

func itoa2(i int) string {
	if i < 10 {
		return "0" + itoa(i)
	}
	return itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

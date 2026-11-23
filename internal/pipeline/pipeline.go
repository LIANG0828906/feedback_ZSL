// Package pipeline orchestrates block evaluation, deterministic window
// ownership across overlap bands, bounded worker parallelism and the three run
// paths (realtime, resume, full recompute) that must produce identical output.
package pipeline

import (
	"context"
	"sort"
	"sync"

	"vqa/internal/errs"
	"vqa/internal/fixed"
	"vqa/internal/metrics"
	"vqa/internal/propagation"
	"vqa/internal/sample"
	"vqa/internal/statemachine"
	"vqa/internal/store"
)

// Run selects how an evaluation path treats an existing checkpoint.
type Run string

const (
	Realtime Run = "realtime"
	Resume   Run = "resume"
	Rebuild  Run = "rebuild"
)

// CrashPlan simulates interruption for acceptance of the resume guarantee.
type CrashPlan struct {
	AfterCommits int // kill the run after this many block commits; 0 disables
}

// Engine evaluates one sequence into one checkpoint store.
type Engine struct {
	Seq         *sample.Sequence
	Params      metrics.Params
	StoreDir    string
	Workers     int
	MaxPixelBuf int
	Crash       CrashPlan
	EdgeTol     int
}

// blockJob is the immutable input handed to a worker.
type blockJob struct {
	frame int
	bx    int
	by    int
}

// blockOutput is the complete frozen result returned by a worker.
type blockOutput struct {
	job    blockJob
	record store.BlockRecord
}

// Run executes the selected path. Realtime and rebuild start from an empty
// store; resume reuses durable blocks after full metadata validation.
func (e *Engine) Run(ctx context.Context, mode Run) (*store.Store, error) {
	if err := e.Params.Validate(); err != nil {
		return nil, err
	}
	manifestHash := store.HashBytes(mustMarshal(e.Seq.Manifest))
	var st *store.Store
	var err error
	switch mode {
	case Rebuild, Realtime:
		st, err = store.Create(e.StoreDir, store.Meta{
			Quality:        e.Seq.Quality,
			ManifestHash:   manifestHash,
			Params:         e.Params,
			FrameCount:     e.Seq.Manifest.Frames,
			BlocksPerFrame: (e.Params.FrameWidth / e.Params.BlockSize) * (e.Params.FrameHeight / e.Params.BlockSize),
		})
	case Resume:
		st, err = store.Open(e.StoreDir, manifestHash, e.Seq.Quality, e.Params)
		if err != nil {
			// Any mismatch invalidates the whole snapshot: recompute from zero.
			st, err = store.Create(e.StoreDir, store.Meta{
				Quality:        e.Seq.Quality,
				ManifestHash:   manifestHash,
				Params:         e.Params,
				FrameCount:     e.Seq.Manifest.Frames,
				BlocksPerFrame: (e.Params.FrameWidth / e.Params.BlockSize) * (e.Params.FrameHeight / e.Params.BlockSize),
			})
		}
	default:
		return nil, errs.New(errs.EPara, "unknown run mode").With("mode", string(mode))
	}
	if err != nil {
		return nil, err
	}
	if err := e.evaluate(ctx, st); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// evaluate walks blocks in global raster order. Parallel workers may compute
// concurrently, but commits are serialized through the store in the exact same
// sequence as the single-threaded path.
func (e *Engine) evaluate(ctx context.Context, st *store.Store) error {
	cols := e.Params.FrameWidth / e.Params.BlockSize
	rows := e.Params.FrameHeight / e.Params.BlockSize

	// Global deterministic job order. Blocks already durable (resume) are
	// skipped up front, so the remaining sequence stays identical across runs.
	var order []blockJob
	for f := 0; f < e.Seq.Manifest.Frames; f++ {
		for by := 0; by < rows; by++ {
			for bx := 0; bx < cols; bx++ {
				if !st.HasBlock(f, bx, by) {
					order = append(order, blockJob{f, bx, by})
				}
			}
		}
	}

	jobs := make(chan blockJob)
	results := make(chan blockOutput, e.Workers)
	errCh := make(chan error, 1)

	// Bounded pixel buffer per worker: each worker allocates at most one block
	// tile plus one recon tile; the semaphore caps total in-flight pixels.
	buf := e.MaxPixelBuf
	if buf <= 0 {
		buf = 64 * 1024
	}
	sem := make(chan struct{}, buf/(e.Params.BlockSize*e.Params.BlockSize*e.Params.Channels))
	if cap(sem) == 0 {
		sem = make(chan struct{}, 1)
	}
	var wg sync.WaitGroup
	for w := 0; w < e.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				out, err := e.evalBlock(job)
				<-sem
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				select {
				case results <- out:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Producer emits the deterministic global block order.
	go func() {
		defer close(jobs)
		for _, job := range order {
			select {
			case jobs <- job:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Collector commits strictly in global order regardless of worker finish.
	pending := map[blockJob]blockOutput{}
	committed := len(st.Blocks())
	nextIdx := 0
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		select {
		case err := <-errCh:
			return err
		case out := <-results:
			pending[out.job] = out
			for nextIdx < len(order) {
				want := order[nextIdx]
				got, ready := pending[want]
				if !ready {
					break
				}
				delete(pending, want)
				nextIdx++
				if err := st.AppendBlock(&got.record); err != nil {
					return err
				}
				committed++
				if e.Crash.AfterCommits > 0 && committed >= e.Crash.AfterCommits {
					_ = st.Close()
					return errSimulatedCrash
				}
				if e.frameComplete(st, got.job.frame, cols, rows) {
					if err := e.commitFrame(st, got.job.frame); err != nil {
						return err
					}
				}
			}
		case <-done:
			if nextIdx >= len(order) && len(pending) == 0 {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *Engine) frameComplete(st *store.Store, frame, cols, rows int) bool {
	count := 0
	for _, r := range st.Blocks() {
		if r.Frame == frame {
			count++
		}
	}
	return count == cols*rows
}

// commitFrame aggregates block windows and stats into a frozen frame record.
func (e *Engine) commitFrame(st *store.Store, frame int) error {
	for _, fr := range st.Frames() {
		if fr.Frame == frame {
			return nil // frame summary is durable and frozen
		}
	}
	orig := e.Seq.Orig[frame]
	rec := e.Seq.Recon[frame]
	tracer := fixed.NewTracer()
	grid := metrics.BuildGrid(e.Params)
	byKey := map[string][]metrics.WindowResult{}
	for _, w := range grid.Tops {
		key := blockKey(frame, w.OwnerBX, w.OwnerBY)
		byKey[key] = append(byKey[key], w)
	}
	var allWindows []metrics.WindowResult
	for _, br := range st.Blocks() {
		if br.Frame != frame {
			continue
		}
		// Each grid window belongs to exactly one block; take each committed
		// block's windows once in grid (raster) order so resume and full runs
		// merge the identical multiset regardless of completion order.
		want := len(byKey[blockKey(frame, br.BX, br.BY)])
		if len(br.Windows) != want {
			return errs.New(errs.EBand, "block window count differs from grid ownership").
				WithInt("frame", int64(frame)).
				WithInt("bx", int64(br.BX)).WithInt("by", int64(br.BY)).
				WithInt("expected", int64(want)).WithInt("actual", int64(len(br.Windows)))
		}
		allWindows = append(allWindows, br.Windows...)
	}
	if len(allWindows) != len(grid.Tops) {
		return errs.New(errs.EDiverge, "merged window set differs from global grid").
			WithInt("frame", int64(frame)).
			WithInt("expected", int64(len(grid.Tops))).WithInt("actual", int64(len(allWindows)))
	}
	// Fixed point averaging is order-sensitive; normalize to the global grid's
	// raster order so merge results never depend on block completion order.
	sort.SliceStable(allWindows, func(i, j int) bool {
		if allWindows[i].TopY != allWindows[j].TopY {
			return allWindows[i].TopY < allWindows[j].TopY
		}
		return allWindows[i].TopX < allWindows[j].TopX
	})
	ssim := metrics.MeanSSIM(allWindows, tracer)
	psnr, _ := metrics.FramePSNR(orig, rec)
	report := propagation.Analyze(frame, orig, rec, e.Params.BlockSize, e.Seq.Quality)
	if err := propagation.VerifyBound(report, psnr); err != nil {
		return err
	}
	return st.AppendFrame(&store.FrameRecord{
		Frame:       frame,
		PSNR:        psnr,
		SSIMQ16:     int64(ssim),
		PSNRLower:   report.Bound.PSNRLowerBound,
		WindowCount: len(allWindows),
	})
}

// evalBlock computes the full frozen record for one block, including the
// globally-owned SSIM windows that fall within its overlap band.
func (e *Engine) evalBlock(job blockJob) (blockOutput, error) {
	frame := e.Seq.Orig[job.frame]
	rec := e.Seq.Recon[job.frame]
	tracer := fixed.NewTracer()
	sm := statemachine.New(job.by*(e.Params.FrameWidth/e.Params.BlockSize) + job.bx)
	if err := sm.Transition(statemachine.Evaluating); err != nil {
		return blockOutput{}, err
	}
	stats, _, quantiles := metrics.EvaluateBlockStats(frame, rec, job.bx, job.by, e.Params, tracer)
	grid := metrics.BuildGrid(e.Params)
	if err := grid.CheckBand(); err != nil {
		return blockOutput{}, err
	}
	var windows []metrics.WindowResult
	for _, w := range grid.Tops {
		if w.OwnerBX == job.bx && w.OwnerBY == job.by {
			s := metrics.EvaluateWindow(frame, rec, w.TopX, w.TopY, e.Params, tracer)
			w.SSIMR = int64(s)
			windows = append(windows, w)
		}
	}
	edges := e.edgesForBlock(job)
	edgeFidelity := metrics.EvaluateEdgeFidelity(frame, rec, edges, e.EdgeTol, tracer)
	report := propagation.Analyze(job.frame, frame, rec, e.Params.BlockSize, e.Seq.Quality)
	var bp propagation.BlockPropagation
	for _, b := range report.Blocks {
		if b.BX == job.bx && b.BY == job.by {
			bp = b
		}
	}
	psnr, _ := metrics.FramePSNR(frame, rec)
	recon := blockPixels(rec, job.bx, job.by, e.Params)
	if err := sm.Transition(statemachine.Committed); err != nil {
		return blockOutput{}, err
	}
	rec2 := blockOutput{
		job: job,
		record: store.BlockRecord{
			Frame:        job.frame,
			BX:           job.bx,
			BY:           job.by,
			State:        string(statemachine.Committed),
			InputHash:    store.HashFrame(frame),
			ParamHash:    store.HashParams(e.Params),
			Params:       e.Params,
			Stats:        stats,
			Quantiles:    quantiles,
			Windows:      windows,
			Propagation:  bp,
			EdgeFidelity: edgeFidelity,
			Truncations:  tracer.Events,
			ReconDigest:  store.HashBytes(recon),
			ReconPixels:  recon,
			PSNR:         psnr,
		},
	}
	return rec2, nil
}

func (e *Engine) edgesForBlock(job blockJob) []sample.EdgePixel {
	fp, ok := e.Seq.Manifest.Fingerprints[fpKey(e.Seq.Quality, job.frame)]
	if !ok {
		return nil
	}
	n := e.Params.BlockSize
	var out []sample.EdgePixel
	for _, edge := range fp.EdgePixels {
		if edge.X/n == job.bx && edge.Y/n == job.by {
			out = append(out, edge)
		}
	}
	return out
}

func blockPixels(f *sample.Frame, bx, by int, p metrics.Params) []uint8 {
	n := p.BlockSize
	out := make([]uint8, n*n*p.Channels)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			for c := 0; c < p.Channels; c++ {
				out[(y*n+x)*p.Channels+c] = f.Pix[((by*n+y)*p.FrameWidth+(bx*n+x))*p.Channels+c]
			}
		}
	}
	return out
}

func fpKey(quality, frame int) string {
	return itoa(quality) + ":" + itoa(frame)
}

func blockKey(frame, bx, by int) string {
	return itoa(frame) + ":" + itoa(bx) + ":" + itoa(by)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	b := make([]byte, 0, 8)
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

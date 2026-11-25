package kernel

import (
	"sort"
	"sync"
	"time"

	"obsproj/internal/model"
)

type MapJob struct {
	Window       int
	Screen       model.Screen
	Source       model.Rect
	TimeResidual int64
}

type MapResult struct {
	Mapped  model.MappedRegion
	Metrics model.ErrorMetrics
	Err     *model.KernelError
}

type MapPoolOptions struct {
	Workers            int
	BufferPixels       int64
	DiagnosticJobDelay time.Duration
}

func DefaultMapPoolOptions() MapPoolOptions {
	return MapPoolOptions{
		Workers:      4,
		BufferPixels: 65536,
	}
}

type worker struct {
	id     int
	buffer []byte
}

// MapAll runs mapping jobs with bounded parallelism and a fixed per-worker
// scratch buffer. A job waits when every worker is occupied; that wait is
// recorded instead of dropping the job. Results are re-sorted by screen ID, so
// caller-visible output does not depend on dispatch order or concurrency.
func MapAll(jobs []MapJob, options MapPoolOptions) ([]MapResult, []model.QueueObservation, *model.KernelError) {
	if options.Workers <= 0 {
		options.Workers = 1
	}
	if options.BufferPixels <= 0 {
		options.BufferPixels = 65536
	}
	sorted := append([]MapJob(nil), jobs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Screen.ID != sorted[j].Screen.ID {
			return sorted[i].Screen.ID < sorted[j].Screen.ID
		}
		return sorted[i].Window < sorted[j].Window
	})
	if len(sorted) == 0 {
		return nil, nil, nil
	}
	results := make([]MapResult, len(sorted))
	observations := make([]model.QueueObservation, len(sorted))
	bufferBytes, bufferErr := model.CheckedMul("pool.buffer_bytes", options.BufferPixels, 4)
	if bufferErr != nil {
		return nil, nil, bufferErr
	}
	tokens := make(chan *worker, options.Workers)
	for i := 0; i < options.Workers; i++ {
		tokens <- &worker{id: i, buffer: make([]byte, int(bufferBytes))}
	}
	var wg sync.WaitGroup
	for i := range sorted {
		start := time.Now()
		var (
			w          *worker
			hadToQueue bool
		)
		select {
		case w = <-tokens:
		default:
			hadToQueue = true
			w = <-tokens
		}
		waited := time.Since(start).Microseconds()
		job := sorted[i]
		if hadToQueue {
			observations[i] = model.QueueObservation{
				Window:     job.Window,
				ScreenID:   job.Screen.ID,
				Reason:     "all_worker_buffers_busy",
				WaitMicros: waited,
			}
		}
		estimatedArea, areaErr := model.CheckedMul("pool."+job.Screen.ID+".pixels", job.Screen.Resolution.W, job.Screen.Resolution.H)
		if areaErr != nil {
			results[i] = MapResult{Err: areaErr}
			tokens <- w
			continue
		}
		if estimatedArea > options.BufferPixels {
			reason := "job_exceeds_per_thread_pixel_buffer_chunked"
			if observations[i].Reason == "" {
				observations[i] = model.QueueObservation{
					Window:     job.Window,
					ScreenID:   job.Screen.ID,
					Reason:     reason,
					WaitMicros: waited,
				}
			} else {
				observations[i].Reason += "," + reason
			}
		}
		wg.Add(1)
		go func(index int, j MapJob, wk *worker, area int64) {
			defer wg.Done()
			defer func() { tokens <- wk }()
			if options.DiagnosticJobDelay > 0 {
				time.Sleep(options.DiagnosticJobDelay)
			}
			mapped, metrics, mapErr := MapRegion(j.Screen, j.Source, j.TimeResidual)
			if mapErr == nil {
				// Touch the bounded scratch buffer. Large screens are processed in
				// deterministic chunks rather than allocating an unbounded frame.
				chunks := area / options.BufferPixels
				if area%options.BufferPixels != 0 {
					chunks++
				}
				for chunk := int64(0); chunk < chunks; chunk++ {
					wk.buffer[int(chunk)%len(wk.buffer)] = byte((chunk + int64(wk.id)) & 0xff)
				}
			}
			results[index] = MapResult{Mapped: mapped, Metrics: metrics, Err: mapErr}
		}(i, job, w, estimatedArea)
	}
	wg.Wait()

	outObs := make([]model.QueueObservation, 0, len(observations))
	for _, observation := range observations {
		if observation.Reason != "" {
			outObs = append(outObs, observation)
		}
	}
	var firstErr *model.KernelError
	for i := range results {
		if results[i].Err != nil {
			firstErr = results[i].Err
			break
		}
	}
	return results, outObs, firstErr
}

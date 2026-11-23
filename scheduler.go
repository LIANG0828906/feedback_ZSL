package terrainbake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
)

// EngineOptions controls scheduler resources and checkpoint behavior.
type EngineOptions struct {
	StoreDir             string
	Params               BakeParams
	MaxWorkers           int
	MaxThreadPixels      int
	NormalMapMemoryBytes int64
	PauseAfter           int
	FaultInjector        func(blockID string, attempt int) error
}

// DefaultEngineOptions returns conservative production defaults.
func DefaultEngineOptions(storeDir string) EngineOptions {
	return EngineOptions{
		StoreDir:             storeDir,
		Params:               DefaultBakeParams(),
		MaxWorkers:           2,
		MaxThreadPixels:      8 * 1024 * 1024,
		NormalMapMemoryBytes: 256 * 1024 * 1024,
	}
}

func normalizeOptions(opts EngineOptions) (EngineOptions, error) {
	if opts.Params.Operator == "" {
		opts.Params = DefaultBakeParams()
	}
	if err := ValidateBakeParams(opts.Params); err != nil {
		return opts, err
	}
	if opts.StoreDir == "" {
		return opts, &Error{Code: CodeParam, Object: "engine_options", Field: "store_dir", Message: "store directory is required"}
	}
	if opts.MaxWorkers <= 0 {
		return opts, &Error{Code: CodeParam, Object: "engine_options", Field: "max_workers", Message: "max_workers must be positive"}
	}
	if opts.MaxWorkers > 256 {
		return opts, &Error{Code: CodeParam, Object: "engine_options", Field: "max_workers", Message: "max_workers must be at most 256"}
	}
	if opts.MaxThreadPixels <= 0 {
		return opts, &Error{Code: CodeParam, Object: "engine_options", Field: "max_thread_pixels", Message: "max_thread_pixels must be positive"}
	}
	if opts.NormalMapMemoryBytes <= 0 {
		return opts, &Error{Code: CodeParam, Object: "engine_options", Field: "normal_map_memory_bytes", Message: "normal_map_memory_bytes must be positive"}
	}
	if opts.PauseAfter < 0 {
		return opts, &Error{Code: CodeParam, Object: "engine_options", Field: "pause_after", Message: "pause_after must be non-negative"}
	}
	return opts, nil
}

type memLimiter struct {
	mu   sync.Mutex
	cap  int64
	used int64
	ch   chan struct{}
}

func newMemLimiter(capacity int64) *memLimiter {
	return &memLimiter{cap: capacity, ch: make(chan struct{})}
}

// acquire reserves n bytes. waited reports that the caller had to queue.
func (m *memLimiter) acquire(ctx context.Context, n int64) (waited bool, err error) {
	if n > m.cap {
		return false, &Error{
			Code: CodeResource, Object: "resource_budget", Field: "normal_map_memory_bytes",
			Message: fmt.Sprintf("a single block needs %d bytes, budget is %d", n, m.cap),
			Limit:   float64(m.cap),
		}
	}
	for {
		m.mu.Lock()
		if m.used+n <= m.cap {
			m.used += n
			m.mu.Unlock()
			return waited, nil
		}
		waited = true
		ch := m.ch
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return waited, &Error{Code: CodeState, Object: "scheduler", Field: "context", Message: ctx.Err().Error()}
		case <-ch:
		}
	}
}

func (m *memLimiter) release(n int64) {
	m.mu.Lock()
	m.used -= n
	if m.used < 0 {
		m.used = 0
	}
	close(m.ch)
	m.ch = make(chan struct{})
	m.mu.Unlock()
}

func estimateRawBytes(pixels int) int64 {
	return int64(pixels) * 32
}

func estimateFinalBytes(pixels int) int64 {
	return int64(pixels) * 48
}

func bakeRawTasks(
	ctx context.Context,
	p *Project,
	params BakeParams,
	ids []string,
	opts EngineOptions,
	store *Store,
) (map[string]string, []QueueEvent, map[string]string, error) {
	paths := make(map[string]string, len(ids))
	failures := make(map[string]string)
	if len(ids) == 0 {
		return paths, nil, failures, nil
	}
	limiter := newMemLimiter(opts.NormalMapMemoryBytes)
	jobs := make(chan struct {
		id      string
		path    string
		reserve int64
	})
	results := make(chan struct {
		id      string
		path    string
		err     error
		reserve int64
	}, len(ids))
	var wg sync.WaitGroup
	for w := 0; w < opts.MaxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				var raw *RawBlock
				var err error
				func() {
					defer limiter.release(job.reserve)
					defer func() {
						if r := recover(); r != nil {
							err = &Error{Code: CodeBake, Object: "block:" + job.id, Field: "panic", BlockID: job.id, Message: fmt.Sprint(r)}
						}
					}()
					raw, err = bakeRawBlock(p, job.id, params)
					if err != nil {
						return
					}
					var data []byte
					data, err = encodeRawBinary(raw)
					if err == nil {
						err = writeFileAtomic(job.path, data, 0o644)
					}
				}()
				results <- struct {
					id      string
					path    string
					err     error
					reserve int64
				}{job.id, job.path, err, job.reserve}
			}
		}()
	}

	var queue []QueueEvent
	var dispatchErr error
	dispatched := 0
	for i, id := range ids {
		b, ok := FindBlock(p, id)
		if !ok {
			dispatchErr = &Error{Code: CodeSchema, Object: "block", Field: "id", BlockID: id, Message: "block does not exist"}
			break
		}
		pixels := b.Width * b.Height
		if pixels > opts.MaxThreadPixels {
			dispatchErr = &Error{
				Code: CodeResource, Object: "block:" + id, Field: "max_thread_pixels", BlockID: id,
				Message: fmt.Sprintf("block needs a %d-pixel thread buffer, limit is %d", pixels, opts.MaxThreadPixels),
				Limit:   float64(opts.MaxThreadPixels),
			}
			queue = append(queue, QueueEvent{BlockID: id, Reason: "per_thread_pixel_budget_exhausted"})
			break
		}
		reserve := estimateRawBytes(pixels)
		waited, err := limiter.acquire(ctx, reserve)
		if err != nil {
			if be, ok := AsError(err); ok && be.BlockID == "" {
				be.BlockID = id
			}
			dispatchErr = err
			break
		}
		if waited {
			queue = append(queue, QueueEvent{BlockID: id, Reason: "normal_map_budget_wait"})
		}
		path := store.TempRawPath(id, i)
		select {
		case jobs <- struct {
			id      string
			path    string
			reserve int64
		}{id, path, reserve}:
			dispatched++
		case <-ctx.Done():
			limiter.release(reserve)
			dispatchErr = &Error{Code: CodeState, Object: "scheduler", Field: "context", Message: ctx.Err().Error()}
		}
		if dispatchErr != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	for res := range results {
		if res.err != nil {
			if be, ok := AsError(res.err); ok {
				be.BlockID = res.id
				failures[res.id] = be.Message
			} else {
				failures[res.id] = res.err.Error()
			}
			continue
		}
		paths[res.id] = res.path
	}
	if dispatchErr != nil {
		return paths, queue, failures, dispatchErr
	}
	if len(failures) != 0 {
		ids := sortedStrings(mapKeys(failures))
		return paths, queue, failures, &Error{
			Code: CodeBake, Object: "block:" + ids[0], Field: "bake", BlockID: ids[0],
			Message: "raw block baking failed; failed blocks remain retryable",
			Details: map[string]any{"failed": failures},
		}
	}
	if dispatched != len(ids) {
		return paths, queue, failures, &Error{Code: CodeState, Object: "scheduler", Field: "dispatch", Message: "not all raw tasks were dispatched"}
	}
	return paths, queue, failures, nil
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func loadEdgeNormals(p *Project, params BakeParams, epoch []string, rawPaths map[string]string) (map[pixelKey]Vec3, error) {
	pairs, err := buildSeamPairs(p)
	if err != nil {
		return nil, err
	}
	inEpoch := make(map[string]bool, len(epoch))
	for _, id := range epoch {
		inEpoch[id] = true
	}
	needed := make(map[string]map[pixelKey]bool)
	for _, pair := range pairs {
		if !inEpoch[pair.blockA] || !inEpoch[pair.blockB] {
			continue
		}
		if needed[pair.blockA] == nil {
			needed[pair.blockA] = make(map[pixelKey]bool)
		}
		if needed[pair.blockB] == nil {
			needed[pair.blockB] = make(map[pixelKey]bool)
		}
		needed[pair.blockA][pair.keyA] = true
		needed[pair.blockB][pair.keyB] = true
	}
	out := make(map[pixelKey]Vec3)
	for id, keys := range needed {
		var raw *RawBlock
		if path, ok := rawPaths[id]; ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, &Error{Code: CodeIO, Object: "raw_block", Field: "path", BlockID: id, Message: err.Error()}
			}
			raw, err = decodeRawBinary(data)
			if err != nil {
				return nil, err
			}
			raw.BlockID = id
		} else {
			raw, err = bakeRawBlock(p, id, params)
			if err != nil {
				return nil, err
			}
		}
		for k := range keys {
			if k.x < 0 || k.x >= raw.Width || k.y < 0 || k.y >= raw.Height {
				return nil, &Error{Code: CodeBake, Object: "seam", Field: "pixel", BlockID: id, X: k.x, Y: k.y, Message: "edge pixel outside raw block"}
			}
			out[k] = raw.Normals[k.y*raw.Width+k.x]
		}
	}
	return out, nil
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return &Error{Code: CodeState, Object: "scheduler", Field: "context", Message: "bake canceled"}
		}
		return &Error{Code: CodeState, Object: "scheduler", Field: "context", Message: err.Error()}
	}
	return nil
}

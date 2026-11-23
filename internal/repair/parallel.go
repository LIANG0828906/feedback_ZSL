package repair

import (
	"errors"
	"fmt"
	"sync"
)

// WorkerPool bounds both the number of concurrent tasks and the amount of
// face data a task may reserve. Results are merged in task order only, so
// serial and parallel execution are deterministic.
type WorkerPool struct {
	MaxWorkers      int
	MaxFacesPerTask int
}

func NewWorkerPool(maxWorkers, maxFacesPerTask int) *WorkerPool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if maxWorkers > 64 {
		maxWorkers = 64
	}
	return &WorkerPool{MaxWorkers: maxWorkers, MaxFacesPerTask: maxFacesPerTask}
}

type ChunkSummary struct {
	Chunk int     `json:"chunk"`
	Faces int     `json:"faces"`
	Verts int     `json:"vertices"`
	AreaB float64 `json:"area"`
}

func (p *WorkerPool) ChunkSummary(faces [][3]int, chunkSize int) ([]ChunkSummary, error) {
	if len(faces) == 0 {
		return nil, nil
	}
	if chunkSize < 1 {
		return nil, errors.New("chunk size must be positive")
	}
	if p.MaxFacesPerTask > 0 && chunkSize > p.MaxFacesPerTask {
		return nil, fmt.Errorf("chunk size %d exceeds per-worker buffer limit %d", chunkSize, p.MaxFacesPerTask)
	}
	nchunks := (len(faces) + chunkSize - 1) / chunkSize
	out := make([]ChunkSummary, nchunks)
	jobs := make(chan int)
	errsCh := make(chan error, p.MaxWorkers)
	var wg sync.WaitGroup
	for w := 0; w < p.MaxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ci := range jobs {
				start := ci * chunkSize
				end := start + chunkSize
				if end > len(faces) {
					end = len(faces)
				}
				seen := make(map[int]bool)
				for _, f := range faces[start:end] {
					seen[f[0]] = true
					seen[f[1]] = true
					seen[f[2]] = true
				}
				out[ci] = ChunkSummary{Chunk: ci, Faces: end - start, Verts: len(seen)}
			}
		}()
	}
	for i := 0; i < nchunks; i++ {
		select {
		case jobs <- i:
		case err := <-errsCh:
			close(jobs)
			wg.Wait()
			return nil, err
		}
	}
	close(jobs)
	wg.Wait()
	return out, nil
}

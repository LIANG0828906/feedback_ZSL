package pipeline

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"maskpipe/internal/model"
	"maskpipe/internal/store"
	"maskpipe/internal/vision"
)

type stageWorkFunc func(chunk Chunk) (blob []byte, geometry any, note string, err error)

func (s *Service) executeStage(
	ctx context.Context,
	stage model.Stage,
	eligible map[int]bool,
	existing map[int]store.StageRecord,
	blobs [][]byte,
	errs []error,
	progress func(ProgressEvent),
	work stageWorkFunc,
) {
	var pending []Chunk
	for _, chunk := range s.Chunks {
		if !eligible[chunk.Index] {
			continue
		}
		if existing != nil {
			if _, ok := existing[chunk.Index]; ok {
				continue
			}
		}
		pending = append(pending, chunk)
	}
	if len(pending) == 0 {
		if progress != nil {
			committed, total := s.Machine.Progress(len(s.Chunks))
			progress(ProgressEvent{Committed: committed, Total: total, Stage: string(stage)})
		}
		return
	}

	workerCount := s.Budget.WorkerCount
	if workerCount > len(pending) {
		workerCount = len(pending)
	}
	queueBudget := s.Budget.PendingTaskQueueBudget
	if queueBudget < 1 {
		queueBudget = 1
	}
	tasks := make(chan Chunk, queueBudget)
	var wg sync.WaitGroup
	var active int32

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range tasks {
				key := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: chunk.X, Y: chunk.Y}, Stage: stage}
				bufferPixels := chunk.Pixels * BufferUnitsPerPixel
				currentActive := atomic.AddInt32(&active, 1)
				s.Metrics.observeStart(int(currentActive), len(tasks), bufferPixels)

				waitErr := s.Control.Wait(ctx)
				if waitErr != nil {
					_ = s.Machine.Transition(key, model.StatusProcessing, s.CurrentParams().ParamVersion, nil)
					_ = s.Machine.Transition(key, model.StatusFailed, "", waitErr)
					errs[chunk.Index] = waitErr
					atomic.AddInt32(&active, -1)
					s.Metrics.taskFinished()
					continue
				}

				params := s.CurrentParams()
				if err := s.Machine.Transition(key, model.StatusProcessing, params.ParamVersion, nil); err != nil {
					errs[chunk.Index] = err
					atomic.AddInt32(&active, -1)
					continue
				}

				blob, geometry, note, workErr := work(chunk)
				if workErr != nil {
					errs[chunk.Index] = workErr
					_ = s.Machine.Transition(key, model.StatusFailed, "", workErr)
					atomic.AddInt32(&active, -1)
					s.Metrics.taskFinished()
					continue
				}

				geometryJSON := ""
				if geometry != nil {
					if b, err := json.Marshal(geometry); err == nil {
						geometryJSON = string(b)
					}
				}
				foreground := 0
				if stage != model.StageStats {
					for _, v := range blob {
						if v != 0 {
							foreground++
						}
					}
					if blobs != nil {
						blobs[chunk.Index] = blob
					}
				}
				summary := StageSummary{
					ChunkX: chunk.X, ChunkY: chunk.Y, PixelX: chunk.PixelX, PixelY: chunk.PixelY,
					Width: chunk.Width, Height: chunk.Height, Encoding: "raw-gray8",
					ForegroundPixels: foreground, BufferPixels: bufferPixels, Downsample: 1,
					Note: note,
				}
				summaryBytes, _ := json.Marshal(summary)
				rec := store.StageRecord{
					SnapshotID: s.SnapshotID(), ChunkX: chunk.X, ChunkY: chunk.Y, Stage: stage,
					CanonicalOrder: CanonicalOrder(chunk, stage), ParamVersion: params.ParamVersion,
					AlgoVersion: vision.AlgorithmVersion, Downsample: 1,
					SummaryJSON: string(summaryBytes), GeometryJSON: geometryJSON,
					Blob: blob, ContentHash: store.HashBytes(blob),
				}
				if s.Store != nil {
					if _, err := s.Store.CommitStage(ctx, rec); err != nil {
						errs[chunk.Index] = err
						_ = s.Machine.Transition(key, model.StatusFailed, "", err)
						atomic.AddInt32(&active, -1)
						s.Metrics.taskFinished()
						continue
					}
				}
				if err := s.Machine.Transition(key, model.StatusCommitted, params.ParamVersion, nil); err != nil {
					errs[chunk.Index] = err
				}
				committed, total := s.Machine.Progress(len(s.Chunks))
				if progress != nil {
					progress(ProgressEvent{
						Committed: committed, Total: total, Stage: string(stage),
						Chunk: key.ID(),
					})
				}
				atomic.AddInt32(&active, -1)
				s.Metrics.taskFinished()
			}
		}()
	}

	for _, chunk := range pending {
		select {
		case tasks <- chunk:
		default:
			event := QueueEvent{
				Time:  time.Now().UTC().Format(time.RFC3339Nano),
				Chunk: model.ChunkKey{ChunkCoord: model.ChunkCoord{X: chunk.X, Y: chunk.Y}}.ID(),
				Stage: string(stage), Reason: "queue_budget_exhausted",
				QueueBudget: queueBudget,
			}
			start := time.Now()
			select {
			case tasks <- chunk:
				event.WaitingMillis = time.Since(start).Milliseconds()
				s.Metrics.recordQueue(event)
			case <-ctx.Done():
				errs[chunk.Index] = ctx.Err()
			}
		}
	}
	close(tasks)
	wg.Wait()
}

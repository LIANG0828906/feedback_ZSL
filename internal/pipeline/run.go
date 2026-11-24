package pipeline

import (
	"context"
	"sync"

	"cadpipe/internal/render"
)

type RunReport struct {
	Requested   []int        `json:"requested"`
	Recomputed  []int        `json:"recomputed"`
	Reused      []int        `json:"reused"`
	Committed   int          `json:"committed"`
	WorkerCount int          `json:"worker_count"`
	BufferCount int          `json:"buffer_count"`
	WaitRecords []WaitRecord `json:"wait_records"`
}

type renderOutput struct {
	id     int
	result *render.TileResult
	lease  *BufferLease
	err    error
}

func (p *Pipeline) RunFull(ctx context.Context) (*RunReport, error) {
	return p.runIDs(ctx, p.SortedTileIDs(nil))
}

func (p *Pipeline) RunPlan(ctx context.Context, plan *InvalidationPlan) (*RunReport, error) {
	if plan == nil {
		return &RunReport{}, nil
	}
	return p.runIDs(ctx, p.SortedTileIDs(plan.TileIDs))
}

func (p *Pipeline) ResumeMissing(ctx context.Context) (*RunReport, error) {
	missing := []int{}
	for id := 0; id < p.Camera.TileCount(); id++ {
		if p.Cache[id] == nil {
			missing = append(missing, id)
		}
	}
	return p.runIDs(ctx, missing)
}

func (p *Pipeline) runIDs(ctx context.Context, ids []int) (*RunReport, error) {
	ids = p.SortedTileIDs(ids)
	report := &RunReport{Requested: append([]int(nil), ids...), WorkerCount: p.Options.Workers, BufferCount: p.Options.BufferCount}
	requested := make(map[int]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	for id := 0; id < p.Camera.TileCount(); id++ {
		if p.Cache[id] != nil && !requested[id] {
			report.Reused = append(report.Reused, id)
		}
	}
	if len(ids) == 0 {
		return report, nil
	}

	views := p.Views()
	jobs := make(chan int)
	outputs := make(chan renderOutput, len(ids))
	var wg sync.WaitGroup
	for w := 0; w < p.Options.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				pixels := p.Camera.TileWidth * p.Camera.TileHeight
				lease, err := p.Pool.Acquire(id, pixels)
				if err != nil {
					outputs <- renderOutput{id: id, err: err}
					continue
				}
				result, err := render.RenderTile(p.Camera, id, views, p.Versions.SceneStructure, lease.Buffer())
				if err != nil {
					lease.Release()
					outputs <- renderOutput{id: id, err: err}
					continue
				}
				result.DependencyHash = p.DependencyHashForPatches(sortedKeys(p.TilePatches[id]))
				if p.AfterRenderTile != nil {
					p.AfterRenderTile(id)
				}
				// The committed result owns immutable memory. Release the bounded
				// worker buffer before entering the ordered commit queue so an
				// out-of-order tile cannot exhaust buffers needed by the head tile.
				result.Color = append([]byte(nil), result.Color...)
				result.Depth = append([]float64(nil), result.Depth...)
				lease.Release()
				outputs <- renderOutput{id: id, result: result}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(outputs)
	}()

	pending := make(map[int]renderOutput)
	next := 0
	processed := 0
	for processed < len(ids) {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case out, ok := <-outputs:
			if !ok {
				if ctx.Err() != nil {
					return report, ctx.Err()
				}
				break
			}
			processed++
			pending[out.id] = out
			for next < len(ids) {
				if err := ctx.Err(); err != nil {
					return report, err
				}
				id := ids[next]
				out, exists := pending[id]
				if !exists {
					break
				}
				if out.err != nil {
					return report, out.err
				}
				if p.Store != nil {
					if err := p.Store.Save(ctx, out.result); err != nil {
						if out.lease != nil {
							out.lease.Release()
						}
						return report, err
					}
					report.Committed++
				}
				out.result.Color = append([]byte(nil), out.result.Color...)
				out.result.Depth = append([]float64(nil), out.result.Depth...)
				p.Cache[id] = out.result
				if out.lease != nil {
					out.lease.Release()
				}
				delete(pending, id)
				report.Recomputed = append(report.Recomputed, id)
				next++
				if p.CommitHook != nil {
					p.CommitHook(id, report.Committed)
				}
			}
		}
	}
	wg.Wait()
	report.WaitRecords = append([]WaitRecord(nil), p.Pool.SnapshotWaits()...)
	return report, nil
}

type Frame struct {
	Width  int
	Height int
	Color  []byte
	Depth  []float64
}

func (p *Pipeline) BuildFrame() (*Frame, error) {
	frame := &Frame{
		Width: p.Camera.Width, Height: p.Camera.Height,
		Color: make([]byte, p.Camera.Width*p.Camera.Height*4),
		Depth: make([]float64, p.Camera.Width*p.Camera.Height),
	}
	for i := range frame.Depth {
		frame.Depth[i] = 1.7976931348623157e+308
	}
	for y := 0; y < p.Camera.Height; y++ {
		for x := 0; x < p.Camera.Width; x++ {
			o := (y*p.Camera.Width + x) * 4
			frame.Color[o+0] = 18
			frame.Color[o+1] = 22
			frame.Color[o+2] = 28
			frame.Color[o+3] = 255
		}
	}
	for tileID, tile := range p.Cache {
		tx, ty := p.Camera.TileXY(tileID)
		x0, y0 := tx*p.Camera.TileWidth, ty*p.Camera.TileHeight
		for ly := 0; ly < tile.Height; ly++ {
			for lx := 0; lx < tile.Width; lx++ {
				gx, gy := x0+lx, y0+ly
				if gx >= p.Camera.Width || gy >= p.Camera.Height {
					continue
				}
				local := ly*p.Camera.TileWidth + lx
				global := gy*p.Camera.Width + gx
				copy(frame.Color[global*4:global*4+4], tile.Color[local*4:local*4+4])
				frame.Depth[global] = tile.Depth[local]
			}
		}
	}
	return frame, nil
}

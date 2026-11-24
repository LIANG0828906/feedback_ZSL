package filters

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"yuvench/errs"
	"yuvench/yuv"
)

type Runtime struct {
	Limits       Limits
	OnBufferWait func(reason string)
}

type bufferBudget struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	notify   chan struct{}
	onWait   func(string)
}

func newBufferBudget(capacity int64, onWait func(string)) *bufferBudget {
	return &bufferBudget{capacity: capacity, notify: make(chan struct{}), onWait: onWait}
}

func (b *bufferBudget) acquire(ctx context.Context, bytes int64, reason string) error {
	if bytes > b.capacity {
		return &errs.Error{Code: errs.CodeResource, Field: "perWorkerBufferBytes", Message: "requested plane chunk exceeds global buffer budget", Expected: b.capacity, Actual: bytes}
	}
	waited := false
	for {
		b.mu.Lock()
		if b.used+bytes <= b.capacity {
			b.used += bytes
			b.mu.Unlock()
			return nil
		}
		ch := b.notify
		b.mu.Unlock()
		if !waited {
			waited = true
			if b.onWait != nil {
				b.onWait(reason)
			}
		}
		select {
		case <-ctx.Done():
			return &errs.Error{Code: errs.CodeCancel, Message: "buffer wait cancelled", Cause: ctx.Err().Error()}
		case <-ch:
		}
	}
}

func (b *bufferBudget) release(bytes int64) {
	b.mu.Lock()
	b.used -= bytes
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
}

// Apply executes one stage and returns a new plane with identical dimensions,
// bit depth, and single-channel layout.
func Apply(ctx context.Context, plane *yuv.Plane, node string, params Params, runtime Runtime) (*yuv.Plane, error) {
	if plane == nil {
		return nil, errs.New(errs.CodeStage, "missing input plane")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	limits := runtime.Limits
	if limits.MaxParallel == 0 {
		limits = DefaultLimits()
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	out := &yuv.Plane{
		Name:     plane.Name,
		Width:    plane.Width,
		Height:   plane.Height,
		Channels: plane.Channels,
		BitDepth: plane.BitDepth,
		MinCode:  plane.MinCode,
		MaxCode:  plane.MaxCode,
		Data:     make([]uint16, len(plane.Data)),
	}
	if plane.Width <= 0 || plane.Height <= 0 || len(plane.Data) != plane.Width*plane.Height {
		return nil, &errs.Error{Code: errs.CodeStage, Message: "invalid input plane shape", Plane: plane.Name}
	}
	rowBytes := int64(plane.Width) * 2
	if rowBytes > limits.PerWorkerBufferBytes {
		return nil, &errs.Error{Code: errs.CodeResource, Field: "perWorkerBufferBytes", Message: "one plane row exceeds per-worker buffer", Plane: plane.Name, Expected: limits.PerWorkerBufferBytes, Actual: rowBytes}
	}
	maxRows := int(limits.PerWorkerBufferBytes / rowBytes)
	if maxRows < 1 {
		maxRows = 1
	}
	chunkRows := min(limits.ChunkRows, maxRows, plane.Height)
	if chunkRows < 1 {
		chunkRows = 1
	}
	chunks := (plane.Height + chunkRows - 1) / chunkRows
	workers := min(limits.MaxParallel, chunks)
	if workers < 1 {
		workers = 1
	}
	budget := newBufferBudget(limits.MaxBufferBytes, runtime.OnBufferWait)
	var next atomic.Int64
	var firstErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	worker := func(workerID int) {
		defer wg.Done()
		for {
			if runCtx.Err() != nil {
				return
			}
			chunk := int(next.Add(1) - 1)
			if chunk >= chunks {
				return
			}
			startY := chunk * chunkRows
			endY := min(startY+chunkRows, plane.Height)
			rows := endY - startY
			bytes := int64(plane.Width * rows * 2)
			reason := "stage=" + node + " worker=" + itoa(workerID) + " plane=" + plane.Name
			if err := budget.acquire(runCtx, bytes, reason); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				cancel()
				return
			}
			scratch := make([]uint16, plane.Width*rows)
			for y := startY; y < endY; y++ {
				if runCtx.Err() != nil {
					break
				}
				for x := 0; x < plane.Width; x++ {
					scratch[(y-startY)*plane.Width+x] = applyPixel(node, plane, x, y, params)
				}
			}
			if runCtx.Err() == nil {
				copy(out.Data[startY*plane.Width:endY*plane.Width], scratch)
			}
			budget.release(bytes)
		}
	}
	for workerID := 0; workerID < workers; workerID++ {
		wg.Add(1)
		go worker(workerID)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, &errs.Error{Code: errs.CodeCancel, Message: "stage cancelled", Node: node, Cause: err.Error()}
	}
	return out, nil
}

func applyPixel(node string, plane *yuv.Plane, x, y int, p Params) uint16 {
	center := float64(plane.Pixel(x, y))
	switch node {
	case NodeYDenoise:
		avg := neighborhoodMean(plane, x, y, p.YDenoise.Radius)
		return yuvRound(center*(1-p.YDenoise.Strength)+avg*p.YDenoise.Strength, plane.MinCode, plane.MaxCode)
	case NodeYSharpen:
		avg := neighborhoodMean(plane, x, y, p.YSharpen.Radius)
		return yuvRound(center+p.YSharpen.Amount*(center-avg), plane.MinCode, plane.MaxCode)
	case NodeYContrast:
		radius := p.YContrast.Window / 2
		avg := neighborhoodMean(plane, x, y, radius)
		return yuvRound(center+p.YContrast.Amount*(center-avg), plane.MinCode, plane.MaxCode)
	case NodeUSmooth, NodeVSmooth:
		var radius int
		var strength float64
		if node == NodeUSmooth {
			radius, strength = p.USmooth.Radius, p.USmooth.Strength
		} else {
			radius, strength = p.VSmooth.Radius, p.VSmooth.Strength
		}
		avg := neighborhoodMean(plane, x, y, radius)
		return yuvRound(center*(1-strength)+avg*strength, plane.MinCode, plane.MaxCode)
	case NodeUGain, NodeVGain:
		gain := p.UGain
		if node == NodeVGain {
			gain = p.VGain
		}
		span := float64(plane.MaxCode) - float64(plane.MinCode)
		neutral := float64(plane.MinCode) + span/2
		value := neutral + gain.Gain*(center-neutral) + gain.Offset*span
		return yuvRound(value, plane.MinCode, plane.MaxCode)
	default:
		return yuvRound(center, plane.MinCode, plane.MaxCode)
	}
}

func neighborhoodMean(plane *yuv.Plane, x, y, radius int) float64 {
	if radius < 1 {
		return float64(plane.Pixel(x, y))
	}
	sum := 0
	count := 0
	for yy := y - radius; yy <= y+radius; yy++ {
		if yy < 0 || yy >= plane.Height {
			continue
		}
		for xx := x - radius; xx <= x+radius; xx++ {
			if xx < 0 || xx >= plane.Width {
				continue
			}
			sum += int(plane.Data[yy*plane.Width+xx])
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}

func yuvRound(value float64, low, high uint16) uint16 {
	if math.IsNaN(value) || value <= float64(low) {
		return low
	}
	if value >= float64(high) {
		return high
	}
	return uint16(math.Round(value))
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	index := len(buf)
	for value > 0 {
		index--
		buf[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[index:])
}

package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"example.com/multiscreen-cast/protocol"
)

type MappingConfig struct {
	MaxWorkers          int
	PerWorkerPixelBytes int64
	MaxResolution       int
	MinScale            protocol.Ratio
	MaxScale            protocol.Ratio
}

func DefaultMappingConfig() MappingConfig {
	return MappingConfig{
		MaxWorkers:          4,
		PerWorkerPixelBytes: 4 << 20,
		MaxResolution:       16384,
		MinScale:            protocol.NewRatio(1, 64),
		MaxScale:            protocol.NewRatio(64, 1),
	}
}

func (c MappingConfig) normalized() (MappingConfig, *protocol.KernelError) {
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 1
	}
	if c.PerWorkerPixelBytes <= 0 {
		return c, protocol.Errorf(protocol.CodePara, "mapping", "$.per_worker_pixel_bytes",
			"per-worker pixel buffer must be positive")
	}
	if c.MaxResolution <= 0 {
		return c, protocol.Errorf(protocol.CodePara, "mapping", "$.max_resolution",
			"max resolution must be positive")
	}
	if !c.MinScale.Positive() || !c.MaxScale.Positive() || c.MinScale.Compare(c.MaxScale) > 0 {
		return c, protocol.Errorf(protocol.CodePara, "mapping", "$.scale_range",
			"scale range must be positive and ordered")
	}
	return c, nil
}

type mapJob struct {
	index    int
	endpoint protocol.Endpoint
}

func mapWindow(ctx context.Context, displays []protocol.Endpoint, region protocol.Rect, window, topologyVersion int,
	algorithmVersion string, cfg MappingConfig) ([]ScreenResult, []QueueRecord, *protocol.KernelError) {
	cfg, cfgErr := cfg.normalized()
	if cfgErr != nil {
		return nil, nil, cfgErr
	}
	jobs := make([]mapJob, 0, len(displays))
	for _, display := range displays {
		jobs = append(jobs, mapJob{index: len(jobs), endpoint: display})
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].endpoint.ID < jobs[j].endpoint.ID
	})
	for i := range jobs {
		jobs[i].index = i
	}
	if len(jobs) == 0 {
		return []ScreenResult{}, []QueueRecord{}, nil
	}
	workers := cfg.MaxWorkers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	chunkSize := (len(jobs) + workers - 1) / workers
	results := make([]ScreenResult, len(jobs))
	errs := make([]*protocol.KernelError, len(jobs))
	queues := make([]QueueRecord, 0, len(jobs)-workers)
	for worker := 0; worker < workers; worker++ {
		start := worker * chunkSize
		end := start + chunkSize
		if end > len(jobs) {
			end = len(jobs)
		}
		for i := start + 1; i < end; i++ {
			queues = append(queues, QueueRecord{
				DisplayID: jobs[i].endpoint.ID,
				Worker:    worker,
				Reason:    fmt.Sprintf("worker %d pixel buffer busy; queued instead of dropping display", worker),
			})
		}
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * chunkSize
		end := start + chunkSize
		if end > len(jobs) {
			end = len(jobs)
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(worker, start, end int) {
			defer wg.Done()
			var scratch []byte
			defer func() {
				scratch = nil
			}()
			for i := start; i < end; i++ {
				if err := ctx.Err(); err != nil {
					errs[i] = protocol.Errorf(protocol.CodeSeg, "mapping", "$.windows",
						"mapping canceled: %v", err)
					continue
				}
				result, required, mErr := mapOne(jobs[i].endpoint, region, window, topologyVersion, algorithmVersion, cfg)
				if mErr != nil {
					errs[i] = mErr
					continue
				}
				if required > 0 {
					scratchBytes := required
					if scratchBytes > cfg.PerWorkerPixelBytes {
						scratchBytes = cfg.PerWorkerPixelBytes
						result.BufferChunks = int((required + cfg.PerWorkerPixelBytes - 1) / cfg.PerWorkerPixelBytes)
					}
					if int64(len(scratch)) < scratchBytes {
						scratch = make([]byte, scratchBytes)
					}
					result.PixelBufferBytes = scratchBytes
					_ = scratch[len(scratch)-1]
				} else {
					result.PixelBufferBytes = 0
					result.BufferChunks = 0
				}
				results[i] = result
			}
		}(worker, start, end)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, queues, err
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].DisplayID < results[j].DisplayID
	})
	sort.Slice(queues, func(i, j int) bool {
		if queues[i].DisplayID != queues[j].DisplayID {
			return queues[i].DisplayID < queues[j].DisplayID
		}
		return queues[i].Worker < queues[j].Worker
	})
	return results, queues, nil
}

func mapOne(display protocol.Endpoint, region protocol.Rect, window, topologyVersion int,
	algorithmVersion string, cfg MappingConfig) (ScreenResult, int64, *protocol.KernelError) {
	resolution := display.Capability.Resolution
	if resolution.Width <= 0 {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.capability.resolution.width",
			"resolution width must be positive"), display.ID, "capability.resolution.width")
	}
	if resolution.Height <= 0 {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.capability.resolution.height",
			"resolution height must be positive"), display.ID, "capability.resolution.height")
	}
	if resolution.Width > cfg.MaxResolution || resolution.Height > cfg.MaxResolution {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.capability.resolution",
			"resolution %dx%d exceeds limit %d", resolution.Width, resolution.Height, cfg.MaxResolution), display.ID, "capability.resolution")
	}
	scale := display.Capability.Scale
	if !scale.Positive() {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.capability.scale",
			"scale must be positive"), display.ID, "capability.scale")
	}
	if scale.Compare(cfg.MinScale) < 0 || scale.Compare(cfg.MaxScale) > 0 {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.capability.scale",
			"scale %s is outside [%s, %s]", scale, cfg.MinScale, cfg.MaxScale), display.ID, "capability.scale")
	}
	topology := display.Topology
	if topology.Width <= 0 {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.topology.width",
			"topology width must be positive"), display.ID, "topology.width")
	}
	if topology.Height <= 0 {
		return ScreenResult{}, 0, protocol.WithDisplay(protocol.Errorf(protocol.CodePara, "mapping", "$.topology.height",
			"topology height must be positive"), display.ID, "topology.height")
	}

	worldLeft := maxInt(region.X, topology.X)
	worldTop := maxInt(region.Y, topology.Y)
	worldRight := minInt(region.X+region.Width, topology.X+topology.Width)
	worldBottom := minInt(region.Y+region.Height, topology.Y+topology.Height)
	result := ScreenResult{
		DisplayID:    display.ID,
		Window:       window,
		SourceWindow: window,
		Mapping: MappingParams{
			AlgorithmVersion: algorithmVersion,
			TopologyVersion:  topologyVersion,
			Topology:         topology,
			Capability:       display.Capability,
		},
	}
	if worldRight <= worldLeft || worldBottom <= worldTop {
		result.World = protocol.Rect{}
		result.ScreenWorld = protocol.Rect{}
		result.Pixel = protocol.Rect{}
		result.SourceHash = screenSourceHash(result, region)
		return result, 0, nil
	}
	localLeft := int64(worldLeft - topology.X)
	localTop := int64(worldTop - topology.Y)
	localRight := int64(worldRight - topology.X)
	localBottom := int64(worldBottom - topology.Y)
	mode := display.Capability.CropMode
	pxLeft, pxRight := scaledEdges(localLeft, localRight, scale, mode, false)
	pyTop, pyBottom := scaledEdges(localTop, localBottom, scale, mode, true)
	pxLeft = clampInt64(pxLeft, 0, int64(resolution.Width))
	pxRight = clampInt64(pxRight, 0, int64(resolution.Width))
	pyTop = clampInt64(pyTop, 0, int64(resolution.Height))
	pyBottom = clampInt64(pyBottom, 0, int64(resolution.Height))
	if pxRight < pxLeft {
		pxRight = pxLeft
	}
	if pyBottom < pyTop {
		pyBottom = pyTop
	}
	result.Pixel = protocol.Rect{
		X:      int(pxLeft),
		Y:      int(pyTop),
		Width:  int(pxRight - pxLeft),
		Height: int(pyBottom - pyTop),
	}
	result.World = protocol.Rect{
		X:      worldLeft,
		Y:      worldTop,
		Width:  worldRight - worldLeft,
		Height: worldBottom - worldTop,
	}
	result.ScreenWorld = protocol.Rect{
		X:      int(localLeft),
		Y:      int(localTop),
		Width:  int(localRight - localLeft),
		Height: int(localBottom - localTop),
	}
	result.SourceHash = screenSourceHash(result, region)
	required := int64(result.Pixel.Width) * int64(result.Pixel.Height) * 4
	if required < 0 {
		required = 0
	}
	return result, required, nil
}

func scaledEdges(left, right int64, scale protocol.Ratio, mode protocol.CropMode, vertical bool) (int64, int64) {
	leftMode, rightMode := mode, mode
	if mode == protocol.CropOutward {
		leftMode, rightMode = protocol.CropFloor, protocol.CropCeil
	}
	if mode == protocol.CropInward {
		leftMode, rightMode = protocol.CropCeil, protocol.CropFloor
	}
	_ = vertical
	return scaleEdge(left, scale, leftMode), scaleEdge(right, scale, rightMode)
}

func scaleEdge(value int64, scale protocol.Ratio, mode protocol.CropMode) int64 {
	switch mode {
	case protocol.CropCeil:
		return ceilDiv(value*scale.Num, scale.Den)
	case protocol.CropNearest:
		return roundDiv(value*scale.Num, scale.Den)
	case protocol.CropFloor, protocol.CropOutward, protocol.CropInward:
		return floorDiv(value*scale.Num, scale.Den)
	default:
		return floorDiv(value*scale.Num, scale.Den)
	}
}

func floorDiv(n, d int64) int64 {
	q := n / d
	r := n % d
	if r != 0 && ((r < 0) != (d < 0)) {
		q--
	}
	return q
}

func ceilDiv(n, d int64) int64 {
	return -floorDiv(-n, d)
}

func roundDiv(n, d int64) int64 {
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

func clampInt64(value, low, high int64) int64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func screenSourceHash(result ScreenResult, source protocol.Rect) string {
	payload, _ := json.Marshal(struct {
		DisplayID   string        `json:"display_id"`
		Source      protocol.Rect `json:"source"`
		Pixel       protocol.Rect `json:"pixel"`
		World       protocol.Rect `json:"world"`
		ScreenWorld protocol.Rect `json:"screen_world"`
		Mapping     MappingParams `json:"mapping"`
	}{
		DisplayID:   result.DisplayID,
		Source:      source,
		Pixel:       result.Pixel,
		World:       result.World,
		ScreenWorld: result.ScreenWorld,
		Mapping:     result.Mapping,
	})
	return protocol.HashBytes(payload)
}

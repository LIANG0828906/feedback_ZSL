package ortho

import (
	"context"
	"fmt"
	"image"
	"sync"
	"time"
)

// ResampleConfig 控制分块重采样行为。
type ResampleConfig struct {
	InterpOrder  int // 0=最近邻, 1=双线性
	TileSize     int // 输出图分块边长（像素）
	Band         int // 源图取数窗口的重叠边带宽度
	Workers      int // 最大并行度，<=1 表示串行
	BufferPixels int // 全部工作线程的像素缓冲总预算
}

// WaitEvent 记录一次缓冲耗尽导致的阻塞排队。
type WaitEvent struct {
	Reason string
	Tile   string
	Waited time.Duration
}

// WaitLog 线程安全地记录资源等待事件。
type WaitLog struct {
	mu     sync.Mutex
	Events []WaitEvent
}

func (l *WaitLog) add(ev WaitEvent) {
	l.mu.Lock()
	l.Events = append(l.Events, ev)
	l.mu.Unlock()
}

// Len 返回记录的等待事件数。
func (l *WaitLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Events)
}

// RequiredBand 返回给定插值阶数所需的最小边带宽度。
func RequiredBand(interpOrder int) int {
	if interpOrder < 0 {
		interpOrder = 0
	}
	return interpOrder + 1
}

// TileMeta 描述一个分块的确定性取数窗口（调试用）。
type TileMeta struct {
	X0, Y0, X1, Y1     int // 输出图块区域
	SX0, SY0, SX1, SY1 int // 含边带的源图取数窗口（已裁剪到图内）
}

// ResampleResult 携带输出图像与各分块取数窗口。
type ResampleResult struct {
	Img   image.Image
	Tiles []TileMeta
}

// Resample 对单个阶段执行分块重采样。
// 每个输出像素的取值只依赖源图与阶段参数，与调度顺序无关，
// 因此分块并行、整图单次、串行执行的结果逐像素一致。
func Resample(ctx context.Context, src image.Image, st Stage, cfg ResampleConfig, log *WaitLog) (*ResampleResult, *Error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	if cfg.InterpOrder < 0 || cfg.InterpOrder > 1 {
		return nil, &Error{Code: EParam, Stage: st.ID(), Field: "InterpOrder", Msg: "must be 0 (nearest) or 1 (bilinear)"}
	}
	if cfg.TileSize <= 0 {
		return nil, &Error{Code: EParam, Stage: st.ID(), Field: "TileSize", Msg: "must be positive"}
	}
	if need := RequiredBand(cfg.InterpOrder); cfg.Band < need {
		return nil, &Error{Code: EBand, Stage: st.ID(), Field: "Band",
			Msg:      fmt.Sprintf("overlap band %d is insufficient for interpolation order %d", cfg.Band, cfg.InterpOrder),
			NeedBand: need}
	}
	inW := src.Bounds().Dx()
	outW, outH := st.OutSize(inW, src.Bounds().Dy())
	if outW <= 0 || outH <= 0 {
		return nil, &Error{Code: EParam, Stage: st.ID(), Field: "OutSize", Msg: "derived output size is non-positive"}
	}
	out := NewOutputLike(src, outW, outH)
	ch := Channels(src)

	type tile struct {
		x0, y0, x1, y1 int
	}
	var tiles []tile
	for y := 0; y < outH; y += cfg.TileSize {
		for x := 0; x < outW; x += cfg.TileSize {
			x1, y1 := x+cfg.TileSize, y+cfg.TileSize
			if x1 > outW {
				x1 = outW
			}
			if y1 > outH {
				y1 = outH
			}
			tiles = append(tiles, tile{x, y, x1, y1})
		}
	}

	// 像素缓冲预算：限制同时在飞的块缓冲数量。
	tilePixels := cfg.TileSize * cfg.TileSize * ch
	nbuf := 1
	if cfg.BufferPixels > 0 && tilePixels > 0 {
		nbuf = cfg.BufferPixels / tilePixels
		if nbuf < 1 {
			nbuf = 1
		}
	}
	sem := make(chan struct{}, nbuf)

	metas := make([]TileMeta, len(tiles))
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	worker := func() {
		defer wg.Done()
		for idx := range jobs {
			t := tiles[idx]
			name := fmt.Sprintf("%s/tile@%d,%d", st.ID(), t.x0, t.y0)
			// 缓冲获取：非阻塞探测失败则记录阻塞排队原因。
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			default:
				start := time.Now()
				select {
				case sem <- struct{}{}:
					if log != nil {
						log.add(WaitEvent{Reason: "pixel_buffer_exhausted", Tile: name, Waited: time.Since(start)})
					}
				case <-ctx.Done():
					return
				}
			}
			metas[idx] = runTile(src, out, st, cfg, t.x0, t.y0, t.x1, t.y1, ch)
			<-sem
		}
	}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}
feed:
	for i := range tiles {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, &Error{Code: EStage, Stage: st.ID(), Msg: "resample cancelled: " + err.Error()}
	}
	return &ResampleResult{Img: out, Tiles: metas}, nil
}

// runTile 计算单个输出块。合并规则：每块只写自己的精确矩形区域，
// 边带仅用于界定单元取数窗口，块间无重叠写入，因此合并是确定性的。
func runTile(src, dst image.Image, st Stage, cfg ResampleConfig, x0, y0, x1, y1, ch int) TileMeta {
	inW := src.Bounds().Dx()
	inH := src.Bounds().Dy()
	// 由块角点与边中点的反向映射推导源图取数窗口，再按边带扩张。
	minX, minY := 1e18, 1e18
	maxX, maxY := -1e18, -1e18
	probes := [][2]float64{
		{float64(x0), float64(y0)}, {float64(x1 - 1), float64(y0)},
		{float64(x0), float64(y1 - 1)}, {float64(x1 - 1), float64(y1 - 1)},
		{float64((x0 + x1) / 2), float64(y0)}, {float64((x0 + x1) / 2), float64(y1 - 1)},
		{float64(x0), float64((y0 + y1) / 2)}, {float64(x1 - 1), float64((y0 + y1) / 2)},
	}
	for _, p := range probes {
		sx, sy := st.Inverse(p[0], p[1])
		if sx < minX {
			minX = sx
		}
		if sx > maxX {
			maxX = sx
		}
		if sy < minY {
			minY = sy
		}
		if sy > maxY {
			maxY = sy
		}
	}
	meta := TileMeta{
		X0: x0, Y0: y0, X1: x1, Y1: y1,
		SX0: clampInt(int(minX)-cfg.Band, 0, inW-1),
		SY0: clampInt(int(minY)-cfg.Band, 0, inH-1),
		SX1: clampInt(int(maxX)+1+cfg.Band, 0, inW),
		SY1: clampInt(int(maxY)+1+cfg.Band, 0, inH),
	}
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			sx, sy := st.Inverse(float64(x), float64(y))
			for c := 0; c < ch; c++ {
				var v uint8
				if cfg.InterpOrder == 0 {
					v = sampleNearest(src, sx, sy, c)
				} else {
					v = sampleBilinear(src, sx, sy, c)
				}
				setChannel(dst, x, y, c, v)
			}
		}
	}
	return meta
}

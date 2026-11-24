package spectralrender

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
)

type Tile struct {
	Index  int `json:"index"`
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func BuildTiles(width, height, tileSize int) ([]Tile, *Error) {
	if tileSize <= 0 {
		return nil, paraError("tile_size", "must be positive")
	}
	var tiles []Tile
	index := 0
	for y := 0; y < height; y += tileSize {
		h := tileSize
		if y+h > height {
			h = height - y
		}
		for x := 0; x < width; x += tileSize {
			w := tileSize
			if x+w > width {
				w = width - x
			}
			tiles = append(tiles, Tile{Index: index, X: x, Y: y, Width: w, Height: h})
			index++
		}
	}
	return tiles, nil
}

type TileResult struct {
	Index            int       `json:"index"`
	X                int       `json:"x"`
	Y                int       `json:"y"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	LinearRGB        []float64 `json:"linear_rgb"`
	XYZAccum         []float64 `json:"xyz_accum"`
	PeakRadiance     []float64 `json:"peak_radiance"`
	Digest           string    `json:"digest"`
	SceneHash        string    `json:"scene_hash"`
	SamplingVersion  string    `json:"sampling_version"`
	ParamVersion     uint64    `json:"param_version"`
	MaterialCacheKey string    `json:"material_cache_key"`
}

type LinearFrame struct {
	Width            int
	Height           int
	LinearRGB        []float64
	XYZ              []float64
	PeakRadiance     []float64
	ParamVersion     uint64
	SamplingVersion  string
	MaterialCacheKey string
	SceneHash        string
}

type Frame struct {
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	LinearRGB        []float64 `json:"linear_rgb"`
	XYZ              []float64 `json:"xyz"`
	PeakRadiance     []float64 `json:"peak_radiance"`
	EncodedRGB       []byte    `json:"encoded_rgb"`
	EncodedSHA256    string    `json:"encoded_sha256"`
	ParamVersion     uint64    `json:"param_version"`
	SamplingVersion  string    `json:"sampling_version"`
	MaterialCacheKey string    `json:"material_cache_key"`
	SceneHash        string    `json:"scene_hash"`
	Exposure         float64   `json:"exposure"`
	ToneCurve        ToneCurve `json:"tone_curve"`
	Gamma            float64   `json:"gamma"`
}

type SchedulerEvent struct {
	Tile   int    `json:"tile"`
	Reason string `json:"reason"`
	Waited bool   `json:"waited"`
}

type TileFailure struct {
	Tile  int    `json:"tile"`
	Code  string `json:"code"`
	Error string `json:"error"`
}

type RenderReport struct {
	ParamVersion      uint64                `json:"param_version"`
	SamplingVersion   string                `json:"sampling_version"`
	MaterialCacheKey  string                `json:"material_cache_key"`
	SceneHash         string                `json:"scene_hash"`
	TileCount         int                   `json:"tile_count"`
	ParallelSlots     int                   `json:"parallel_slots"`
	MemorySlots       int                   `json:"memory_slots"`
	TileBytesEstimate int64                 `json:"tile_bytes_estimate"`
	ResumedTiles      []int                 `json:"resumed_tiles"`
	RenderedTiles     []int                 `json:"rendered_tiles"`
	FailedTiles       []TileFailure         `json:"failed_tiles,omitempty"`
	SchedulerEvents   []SchedulerEvent      `json:"scheduler_events,omitempty"`
	Invalidation      *SnapshotInvalidation `json:"snapshot_invalidation,omitempty"`
	Quantization      QuantizationReport    `json:"quantization"`
	FinalImageDigest  string                `json:"final_image_digest,omitempty"`
}

type RenderOptions struct {
	Store           *CheckpointStore
	Context         context.Context
	FailTile        func(Tile) error
	OnTileCommitted func(int)
}

func ComputeTileDigest(t *TileResult) string {
	h := sha256.New()
	writeInt := func(v int64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v))
		h.Write(b[:])
	}
	writeFloat := func(v float64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
		h.Write(b[:])
	}
	writeInt(int64(t.Index))
	writeInt(int64(t.X))
	writeInt(int64(t.Y))
	writeInt(int64(t.Width))
	writeInt(int64(t.Height))
	writeInt(int64(t.ParamVersion))
	h.Write([]byte(t.SceneHash))
	h.Write([]byte(t.SamplingVersion))
	h.Write([]byte(t.MaterialCacheKey))
	for _, v := range t.LinearRGB {
		writeFloat(v)
	}
	for _, v := range t.XYZAccum {
		writeFloat(v)
	}
	for _, v := range t.PeakRadiance {
		writeFloat(v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateTileResult(t *TileResult) *Error {
	if t.Width <= 0 || t.Height <= 0 {
		return &Error{Code: ErrCheckpoint, Tile: t.Index, Message: "tile dimensions must be positive"}
	}
	pixels := t.Width * t.Height
	if len(t.LinearRGB) != pixels*3 || len(t.XYZAccum) != pixels*3 || len(t.PeakRadiance) != pixels {
		return &Error{Code: ErrCheckpoint, Tile: t.Index, Message: "tile buffer lengths do not match dimensions"}
	}
	for _, v := range t.LinearRGB {
		if !isFinite(v) {
			return &Error{Code: ErrCheckpoint, Tile: t.Index, Message: "linear RGB contains a non-finite value"}
		}
	}
	for _, v := range t.XYZAccum {
		if !isFinite(v) {
			return &Error{Code: ErrCheckpoint, Tile: t.Index, Message: "XYZ accumulation contains a non-finite value"}
		}
	}
	for _, v := range t.PeakRadiance {
		if !isFinite(v) {
			return &Error{Code: ErrCheckpoint, Tile: t.Index, Message: "peak radiance contains a non-finite value"}
		}
	}
	if t.Digest == "" {
		t.Digest = ComputeTileDigest(t)
	}
	if ComputeTileDigest(t) != t.Digest {
		return &Error{Code: ErrCheckpoint, Tile: t.Index, Message: "tile digest mismatch"}
	}
	return nil
}

type pixelHit struct {
	ok        bool
	mesh      int
	depth     float64
	normal    [3]float64
	albedo    RGB
	roughness float64
	metallic  float64
}

func samplePixel(scene *Scene, quant *QuantizedInput, x, y int) pixelHit {
	ndcX := (float64(x)+0.5)/float64(scene.Width)*2 - 1
	ndcY := 1 - (float64(y)+0.5)/float64(scene.Height)*2
	best := pixelHit{depth: math.Inf(1)}
	for mi := range scene.Meshes {
		mesh := &scene.Meshes[mi]
		for _, tri := range mesh.Triangles {
			a := mesh.Vertices[tri[0]]
			b := mesh.Vertices[tri[1]]
			c := mesh.Vertices[tri[2]]
			ax, ay := a.Position[0], a.Position[1]
			bx, by := b.Position[0], b.Position[1]
			cx, cy := c.Position[0], c.Position[1]
			den := (by-cy)*(ax-cx) + (cx-bx)*(ay-cy)
			if math.Abs(den) < 1e-15 {
				continue
			}
			l0 := ((by-cy)*(ndcX-cx) + (cx-bx)*(ndcY-cy)) / den
			l1 := ((cy-ay)*(ndcX-cx) + (ax-cx)*(ndcY-cy)) / den
			l2 := 1 - l0 - l1
			const eps = -1e-10
			if l0 < eps || l1 < eps || l2 < eps {
				continue
			}
			depth := l0*a.Position[2] + l1*b.Position[2] + l2*c.Position[2]
			if depth >= best.depth-1e-12 {
				continue
			}
			qa := quant.Vertices[mi][tri[0]]
			qb := quant.Vertices[mi][tri[1]]
			qc := quant.Vertices[mi][tri[2]]
			var albedo RGB
			for ch := 0; ch < 3; ch++ {
				albedo[ch] = l0*qa.Albedo[ch] + l1*qb.Albedo[ch] + l2*qc.Albedo[ch]
			}
			var n [3]float64
			for ch := 0; ch < 3; ch++ {
				n[ch] = l0*a.Normal[ch] + l1*b.Normal[ch] + l2*c.Normal[ch]
			}
			best = pixelHit{
				ok: true, mesh: mi, depth: depth, normal: normalized(n), albedo: albedo,
				roughness: l0*qa.Roughness + l1*qb.Roughness + l2*qc.Roughness,
				metallic:  l0*qa.Metallic + l1*qb.Metallic + l2*qc.Metallic,
			}
		}
	}
	return best
}

func renderTile(scene *Scene, engine *SpectralEngine, params RenderParams, tile Tile) (*TileResult, error) {
	pixels := tile.Width * tile.Height
	res := &TileResult{
		Index: tile.Index, X: tile.X, Y: tile.Y, Width: tile.Width, Height: tile.Height,
		LinearRGB:        make([]float64, pixels*3),
		XYZAccum:         make([]float64, pixels*3),
		PeakRadiance:     make([]float64, pixels),
		SceneHash:        HashScene(scene),
		SamplingVersion:  engine.samplingVersion,
		ParamVersion:     params.Version,
		MaterialCacheKey: engine.materialKey,
	}
	for ly := 0; ly < tile.Height; ly++ {
		for lx := 0; lx < tile.Width; lx++ {
			gx := tile.X + lx
			gy := tile.Y + ly
			idx := ly*tile.Width + lx
			hit := samplePixel(scene, engine.quantized, gx, gy)
			if !hit.ok {
				res.LinearRGB[idx*3] = scene.BackgroundRGB[0]
				res.LinearRGB[idx*3+1] = scene.BackgroundRGB[1]
				res.LinearRGB[idx*3+2] = scene.BackgroundRGB[2]
				continue
			}
			var xyz RGB
			for li := range scene.Lights {
				xyz = xyz.Add(engine.shade(hit.mesh, li, hit.normal))
			}
			direct := sanitizeXYZ(xyz)
			attenuation := params.IndirectWeight *
				(0.35 + 0.65*(1-hit.roughness)) *
				(0.35 + 0.65*hit.metallic)
			bounce := direct
			for b := 0; b < params.Bounces; b++ {
				bounce = bounce.Mul(attenuation)
				xyz = xyz.Add(bounce)
			}
			rgb := engine.WhiteNormalizedRGB(xyz)
			specTint := RGB{0.80, 0.88, 1.0}
			tint := hit.albedo.Mul(1 - hit.metallic).Add(specTint.Mul(hit.metallic))
			gain := (1-hit.metallic)*(0.85+0.15*(1-hit.roughness)) +
				hit.metallic*(0.55+0.45*(1-hit.roughness))
			rgb = rgb.MulComponent(tint).Mul(gain)
			if !rgb.IsFinite() {
				return nil, gamutError(Pixel{gx, gy}, "RGB", "non-finite target gamut value")
			}
			if rgb.Min() < -1e-9 {
				ch := "R"
				if rgb[1] < rgb[0] {
					ch = "G"
				}
				if rgb[2] < rgb[1] && rgb[2] < rgb[0] {
					ch = "B"
				}
				return nil, gamutError(Pixel{gx, gy}, ch, "linear RGB is outside the target gamut")
			}
			res.LinearRGB[idx*3] = rgb[0]
			res.LinearRGB[idx*3+1] = rgb[1]
			res.LinearRGB[idx*3+2] = rgb[2]
			res.XYZAccum[idx*3] = xyz[0]
			res.XYZAccum[idx*3+1] = xyz[1]
			res.XYZAccum[idx*3+2] = xyz[2]
			res.PeakRadiance[idx] = xyz[1]
		}
	}
	res.Digest = ComputeTileDigest(res)
	if err := validateTileResult(res); err != nil {
		return nil, err
	}
	return res, nil
}

func assembleLinearFrame(tiles []Tile, results []*TileResult, width, height int, params RenderParams) (*LinearFrame, *Error) {
	frame := &LinearFrame{
		Width: width, Height: height,
		LinearRGB:    make([]float64, width*height*3),
		XYZ:          make([]float64, width*height*3),
		PeakRadiance: make([]float64, width*height),
		ParamVersion: params.Version,
	}
	for i, tile := range tiles {
		r := results[i]
		if r == nil {
			return nil, &Error{Code: ErrState, Tile: tile.Index, Message: "missing tile result"}
		}
		if r.Width != tile.Width || r.Height != tile.Height {
			return nil, &Error{Code: ErrState, Tile: tile.Index, Message: "tile dimensions changed"}
		}
		if frame.SamplingVersion == "" {
			frame.SamplingVersion = r.SamplingVersion
			frame.MaterialCacheKey = r.MaterialCacheKey
			frame.SceneHash = r.SceneHash
		}
		for ly := 0; ly < tile.Height; ly++ {
			for lx := 0; lx < tile.Width; lx++ {
				src := ly*tile.Width + lx
				dst := (tile.Y+ly)*width + tile.X + lx
				copy(frame.LinearRGB[dst*3:dst*3+3], r.LinearRGB[src*3:src*3+3])
				copy(frame.XYZ[dst*3:dst*3+3], r.XYZAccum[src*3:src*3+3])
				frame.PeakRadiance[dst] = r.PeakRadiance[src]
			}
		}
	}
	return frame, nil
}

func toneMapChannel(v, exposure float64, curve ToneCurve) float64 {
	x := v * exposure
	switch curve {
	case ToneLinear:
		return clamp01(x)
	case ToneReinhard:
		if x <= 0 {
			return 0
		}
		return x / (1 + x)
	case ToneACES:
		if x <= 0 {
			return 0
		}
		const a, b, c, d, e = 2.51, 0.03, 2.43, 0.59, 0.14
		return clamp01((x * (a*x + b)) / (x*(c*x+d) + e))
	default:
		return clamp01(x)
	}
}

func EncodeDisplay(linear *LinearFrame, params RenderParams) (*Frame, *Error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if err := RequireSameVersion(linear.ParamVersion, params.Version, "param_version"); err != nil {
		return nil, err
	}
	if len(linear.LinearRGB) != linear.Width*linear.Height*3 {
		return nil, newError(ErrState, "linear_rgb", "linear frame buffer length mismatch")
	}
	f := &Frame{
		Width: linear.Width, Height: linear.Height,
		LinearRGB:        append([]float64(nil), linear.LinearRGB...),
		XYZ:              append([]float64(nil), linear.XYZ...),
		PeakRadiance:     append([]float64(nil), linear.PeakRadiance...),
		EncodedRGB:       make([]byte, linear.Width*linear.Height*3),
		ParamVersion:     params.Version,
		SamplingVersion:  linear.SamplingVersion,
		MaterialCacheKey: linear.MaterialCacheKey,
		SceneHash:        linear.SceneHash,
		Exposure:         params.Exposure, ToneCurve: params.ToneCurve, Gamma: params.Gamma,
	}
	for i := 0; i < linear.Width*linear.Height; i++ {
		for ch := 0; ch < 3; ch++ {
			v := linear.LinearRGB[i*3+ch]
			if !isFinite(v) {
				return nil, gamutError(Pixel{i % linear.Width, i / linear.Width}, string("RGB"[ch]), "non-finite linear RGB")
			}
			if v < -1e-9 {
				return nil, gamutError(Pixel{i % linear.Width, i / linear.Width}, string("RGB"[ch]),
					"linear RGB is outside the target gamut")
			}
			mapped := toneMapChannel(v, params.Exposure, params.ToneCurve)
			encoded := math.Pow(clamp01(mapped), 1/params.Gamma)
			f.EncodedRGB[i*3+ch] = byte(math.Round(clamp01(encoded) * 255))
		}
	}
	sum := sha256.Sum256(f.EncodedRGB)
	f.EncodedSHA256 = hex.EncodeToString(sum[:])
	return f, nil
}

func tileByteEstimate(tileSize, sampleCount int) int64 {
	pixels := int64(tileSize * tileSize)
	return pixels*(3*8+3*8+8) + int64(sampleCount*3*8) + 8192
}

func Render(scene *Scene, sceneHash string, params RenderParams, opts RenderOptions) (*Frame, *RenderReport, error) {
	if err := params.Validate(); err != nil {
		return nil, nil, err
	}
	if sceneHash == "" {
		sceneHash = HashScene(scene)
	}
	engine, err := NewSpectralEngine(scene, params)
	if err != nil {
		return nil, nil, err
	}
	tiles, err := BuildTiles(scene.Width, scene.Height, params.TileSize)
	if err != nil {
		return nil, nil, err
	}
	report := &RenderReport{
		ParamVersion:     params.Version,
		SamplingVersion:  engine.samplingVersion,
		MaterialCacheKey: engine.materialKey,
		SceneHash:        sceneHash,
		TileCount:        len(tiles),
		Quantization:     engine.quantized.Report,
	}
	results := make([]*TileResult, len(tiles))
	missing := make([]Tile, 0, len(tiles))

	if opts.Store != nil {
		meta := SnapshotMeta{
			SchemaVersion:    1,
			AlgorithmVersion: AlgorithmVersion,
			SceneHash:        sceneHash,
			SamplingVersion:  engine.samplingVersion,
			ParamVersion:     params.Version,
			MaterialCacheKey: engine.materialKey,
			Params:           params,
			Width:            scene.Width,
			Height:           scene.Height,
			TileSize:         params.TileSize,
			TilesX:           (scene.Width + params.TileSize - 1) / params.TileSize,
			TilesY:           (scene.Height + params.TileSize - 1) / params.TileSize,
		}
		resume, beginErr := opts.Store.Begin(meta)
		if beginErr != nil {
			return nil, report, beginErr
		}
		if resume.Invalidation != nil {
			report.Invalidation = resume.Invalidation
		}
		for idx, t := range resume.Tiles {
			if idx >= 0 && idx < len(tiles) {
				results[idx] = t
				report.ResumedTiles = append(report.ResumedTiles, idx)
			}
		}
	} else {
		for i := range results {
			results[i] = nil
		}
	}
	for i := range tiles {
		if results[i] == nil {
			missing = append(missing, tiles[i])
		} else if err := validateTileResult(results[i]); err != nil {
			return nil, report, err
		}
	}

	tileBytes := tileByteEstimate(params.TileSize, params.SampleCount)
	report.TileBytesEstimate = tileBytes
	report.ParallelSlots = params.MaxParallel
	memSlots := int(params.MemoryBudgetBytes / tileBytes)
	if memSlots < 1 {
		memSlots = 1
		report.SchedulerEvents = append(report.SchedulerEvents, SchedulerEvent{
			Tile: -1, Waited: true,
			Reason: fmt.Sprintf("memory_budget_bytes=%d is below one tile estimate (%d); serialized to one tile", params.MemoryBudgetBytes, tileBytes),
		})
	}
	if memSlots < params.MaxParallel {
		report.SchedulerEvents = append(report.SchedulerEvents, SchedulerEvent{
			Tile: -1, Waited: true,
			Reason: fmt.Sprintf("memory budget allows %d concurrent tiles; max_parallel=%d is queued accordingly", memSlots, params.MaxParallel),
		})
	}
	report.MemorySlots = memSlots
	if len(missing) > memSlots {
		report.SchedulerEvents = append(report.SchedulerEvents, SchedulerEvent{
			Tile: missing[memSlots].Index, Waited: true,
			Reason: "memory budget exhausted while acquiring a tile buffer; waiting for a released buffer",
		})
	}
	if len(missing) > params.MaxParallel {
		report.SchedulerEvents = append(report.SchedulerEvents, SchedulerEvent{
			Tile: missing[params.MaxParallel].Index, Waited: true,
			Reason: "max_parallel limit reached; tile queued until a worker slot is released",
		})
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var eventMu sync.Mutex
	appendEvent := func(ev SchedulerEvent) {
		eventMu.Lock()
		report.SchedulerEvents = append(report.SchedulerEvents, ev)
		eventMu.Unlock()
	}
	parallelSem := make(chan struct{}, params.MaxParallel)
	memSem := make(chan struct{}, memSlots)
	type outcome struct {
		tile   Tile
		result *TileResult
		err    error
	}
	outcomes := make(chan outcome, len(missing))
	var wg sync.WaitGroup
	for _, tile := range missing {
		wg.Add(1)
		go func(t Tile) {
			defer wg.Done()
			select {
			case memSem <- struct{}{}:
			default:
				appendEvent(SchedulerEvent{Tile: t.Index, Reason: "memory budget exhausted; blocking", Waited: true})
				select {
				case memSem <- struct{}{}:
				case <-ctx.Done():
					outcomes <- outcome{tile: t, err: ctx.Err()}
					return
				}
			}
			defer func() { <-memSem }()
			select {
			case parallelSem <- struct{}{}:
			default:
				appendEvent(SchedulerEvent{Tile: t.Index, Reason: "max_parallel limit reached; blocking", Waited: true})
				select {
				case parallelSem <- struct{}{}:
				case <-ctx.Done():
					outcomes <- outcome{tile: t, err: ctx.Err()}
					return
				}
			}
			defer func() { <-parallelSem }()
			if cerr := ctx.Err(); cerr != nil {
				outcomes <- outcome{tile: t, err: cerr}
				return
			}
			if opts.FailTile != nil {
				if ferr := opts.FailTile(t); ferr != nil {
					outcomes <- outcome{tile: t, err: ferr}
					return
				}
			}
			r, rerr := renderTile(scene, engine, params, t)
			outcomes <- outcome{tile: t, result: r, err: rerr}
		}(tile)
	}
	go func() {
		wg.Wait()
		close(outcomes)
	}()

	pending := make(map[int]outcome)
	next := 0
	stopCommit := false
	failure := (*TileFailure)(nil)
	var failureErr error
	for out := range outcomes {
		pending[out.tile.Index] = out
		for {
			if stopCommit || next >= len(tiles) {
				break
			}
			if results[next] != nil {
				next++
				continue
			}
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if ctx.Err() != nil {
				stopCommit = true
				break
			}
			if p.err != nil {
				code := string(ErrRender)
				if ce := CodeOf(p.err); ce != "" {
					code = string(ce)
				}
				failure = &TileFailure{Tile: next, Code: code, Error: p.err.Error()}
				failureErr = p.err
				report.FailedTiles = append(report.FailedTiles, *failure)
				stopCommit = true
				break
			}
			if verr := validateTileResult(p.result); verr != nil {
				failure = &TileFailure{Tile: next, Code: string(verr.Code), Error: verr.Error()}
				failureErr = verr
				report.FailedTiles = append(report.FailedTiles, *failure)
				stopCommit = true
				break
			}
			if opts.Store != nil {
				if cerr := opts.Store.Commit(p.result); cerr != nil {
					failure = &TileFailure{Tile: next, Code: string(CodeOf(cerr)), Error: cerr.Error()}
					failureErr = cerr
					report.FailedTiles = append(report.FailedTiles, *failure)
					stopCommit = true
					break
				}
			}
			results[next] = p.result
			report.RenderedTiles = append(report.RenderedTiles, next)
			next++
			if opts.OnTileCommitted != nil {
				opts.OnTileCommitted(next - 1)
			}
			if ctx.Err() != nil {
				stopCommit = true
			}
		}
	}
	if ctx.Err() != nil {
		return nil, report, &Error{Code: ErrCancel, Message: "render canceled; only atomically committed tiles are resumable", Err: ctx.Err()}
	}
	if failure != nil {
		if e := AsError(failureErr); e != nil {
			return nil, report, e
		}
		return nil, report, &Error{Code: ErrRender, Tile: failure.Tile, Message: failure.Error, Err: failureErr}
	}
	for i := range results {
		if results[i] == nil {
			return nil, report, &Error{Code: ErrState, Tile: i, Message: "render finished without a result for this tile"}
		}
	}
	linear, lerr := assembleLinearFrame(tiles, results, scene.Width, scene.Height, params)
	if lerr != nil {
		return nil, report, lerr
	}
	frame, ferr := EncodeDisplay(linear, params)
	if ferr != nil {
		return nil, report, ferr
	}
	report.FinalImageDigest = frame.EncodedSHA256
	if opts.Store != nil {
		if cerr := opts.Store.WriteFinalImage(frame); cerr != nil {
			return nil, report, cerr
		}
	}
	return frame, report, nil
}

func RenderPreview(scene *Scene, sceneHash string, store *ParamStore, opts RenderOptions) (*Frame, *RenderReport, error) {
	params := store.Snapshot()
	return Render(scene, sceneHash, params, opts)
}

func (s *ExactSession) Render(scene *Scene, sceneHash string, opts RenderOptions) (*Frame, *RenderReport, error) {
	return Render(scene, sceneHash, s.params, opts)
}

func IsGamutError(err error) bool { return CodeOf(err) == ErrGamut }

// Package render implements a deterministic differentiable ray caster for the
// canonical sphere/plane/rectangle-light scene and deterministic target images.
package render

import (
	"fmt"
	"math"
	"sync"

	"diffrender/errs"
	"diffrender/scene"
)

const epsilonT = 1e-7

const (
	MaxWorkerLimit     = 64
	MaxBlockPixelLimit = 65536
	MaxQueueLimit      = 65536
)

// Config bounds worker-pool concurrency and each worker's pixel buffer.
type Config struct {
	MaxWorkers    int
	BlockPixels   int
	QueueCapacity int
	Retries       int
	// Fault is a test hook. It can fail one block without affecting others.
	Fault func(view, start, end, attempt int) error
}

// DefaultConfig returns a bounded parallel configuration.
func DefaultConfig() Config {
	return Config{MaxWorkers: 4, BlockPixels: 128, QueueCapacity: 8, Retries: 1}
}

func normalizeConfig(c Config) (Config, error) {
	if c.MaxWorkers == 0 {
		c.MaxWorkers = 1
	}
	if c.MaxWorkers < 0 {
		return c, errs.New(errs.CodeConfig, "<render>", "maxWorkers", "must be >= 0")
	}
	if c.MaxWorkers > MaxWorkerLimit {
		return c, errs.New(errs.CodeConfig, "<render>", "maxWorkers", "must be <= %d", MaxWorkerLimit)
	}
	if c.BlockPixels == 0 {
		c.BlockPixels = 128
	}
	if c.BlockPixels < 0 {
		return c, errs.New(errs.CodeConfig, "<render>", "blockPixels", "must be >= 0")
	}
	if c.BlockPixels > MaxBlockPixelLimit {
		return c, errs.New(errs.CodeConfig, "<render>", "blockPixels", "must be <= %d", MaxBlockPixelLimit)
	}
	if c.QueueCapacity < 0 {
		return c, errs.New(errs.CodeConfig, "<render>", "queueCapacity", "must be >= 0")
	}
	if c.QueueCapacity > MaxQueueLimit {
		return c, errs.New(errs.CodeConfig, "<render>", "queueCapacity", "must be <= %d", MaxQueueLimit)
	}
	if c.Retries < 0 {
		return c, errs.New(errs.CodeConfig, "<render>", "retries", "must be >= 0")
	}
	return c, nil
}

// BlockResult is one independently retryable pixel block.
type BlockResult struct {
	View     int
	Index    int
	Start    int
	End      int
	Attempts int
	Loss     float64
	Gradient []float64
	Pixels   []float64
	Err      error
}

// Batch contains ordered block results and their ordered reduction.
type Batch struct {
	ParamIDs []string
	Blocks   []BlockResult
	Loss     float64
	Grad     []float64
	Images   []Image
	Metrics  Metrics

	ctx       *batchContext
	aggregate error
}

// Metrics records the enforced resource bounds.
type Metrics struct {
	Workers               int
	QueueCapacity         int
	BlockPixels           int
	PerWorkerBufferValues int
	BlockCount            int
	FailedBlockCount      int
}

type blockSpec struct {
	view  int
	index int
	start int
	end   int
}

type batchContext struct {
	scene       *scene.Scene
	specs       []ParamSpec
	adScene     *adScene
	targets     []Image
	blocks      []blockSpec
	results     []BlockResult
	views       []scene.Camera
	totalValues int
	totalPixels int
	paramCount  int
	cfg         Config
}

// Compute renders all cameras and, when targets are supplied, computes the
// photometric least-squares loss and gradient. Block failures are isolated and
// remain available for RetryFailed.
func Compute(s *scene.Scene, targets []Image, cfg Config) (*Batch, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := scene.Validate(s, "<render>"); err != nil {
		return nil, err
	}
	specs, err := ParameterSpecs(s)
	if err != nil {
		return nil, err
	}
	if targets != nil {
		if len(targets) != len(s.Cameras) {
			return nil, errs.New(errs.CodeDecode, "<render>", "targets", "expected %d target images, got %d", len(s.Cameras), len(targets))
		}
		for i := range targets {
			if err := validateTargetForCamera(targets[i], s.Cameras[i], fmt.Sprintf("<target:%d>", i)); err != nil {
				return nil, err
			}
		}
	}
	ctx := &batchContext{
		scene:   s,
		specs:   specs,
		targets: targets,
		views:   s.Cameras,
		cfg:     cfg,
	}
	ctx.adScene = buildADScene(s, specs, len(specs))
	ctx.paramCount = len(specs)
	for view, cam := range s.Cameras {
		ctx.totalPixels += cam.Width * cam.Height
		if targets != nil {
			ctx.totalValues += cam.Width * cam.Height * 3
		}
		for start := 0; start < cam.Width*cam.Height; start += cfg.BlockPixels {
			end := start + cfg.BlockPixels
			if end > cam.Width*cam.Height {
				end = cam.Width * cam.Height
			}
			ctx.blocks = append(ctx.blocks, blockSpec{view: view, index: len(ctx.blocks), start: start, end: end})
		}
	}
	ctx.results = make([]BlockResult, len(ctx.blocks))
	b := &Batch{
		ParamIDs: make([]string, len(specs)),
		Blocks:   ctx.results,
		Images:   make([]Image, len(s.Cameras)),
		Metrics: Metrics{
			Workers:               cfg.MaxWorkers,
			QueueCapacity:         cfg.QueueCapacity,
			BlockPixels:           cfg.BlockPixels,
			PerWorkerBufferValues: cfg.BlockPixels * 3,
			BlockCount:            len(ctx.blocks),
		},
		ctx: ctx,
	}
	for i, spec := range specs {
		b.ParamIDs[i] = spec.ID
	}
	for i, cam := range s.Cameras {
		b.Images[i] = Image{CameraID: cam.ID, Width: cam.Width, Height: cam.Height, Pixels: make([]float64, cam.Width*cam.Height*3)}
	}
	b.runIndices(allIndices(len(ctx.blocks)), cfg)
	for retry := 0; retry < cfg.Retries; retry++ {
		failed := b.failedIndices()
		if len(failed) == 0 {
			break
		}
		b.runIndices(failed, cfg)
	}
	err = b.Aggregate()
	return b, err
}

// Render renders all cameras without a loss target.
func Render(s *scene.Scene, cfg Config) ([]Image, error) {
	b, err := Compute(s, nil, cfg)
	if err != nil {
		return nil, err
	}
	return b.Images, nil
}

// RetryFailed recomputes only failed blocks with a replacement configuration.
// Completed blocks are never recomputed, preserving their values exactly.
func (b *Batch) RetryFailed(cfg Config) error {
	if b == nil || b.ctx == nil {
		return errs.New(errs.CodeRender, "<render>", "batch", "batch is not initialized")
	}
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	failed := b.failedIndices()
	if len(failed) == 0 {
		return nil
	}
	b.ctx.cfg = cfg
	b.aggregate = nil
	b.runIndices(failed, cfg)
	return b.Aggregate()
}

// Aggregate performs the deterministic, block-ordered reduction.
func (b *Batch) Aggregate() error {
	if b == nil || b.ctx == nil {
		return errs.New(errs.CodeRender, "<render>", "batch", "batch is not initialized")
	}
	// Reset assembled images so retries do not accumulate stale values.
	for i := range b.Images {
		for j := range b.Images[i].Pixels {
			b.Images[i].Pixels[j] = 0
		}
	}
	failed := b.failedIndices()
	b.Metrics.FailedBlockCount = len(failed)
	for _, idx := range failed {
		r := b.Blocks[idx]
		if r.Err != nil {
			b.aggregate = errs.New(errs.CodeRender, "<render>", fmt.Sprintf("blocks[%d:%d]", r.View, r.Index), "block failed: %v", r.Err)
			return b.aggregate
		}
	}
	for i := range b.Blocks {
		block := b.Blocks[i]
		base := block.Start * 3
		copy(b.Images[block.View].Pixels[base:base+len(block.Pixels)], block.Pixels)
	}
	if b.ctx.targets == nil {
		b.Loss = 0
		b.Grad = nil
		b.aggregate = nil
		return nil
	}
	grad := make([]float64, b.ctx.paramCount)
	var loss float64
	for i := range b.Blocks {
		loss += b.Blocks[i].Loss
		if len(b.Blocks[i].Gradient) != len(grad) {
			b.aggregate = errs.New(errs.CodeGradient, "<render>", "gradient", "block %d has %d values, expected %d", i, len(b.Blocks[i].Gradient), len(grad))
			return b.aggregate
		}
		for p := range grad {
			grad[p] += b.Blocks[i].Gradient[p]
		}
	}
	denom := float64(b.ctx.totalValues)
	if denom == 0 {
		b.aggregate = errs.New(errs.CodeRender, "<render>", "targets", "no target values")
		return b.aggregate
	}
	b.Loss = loss / denom
	for p := range grad {
		grad[p] /= denom
	}
	b.Grad = grad
	b.aggregate = nil
	return nil
}

func (b *Batch) failedIndices() []int {
	out := make([]int, 0)
	for i := range b.Blocks {
		if b.Blocks[i].Err != nil {
			out = append(out, i)
		}
	}
	return out
}

func allIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func (b *Batch) runIndices(indices []int, cfg Config) {
	if len(indices) == 0 {
		return
	}
	workers := cfg.MaxWorkers
	if workers > len(indices) {
		workers = len(indices)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int, cfg.QueueCapacity)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scratch := make([]float64, cfg.BlockPixels*3)
			for idx := range jobs {
				b.safeEvaluateBlock(idx, scratch, cfg)
			}
		}()
	}
	for _, idx := range indices {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()
}

func (b *Batch) safeEvaluateBlock(idx int, scratch []float64, cfg Config) {
	prevAttempts := b.Blocks[idx].Attempts
	defer func() {
		if recovered := recover(); recovered != nil {
			block := b.ctx.blocks[idx]
			b.Blocks[idx] = BlockResult{
				View: block.view, Index: idx, Start: block.start, End: block.end,
				Attempts: prevAttempts + 1, Err: fmt.Errorf("panic: %v", recovered),
			}
		}
	}()
	b.evaluateBlock(idx, scratch, cfg)
}

func (b *Batch) evaluateBlock(idx int, scratch []float64, cfg Config) {
	block := b.ctx.blocks[idx]
	prevAttempts := b.Blocks[idx].Attempts
	result := BlockResult{
		View:     block.view,
		Index:    idx,
		Start:    block.start,
		End:      block.end,
		Attempts: prevAttempts + 1,
	}
	n := block.end - block.start
	if n <= 0 || n*3 > len(scratch) || n > cfg.BlockPixels {
		result.Err = errs.New(errs.CodeConfig, "<render>", "blockPixels", "block requires %d pixels but buffer holds %d", n, len(scratch)/3)
		b.Blocks[idx] = result
		return
	}
	if cfg.Fault != nil {
		if err := cfg.Fault(block.view, block.start, block.end, prevAttempts); err != nil {
			result.Err = err
			b.Blocks[idx] = result
			return
		}
	}
	cam := b.ctx.views[block.view]
	width := cam.Width
	var grad []float64
	if b.ctx.targets != nil {
		grad = make([]float64, b.ctx.paramCount)
	}
	target := []float64(nil)
	if b.ctx.targets != nil {
		target = b.ctx.targets[block.view].Pixels
	}
	for pixel := block.start; pixel < block.end; pixel++ {
		local := pixel - block.start
		x, y := pixel%width, pixel/width
		rgb := tracePixel(b.ctx.adScene, cam, x, y, b.ctx.paramCount)
		out := local * 3
		scratch[out], scratch[out+1], scratch[out+2] = rgb.x.v, rgb.y.v, rgb.z.v
		if target != nil {
			base := pixel * 3
			d0 := rgb.x.v - target[base]
			d1 := rgb.y.v - target[base+1]
			d2 := rgb.z.v - target[base+2]
			result.Loss += d0*d0 + d1*d1 + d2*d2
			for p := 0; p < b.ctx.paramCount; p++ {
				grad[p] += 2*d0*rgb.x.d[p] + 2*d1*rgb.y.d[p] + 2*d2*rgb.z.d[p]
			}
		}
	}
	result.Pixels = append([]float64(nil), scratch[:n*3]...)
	result.Gradient = grad
	b.Blocks[idx] = result
}

func validateTargetForCamera(im Image, cam scene.Camera, file string) error {
	if err := im.Validate(file); err != nil {
		return err
	}
	if im.CameraID != cam.ID {
		return errs.New(errs.CodeDecode, file, "target.cameraId", "target camera %q does not match scene camera %q", im.CameraID, cam.ID)
	}
	if im.Width != cam.Width || im.Height != cam.Height {
		return errs.New(errs.CodeDecode, file, "target.resolution", "target is %dx%d, camera expects %dx%d", im.Width, im.Height, cam.Width, cam.Height)
	}
	return nil
}

type adSphere struct {
	center adVec
	radius ad
	albedo adVec
}

type adPlane struct {
	normal adVec
	offset ad
	albedo adVec
}

type adLight struct {
	center     adVec
	normal     adVec
	up         adVec
	halfWidth  float64
	halfHeight float64
	power      ad
	color      adVec
}

type adScene struct {
	spheres        []adSphere
	planes         []adPlane
	lights         []adLight
	shadowSoftness float64
	toneMapping    string
	n              int
}

func buildADScene(s *scene.Scene, specs []ParamSpec, n int) *adScene {
	val := func(v float64, idx int) ad {
		if idx >= 0 {
			return adVar(v, idx, n)
		}
		return adConst(v, n)
	}
	vec := func(v scene.Vec3, ix, iy, iz int) adVec {
		return adVec{val(v.X, ix), val(v.Y, iy), val(v.Z, iz)}
	}
	find := func(match func(ParamSpec) bool) int {
		return parameterIndex(specs, match)
	}
	out := &adScene{n: n, shadowSoftness: s.Settings.ShadowSoftness, toneMapping: s.Settings.ToneMapping}
	for i, sp := range s.Spheres {
		ix := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamCenterX })
		iy := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamCenterY })
		iz := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamCenterZ })
		ir := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamRadius })
		iar := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamAlbedoR })
		iag := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamAlbedoG })
		iab := find(func(p ParamSpec) bool { return p.Sphere == i && p.Kind == ParamAlbedoB })
		out.spheres = append(out.spheres, adSphere{
			center: vec(sp.Center, ix, iy, iz),
			radius: val(sp.Radius, ir),
			albedo: vec(sp.Albedo, iar, iag, iab),
		})
	}
	for i, pl := range s.Planes {
		iar := find(func(p ParamSpec) bool { return p.Plane == i && p.Kind == ParamAlbedoR })
		iag := find(func(p ParamSpec) bool { return p.Plane == i && p.Kind == ParamAlbedoG })
		iab := find(func(p ParamSpec) bool { return p.Plane == i && p.Kind == ParamAlbedoB })
		out.planes = append(out.planes, adPlane{
			normal: vec(pl.Normal, -1, -1, -1),
			offset: adConst(pl.Offset, n),
			albedo: vec(pl.Albedo, iar, iag, iab),
		})
	}
	for i, li := range s.Lights {
		ip := find(func(p ParamSpec) bool { return p.Light == i && p.Kind == ParamPower })
		out.lights = append(out.lights, adLight{
			center: vec(li.Center, -1, -1, -1), normal: vec(li.Normal, -1, -1, -1), up: vec(li.Up, -1, -1, -1),
			halfWidth: li.HalfWidth, halfHeight: li.HalfHeight,
			power: val(li.RadiantPowerW, ip),
			color: vec(li.Color, -1, -1, -1),
		})
	}
	return out
}

type hitKind uint8

const (
	hitNone hitKind = iota
	hitSphere
	hitPlane
)

type hitRecord struct {
	kind   hitKind
	index  int
	t      ad
	point  adVec
	normal adVec
	albedo adVec
}

func tracePixel(as *adScene, cam scene.Camera, px, py, n int) adVec {
	forward, right, up, err := scene.CameraBasis(cam)
	if err != nil {
		return adVec{adConst(0, n), adConst(0, n), adConst(0, n)}
	}
	tanHalf := math.Tan(cam.FovYRad / 2)
	aspect := float64(cam.Width) / float64(cam.Height)
	sx := (2*(float64(px)+0.5)/float64(cam.Width) - 1) * tanHalf * aspect
	sy := (1 - 2*(float64(py)+0.5)/float64(cam.Height)) * tanHalf
	dirWorld := forward.Add(right.Mul(sx)).Add(up.Mul(sy))
	dirWorld, _ = dirWorld.Normalized()
	rayO := adVec{adConst(cam.Eye.X, n), adConst(cam.Eye.Y, n), adConst(cam.Eye.Z, n)}
	rayD := adVec{adConst(dirWorld.X, n), adConst(dirWorld.Y, n), adConst(dirWorld.Z, n)}
	best := adConst(math.Inf(1), n)
	var hit hitRecord
	for i := range as.spheres {
		sp := &as.spheres[i]
		oc := adVecSub(rayO, sp.center)
		b := adDot(oc, rayD)
		c := adSub(adDot(oc, oc), adMul(sp.radius, sp.radius))
		disc := adSub(adMul(b, b), c)
		if disc.v <= 0 {
			continue
		}
		sq := adSqrt(disc)
		negB := adNeg(b)
		t := adSub(negB, sq)
		if t.v <= epsilonT {
			t = adAdd(negB, sq)
		}
		if t.v <= epsilonT || t.v >= best.v {
			continue
		}
		p := adVecAdd(rayO, adVecMulAd(rayD, t))
		normal := adNormalize(adVecSub(p, sp.center))
		best = t
		hit = hitRecord{kind: hitSphere, index: i, t: t, point: p, normal: normal, albedo: sp.albedo}
	}
	for i := range as.planes {
		pl := &as.planes[i]
		den := adDot(pl.normal, rayD)
		if math.Abs(den.v) <= 1e-12 {
			continue
		}
		t := adDiv(adSub(pl.offset, adDot(pl.normal, rayO)), den)
		if t.v <= epsilonT || t.v >= best.v {
			continue
		}
		p := adVecAdd(rayO, adVecMulAd(rayD, t))
		best = t
		hit = hitRecord{kind: hitPlane, index: i, t: t, point: p, normal: pl.normal, albedo: pl.albedo}
	}
	if hit.kind == hitNone {
		zero := adConst(0, n)
		return adVec{zero, zero, zero}
	}
	return shade(as, &hit)
}

func shade(as *adScene, hit *hitRecord) adVec {
	n := as.n
	zero := adConst(0, n)
	out := adVec{zero, zero, zero}
	for i := range as.lights {
		li := &as.lights[i]
		toLight := adVecSub(li.center, hit.point)
		dist2 := adDot(toLight, toLight)
		if dist2.v <= 1e-12 {
			continue
		}
		l := adNormalize(toLight)
		ndotl := adMax(adDot(hit.normal, l), zero)
		if ndotl.v <= 0 {
			continue
		}
		visibility := adConst(1, n)
		lightSoftness := 0.03 * (li.halfWidth + li.halfHeight)
		for si := range as.spheres {
			if hit.kind == hitSphere && hit.index == si {
				continue
			}
			visibility = adMul(visibility, softVisibility(as, hit.point, l, &as.spheres[si], lightSoftness))
		}
		denom := adAdd(adScale(dist2, 4*math.Pi), adConst(0.25, n))
		energy := adDiv(li.power, denom)
		lambert := adMul(ndotl, adMul(energy, visibility))
		out.x = adAdd(out.x, adMul(adMul(hit.albedo.x, li.color.x), lambert))
		out.y = adAdd(out.y, adMul(adMul(hit.albedo.y, li.color.y), lambert))
		out.z = adAdd(out.z, adMul(adMul(hit.albedo.z, li.color.z), lambert))
	}
	out = adVecScale(out, 0.75)
	if as.toneMapping == "srgb" {
		out.x = adPow(adClamp(out.x, 0, 1), 1/2.2)
		out.y = adPow(adClamp(out.y, 0, 1), 1/2.2)
		out.z = adPow(adClamp(out.z, 0, 1), 1/2.2)
	}
	return out
}

func softVisibility(as *adScene, point, lightDir adVec, occluder *adSphere, lightSoftness float64) ad {
	n := as.n
	one := adConst(1, n)
	t := adDot(adVecSub(occluder.center, point), lightDir)
	if t.v <= 0 {
		return one
	}
	closest := adVecAdd(point, adVecMulAd(lightDir, t))
	distance := adLength(adVecSub(occluder.center, closest))
	soft := as.shadowSoftness + lightSoftness
	if soft < 0.02 {
		soft = 0.02
	}
	delta := adSub(distance, occluder.radius)
	x := adDiv(adAdd(delta, adConst(soft, n)), adConst(2*soft, n))
	return adSmoothstep(0, 1, x)
}

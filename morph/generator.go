package morph

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type GenerateOptions struct {
	ID                  string
	Seed                int64
	Paths               int
	Keyframes           int
	Segments            int
	Samples             int
	Scale               float64
	Quantum             float64
	ReferenceGrid       int
	CurveSamplesPerPath int
	CoordExtent         float64
	PaddingPixels       int
	MaxFramePixels      int
}

func DefaultGenerateOptions() GenerateOptions {
	return GenerateOptions{
		ID: "demo", Seed: 42, Paths: 8, Keyframes: 3, Segments: 7, Samples: 9,
		Scale: 12, Quantum: 0.001, ReferenceGrid: 96, CoordExtent: 48,
		CurveSamplesPerPath: 18,
		PaddingPixels:       4, MaxFramePixels: 2048,
	}
}

type xorshift64 struct{ state uint64 }

func newRNG(seed int64) *xorshift64 {
	x := uint64(seed) ^ 0x9e3779b97f4a7c15
	if x == 0 {
		x = 0xa5a5a5a5a5a5a5a5
	}
	return &xorshift64{state: x}
}

func (r *xorshift64) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *xorshift64) float() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

func (r *xorshift64) rangeF(a, b float64) float64 { return a + (b-a)*r.float() }
func (r *xorshift64) rangeI(a, b int) int {
	if b <= a {
		return a
	}
	return a + int(r.next()%uint64(b-a+1))
}

// GenerateScene creates a deterministic animated contour scene and an
// independent truth manifest. All random choices come from an in-package
// xorshift generator so runs are reproducible across Go patch releases.
func GenerateScene(opts GenerateOptions) (*Scene, *Truth, error) {
	if opts.ID == "" {
		opts.ID = fmt.Sprintf("scene-%d", opts.Seed)
	}
	if opts.Paths < 1 || opts.Keyframes < 2 || opts.Segments < 4 || opts.Samples < 3 {
		return nil, nil, withField(newError(CodeParam, "invalid generator dimensions"), "generate")
	}
	if opts.Scale <= 0 || opts.Quantum <= 0 || opts.CoordExtent <= 0 {
		return nil, nil, withField(newError(CodeParam, "invalid generator scale/quantum"), "generate")
	}
	if opts.ReferenceGrid <= 0 {
		opts.ReferenceGrid = 96
	}
	if opts.CurveSamplesPerPath <= 0 {
		opts.CurveSamplesPerPath = 18
	}
	rng := newRNG(opts.Seed)
	scene := &Scene{
		Format: SceneFormat, Version: 1, ID: opts.ID, Seed: opts.Seed,
		Quantum: opts.Quantum, CoordMin: -opts.CoordExtent, CoordMax: opts.CoordExtent,
		Render: RenderDefaults{
			Scale: opts.Scale, PaddingPixels: opts.PaddingPixels, MaxFramePixels: opts.MaxFramePixels,
		},
		SampleTimes: sampleTimes(opts.Keyframes, opts.Samples),
		Paths:       make([]Path, 0, opts.Paths),
	}
	keyTimes := keyTimes(opts.Keyframes)
	// Keep a deterministic margin so deformed control points also remain inside
	// the declared quantization range at the largest stress levels.
	fieldExtent := opts.CoordExtent * 0.90
	cols, rows, cell, radius := layout(opts.Paths, fieldExtent)
	for i := 0; i < opts.Paths; i++ {
		cx := -fieldExtent + cell*0.5 + float64(i%cols)*cell
		cy := -fieldExtent + cell*0.5 + float64(i/cols)*cell
		if rows > 0 && i/cols >= rows {
			cx = math.Mod(cx, fieldExtent*1.8) - fieldExtent*0.9
		}
		phase := rng.rangeF(0, math.Pi*2)
		spin := rng.rangeF(-0.7, 0.7)
		rad := radius * rng.rangeF(0.72, 1.0)
		kfs := make([]Keyframe, opts.Keyframes)
		for k := 0; k < opts.Keyframes; k++ {
			u := 0.0
			if opts.Keyframes > 1 {
				u = float64(k) / float64(opts.Keyframes-1)
			}
			segs := makeContour(rng, cx, cy, rad, phase, spin, u, opts.Segments, opts.Quantum)
			kfs[k] = Keyframe{Time: keyTimes[k], Segments: segs}
		}
		p := Path{
			ID:        fmt.Sprintf("p%04d", i),
			Opacity:   quantizeRange(rng.rangeF(0.55, 1.0), 0.01),
			BlendMode: []string{"over", "over", "multiply", "screen", "add"}[(i+int(opts.Seed))%5],
			FillRule:  []string{"nonzero", "evenodd"}[(i+3)%2],
			Keyframes: kfs,
		}
		switch {
		case i%7 == 0:
			p.Fill = &FillStyle{Color: randomColor(rng)}
			p.Stroke = &StrokeStyle{
				Color: randomColor(rng),
				Width: quantizeRange(rng.rangeF(0.45, 1.25), opts.Quantum*2),
				Join:  []string{"round", "bevel", "miter"}[(i/7)%3], MiterLimit: 4,
			}
		case i%3 == 0:
			p.Stroke = &StrokeStyle{
				Color: randomColor(rng),
				Width: quantizeRange(rng.rangeF(0.35, 1.1), opts.Quantum*2),
				Join:  []string{"round", "bevel", "miter"}[i%3], MiterLimit: 4,
			}
		default:
			p.Fill = &FillStyle{Color: randomColor(rng)}
		}
		scene.Paths = append(scene.Paths, p)
	}
	if err := ValidateScene(scene); err != nil {
		return nil, nil, err
	}
	truth, err := buildTruth(scene, opts.ReferenceGrid, opts.CurveSamplesPerPath)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateTruth(scene, truth); err != nil {
		return nil, nil, err
	}
	return scene, truth, nil
}

func keyTimes(k int) []float64 {
	out := make([]float64, k)
	for i := range out {
		out[i] = float64(i) / float64(k-1)
	}
	return out
}

func sampleTimes(keyframes, samples int) []float64 {
	set := map[float64]bool{0: true, 1: true}
	for i := 0; i < samples; i++ {
		set[float64(i)/float64(samples-1)] = true
	}
	kt := keyTimes(keyframes)
	for i := 0; i+1 < len(kt); i++ {
		set[(kt[i]+kt[i+1])*0.5] = true
	}
	out := make([]float64, 0, len(set)+samples)
	for t := range set {
		out = append(out, t)
	}
	sortFloats(out)
	return out
}

func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func layout(paths int, extent float64) (cols, rows int, cell, radius float64) {
	cols = int(math.Ceil(math.Sqrt(float64(paths))))
	if cols < 1 {
		cols = 1
	}
	rows = (paths + cols - 1) / cols
	cell = (2 * extent) / float64(cols)
	if rows > cols {
		cell = (2 * extent) / float64(rows)
	}
	radius = math.Max(0.15, cell*0.30)
	return
}

func randomColor(rng *xorshift64) [3]float64 {
	base := rng.rangeF(0.35, 0.95)
	return [3]float64{
		quantizeRange(math.Min(1, base+rng.rangeF(-0.18, 0.18)), 0.001),
		quantizeRange(math.Min(1, base+rng.rangeF(-0.18, 0.18)), 0.001),
		quantizeRange(math.Min(1, base+rng.rangeF(-0.18, 0.18)), 0.001),
	}
}

func quantizeRange(v, q float64) float64 {
	if q <= 0 {
		return v
	}
	return math.Max(0, math.Min(1, Quantize(v, q)))
}

func makeContour(rng *xorshift64, cx, cy, radius, phase, spin, u float64, n int, quantum float64) []Segment {
	verts := make([]Vec2, n)
	for i := 0; i < n; i++ {
		a := phase + 2*math.Pi*float64(i)/float64(n)
		rad := radius * (0.88 + 0.20*math.Sin(3*a+phase) + 0.08*math.Cos(5*a-phase))
		rad *= 1 + 0.18*math.Sin(2*math.Pi*u*1.7+float64(i)*0.61)
		b := a + spin*u
		vx := cx + math.Cos(b)*rad + 0.35*math.Sin(2*math.Pi*u+phase)
		vy := cy + math.Sin(b)*rad + 0.35*math.Cos(2*math.Pi*u*1.3-phase)
		verts[i] = QuantizeVec(Vec2{vx, vy}, quantum)
	}
	segs := make([]Segment, n)
	for i := 0; i < n; i++ {
		a, b := verts[i], verts[(i+1)%n]
		ab := b.Sub(a)
		mid := a.Add(b).Mul(0.5)
		normal := Vec2{-ab.Y, ab.X}.Normalized()
		// Subtle per-segment deformation makes segment-level animation visible.
		wave := math.Sin(float64(i)*0.83 + phase + 2*math.Pi*u)
		offset := ab.Len() * (0.10 + 0.08*wave)
		if i%2 == 0 {
			ctrl := QuantizeVec(mid.Add(normal.Mul(offset)), quantum)
			segs[i] = Segment{Kind: "Q", P0: a, P1: ctrl, P2: b}
		} else {
			c1 := QuantizeVec(a.Add(ab.Mul(0.33)).Add(normal.Mul(offset)).Add(ab.Mul(0.04*wave)), quantum)
			c2 := QuantizeVec(a.Add(ab.Mul(0.67)).Add(normal.Mul(offset*0.75)).Sub(ab.Mul(0.04*wave)), quantum)
			end := b
			segs[i] = Segment{Kind: "C", P0: a, P1: c1, P2: c2, P3: &end}
		}
	}
	return segs
}

func buildTruth(scene *Scene, refGrid, curveSamplesPerPath int) (*Truth, error) {
	truth := &Truth{
		Format: TruthFormat, Version: 1, SceneID: scene.ID, Seed: scene.Seed,
		Algorithm: "analytic-bezier-v1", Render: scene.Render,
		Samples: make([]TruthSample, 0, len(scene.SampleTimes)),
	}
	keyframeSet := map[float64]bool{}
	for _, p := range scene.Paths {
		for _, k := range p.Keyframes {
			keyframeSet[k.Time] = true
		}
	}
	for si, t := range scene.SampleTimes {
		ts := TruthSample{Index: si, Time: t}
		ts.MidpointLower, ts.MidpointUpper = midpointInterval(scene.Paths[0].Keyframes, t)
		ts.MidpointOfKeyframes = ts.MidpointUpper > ts.MidpointLower && !keyframeSet[t]
		frameBBox := EmptyBBox()
		paths := make([]PathTruth, 0, len(scene.Paths))
		for pi := range scene.Paths {
			p := &scene.Paths[pi]
			segs, _, err := InterpolatePath(p, t, "linear")
			if err != nil {
				return nil, err
			}
			b := SegmentsBBox(segs)
			r := 0.0
			if p.Stroke != nil {
				r = p.Stroke.Width * 0.5
			}
			sb := b
			if r > 0 {
				sb = b.Expand(r)
			}
			frameBBox = frameBBox.Union(sb)
			pt := PathTruth{
				PathID: p.ID, ContourArea: math.Abs(ContourArea(segs)),
				GeometryBBox: sb, Closed: true,
			}
			if p.Fill != nil {
				pt.FillArea = pt.ContourArea
			}
			if p.Stroke != nil {
				dense, _ := FlattenSegments(segs, scene.Quantum*2)
				pt.StrokeArea = p.Stroke.Width * PolylineLength(dense)
			}
			for segmentIndex := range segs {
				if len(pt.CurveSamples) >= curveSamplesPerPath {
					break
				}
				for _, q := range []float64{0.25, 0.5, 0.75} {
					pt.CurveSamples = append(pt.CurveSamples, CurveSample{
						Segment: segmentIndex, T: q, Point: EvalSegment(segs[segmentIndex], q),
					})
				}
			}
			pt.ClosureCheckpoints = []Vec2{segs[0].P0, segs[len(segs)-1].End(), segs[len(segs)/2].P0}
			pt.ReferenceCoverageArea = referenceCoverageArea(p, segs, refGrid)
			paths = append(paths, pt)
		}
		fg, err := DeriveFrameGeometry(frameBBox, scene.Render.Scale, scene.Render.PaddingPixels, scene.Render.MaxFramePixels)
		if err != nil {
			return nil, err
		}
		ts.FrameBBox = frameBBox
		ts.RenderBBox = fg.WorldBBox
		ts.Width, ts.Height = fg.Width, fg.Height
		ts.Paths = paths
		truth.Samples = append(truth.Samples, ts)
	}
	return truth, nil
}

func midpointInterval(kfs []Keyframe, t float64) (float64, float64) {
	for i := 0; i+1 < len(kfs); i++ {
		lo, hi := kfs[i].Time, kfs[i+1].Time
		mid := (lo + hi) * 0.5
		if math.Abs(t-mid) <= 1e-9 {
			return lo, hi
		}
	}
	return 0, 0
}

// referenceCoverageArea is a deliberately independent dense deterministic
// oracle. Analytic contour area remains available in Truth for fill validation.
func referenceCoverageArea(p *Path, segs []Segment, cells int) float64 {
	r := 0.0
	if p.Stroke != nil {
		r = p.Stroke.Width * 0.5
	}
	b := SegmentsBBox(segs)
	if r > 0 {
		b = b.Expand(r)
	}
	if b.IsEmpty() {
		return 0
	}
	pts, _ := FlattenSegments(segs, 0.002)
	if len(pts) < 3 {
		return 0
	}
	cells = maxInt(24, cells)
	nx := cells
	aspect := b.Height() / math.Max(b.Width(), 1e-12)
	ny := maxInt(24, int(float64(cells)*aspect))
	inside := 0
	for iy := 0; iy < ny; iy++ {
		y := b.Min.Y + (float64(iy)+0.5)*b.Height()/float64(ny)
		for ix := 0; ix < nx; ix++ {
			x := b.Min.X + (float64(ix)+0.5)*b.Width()/float64(nx)
			q := Vec2{x, y}
			hit := false
			if p.Fill != nil {
				hit = pointInPolyline(q, pts)
			}
			if !hit && r > 0 {
				hit = distanceToPolyline(q, pts) <= r
			}
			if hit {
				inside++
			}
		}
	}
	return float64(inside) / float64(nx*ny) * b.Width() * b.Height()
}

func pointInPolyline(p Vec2, pts []Vec2) bool {
	inside := false
	for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
		a, b := pts[i], pts[j]
		if (a.Y > p.Y) != (b.Y > p.Y) {
			x := (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y) + a.X
			if p.X < x {
				inside = !inside
			}
		}
	}
	return inside
}

func distanceToPolyline(p Vec2, pts []Vec2) float64 {
	best := math.Inf(1)
	for i := 0; i+1 < len(pts); i++ {
		d := distancePointSegment(p, pts[i], pts[i+1])
		if d < best {
			best = d
		}
	}
	return best
}

func distancePointSegment(p, a, b Vec2) float64 {
	ab := b.Sub(a)
	den := ab.Dot(ab)
	if den < 1e-18 {
		return p.Distance(a)
	}
	t := p.Sub(a).Dot(ab) / den
	t = math.Max(0, math.Min(1, t))
	return p.Distance(a.Add(ab.Mul(t)))
}

func WriteGenerated(outDir string, scene *Scene, truth *Truth) (string, string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", "", err
	}
	scenePath := filepath.Join(outDir, scene.ID+".scene.json")
	truthPath := filepath.Join(outDir, scene.ID+".truth.json")
	if err := WriteJSONAtomic(scenePath, scene); err != nil {
		return "", "", err
	}
	if err := WriteJSONAtomic(truthPath, truth); err != nil {
		return "", "", err
	}
	return scenePath, truthPath, nil
}

func WriteGeneratedCompressed(outDir string, scene *Scene, truth *Truth) (string, string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", "", err
	}
	scenePath := filepath.Join(outDir, scene.ID+".scene.json.gz")
	truthPath := filepath.Join(outDir, scene.ID+".truth.json.gz")
	if err := WriteJSONGzipAtomic(scenePath, scene); err != nil {
		return "", "", err
	}
	if err := WriteJSONGzipAtomic(truthPath, truth); err != nil {
		return "", "", err
	}
	return scenePath, truthPath, nil
}

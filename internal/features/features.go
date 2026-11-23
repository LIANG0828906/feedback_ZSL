package features

import (
	"math"
	"math/bits"
	"sort"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
)

type KeyPoint struct {
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Scale    float64 `json:"scale"`
	Angle    float64 `json:"angle"`
	Response float64 `json:"response"`
}

type Feature struct {
	Point KeyPoint
	Desc  [32]byte
}

type Options struct {
	MaxFeatures       int
	ScaleLevels       int
	ResponseThreshold float64
}

type MatchPair struct {
	Source   KeyPoint `json:"source"`
	Target   KeyPoint `json:"target"`
	Distance int      `json:"distance"`
	Ratio    float64  `json:"ratio"`
}

type grayImage struct {
	W, H int
	Pix  []float32
}

type briefPair struct {
	A, B int
}

var briefPattern []briefPair

func init() {
	// A deterministic, centre-biased pattern. The exact distribution is not
	// cryptographically important; repeatability is.
	state := uint64(0x9e3779b97f4a7c15)
	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state
	}
	briefPattern = make([]briefPair, 256)
	coord := func() int {
		v := int(next()%25) - 12
		if v == 0 {
			v = 1
		}
		return v
	}
	for i := range briefPattern {
		briefPattern[i] = briefPair{A: (coord()+12)*32 + (coord() + 12), B: (coord()+12)*32 + (coord() + 12)}
	}
}

func Extract(gray []float32, w, h int, opt Options) ([]Feature, error) {
	if opt.MaxFeatures < 32 || opt.MaxFeatures > 200000 {
		return nil, errs.Field(errs.EPara, "feature budget out of range [32,200000]", "feature_budget")
	}
	if opt.ScaleLevels < 1 || opt.ScaleLevels > 7 {
		return nil, errs.Field(errs.EPara, "scale levels out of range [1,7]", "scale_levels")
	}
	if opt.ResponseThreshold <= 0 || opt.ResponseThreshold >= 1 {
		return nil, errs.Field(errs.EPara, "response threshold out of range (0,1)", "response_threshold")
	}
	base := &grayImage{W: w, H: h, Pix: gray}
	levels := make([]*grayImage, opt.ScaleLevels)
	levels[0] = base
	for i := 1; i < opt.ScaleLevels; i++ {
		p := levels[i-1]
		nw, nh := p.W/2, p.H/2
		if nw < 24 || nh < 24 {
			levels = levels[:i]
			break
		}
		levels[i] = downsample(p)
	}
	perLevel := int(math.Ceil(float64(opt.MaxFeatures) / float64(len(levels)) * 1.35))
	var all []Feature
	for li, im := range levels {
		cands := harris(im, opt.ResponseThreshold)
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].Response == cands[j].Response {
				if cands[i].Y == cands[j].Y {
					return cands[i].X < cands[j].X
				}
				return cands[i].Y < cands[j].Y
			}
			return cands[i].Response > cands[j].Response
		})
		blocked := make(map[int64]bool)
		taken := 0
		factor := math.Pow(2, float64(li))
		for _, kp := range cands {
			if taken >= perLevel {
				break
			}
			gx, gy := int(kp.X)/5, int(kp.Y)/5
			key := int64(gy)<<32 | int64(gx)
			if blocked[key] {
				continue
			}
			blocked[key] = true
			angle := orientation(im, kp.X, kp.Y, 8)
			desc := descriptor(im, kp.X, kp.Y, angle, 12)
			all = append(all, Feature{
				Point: KeyPoint{X: kp.X * factor, Y: kp.Y * factor, Scale: factor, Angle: angle, Response: kp.Response},
				Desc:  desc,
			})
			taken++
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Point.Response == all[j].Point.Response {
			if all[i].Point.Y == all[j].Point.Y {
				return all[i].Point.X < all[j].Point.X
			}
			return all[i].Point.Y < all[j].Point.Y
		}
		return all[i].Point.Response > all[j].Point.Response
	})
	if len(all) > opt.MaxFeatures {
		all = all[:opt.MaxFeatures]
	}
	return all, nil
}

func harris(im *grayImage, threshold float64) []KeyPoint {
	w, h := im.W, im.H
	if w < 12 || h < 12 {
		return nil
	}
	response := make([]float32, w*h)
	maxResp := float32(0)
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			gx := (im.Pix[i-w+1] + 2*im.Pix[i+1] + im.Pix[i+w+1]) - (im.Pix[i-w-1] + 2*im.Pix[i-1] + im.Pix[i+w-1])
			gy := (im.Pix[i+w-1] + 2*im.Pix[i+w] + im.Pix[i+w+1]) - (im.Pix[i-w-1] + 2*im.Pix[i-w] + im.Pix[i-w+1])
			response[i] = gx*gx + gy*gy
		}
	}
	// Box-filter the structure energy to suppress isolated compression noise.
	smoothed := make([]float32, w*h)
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			var s float32
			for yy := -1; yy <= 1; yy++ {
				for xx := -1; xx <= 1; xx++ {
					s += response[(y+yy)*w+x+xx]
				}
			}
			smoothed[y*w+x] = s / 9
			if smoothed[y*w+x] > maxResp {
				maxResp = smoothed[y*w+x]
			}
		}
	}
	if maxResp <= 0 {
		return nil
	}
	thr := maxResp * float32(threshold)
	out := make([]KeyPoint, 0, 4096)
	for y := 2; y < h-2; y++ {
		for x := 2; x < w-2; x++ {
			v := smoothed[y*w+x]
			if v < thr {
				continue
			}
			maxN := v
			for yy := -1; yy <= 1; yy++ {
				for xx := -1; xx <= 1; xx++ {
					if xx == 0 && yy == 0 {
						continue
					}
					if smoothed[(y+yy)*w+x+xx] > maxN {
						maxN = smoothed[(y+yy)*w+x+xx]
					}
				}
			}
			if maxN == v {
				out = append(out, KeyPoint{X: float64(x), Y: float64(y), Response: float64(v)})
			}
		}
	}
	return out
}

func orientation(im *grayImage, x, y, radius float64) float64 {
	cx, cy := int(math.Round(x)), int(math.Round(y))
	m10, m01 := 0.0, 0.0
	r := int(math.Ceil(radius))
	for yy := -r; yy <= r; yy++ {
		for xx := -r; xx <= r; xx++ {
			px, py := cx+xx, cy+yy
			if px < 0 || py < 0 || px >= im.W || py >= im.H {
				continue
			}
			if float64(xx*xx+yy*yy) > radius*radius {
				continue
			}
			v := float64(im.Pix[py*im.W+px])
			m10 += float64(xx) * v
			m01 += float64(yy) * v
		}
	}
	return math.Atan2(m01, m10)
}

func descriptor(im *grayImage, x, y, angle, radius float64) [32]byte {
	var out [32]byte
	c, s := math.Cos(angle), math.Sin(angle)
	sample := func(p briefPair) float64 {
		ax := float64(p.A/32-12) / 12 * radius
		ay := float64(p.A%32-12) / 12 * radius
		bx := float64(p.B/32-12) / 12 * radius
		by := float64(p.B%32-12) / 12 * radius
		rax := c*ax - s*ay
		ray := s*ax + c*ay
		rbx := c*bx - s*by
		rby := s*bx + c*by
		return bilinear(im, x+rax, y+ray) - bilinear(im, x+rbx, y+rby)
	}
	for i, p := range briefPattern {
		if sample(p) > 0 {
			out[i/8] |= 1 << uint(i%8)
		}
	}
	return out
}

func bilinear(im *grayImage, x, y float64) float64 {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > float64(im.W-1) {
		x = float64(im.W - 1)
	}
	if y > float64(im.H-1) {
		y = float64(im.H - 1)
	}
	x0, y0 := int(x), int(y)
	x1, y1 := x0+1, y0+1
	if x1 >= im.W {
		x1 = im.W - 1
	}
	if y1 >= im.H {
		y1 = im.H - 1
	}
	fx, fy := x-float64(x0), y-float64(y0)
	v0 := float64(im.Pix[y0*im.W+x0])*(1-fx) + float64(im.Pix[y0*im.W+x1])*fx
	v1 := float64(im.Pix[y1*im.W+x0])*(1-fx) + float64(im.Pix[y1*im.W+x1])*fx
	return v0*(1-fy) + v1*fy
}

func downsample(im *grayImage) *grayImage {
	w, h := im.W/2, im.H/2
	out := &grayImage{W: w, H: h, Pix: make([]float32, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var s float32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					s += im.Pix[(y*2+dy)*im.W+x*2+dx]
				}
			}
			out.Pix[y*w+x] = s / 4
		}
	}
	return out
}

func Distance(a, b [32]byte) int {
	d := 0
	for i := 0; i < 32; i += 8 {
		x := uint64(a[i]) | uint64(a[i+1])<<8 | uint64(a[i+2])<<16 | uint64(a[i+3])<<24 |
			uint64(a[i+4])<<32 | uint64(a[i+5])<<40 | uint64(a[i+6])<<48 | uint64(a[i+7])<<56
		y := uint64(b[i]) | uint64(b[i+1])<<8 | uint64(b[i+2])<<16 | uint64(b[i+3])<<24 |
			uint64(b[i+4])<<32 | uint64(b[i+5])<<40 | uint64(b[i+6])<<48 | uint64(b[i+7])<<56
		d += bits.OnesCount64(x ^ y)
	}
	return d
}

// Match performs bidirectional nearest-neighbour checks and a ratio test in
// both directions. Scenes with strongly repetitive motifs are further guarded
// by requiring unique nearest neighbours.
func Match(a, b []Feature, ratio float64) []MatchPair {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	nnAB := make([]int, len(a))
	secondAB := make([]int, len(a))
	distAB := make([]int, len(a))
	for i := range a {
		best, second := math.MaxInt32, math.MaxInt32
		bi, si := -1, -1
		for j := range b {
			d := Distance(a[i].Desc, b[j].Desc)
			if d < best {
				second, si = best, bi
				best, bi = d, j
			} else if d < second {
				second, si = d, j
			}
		}
		nnAB[i], secondAB[i], distAB[i] = bi, si, best
	}
	nnBA := make([]int, len(b))
	distBA := make([]int, len(b))
	for j := range b {
		best, bi := math.MaxInt32, -1
		for i := range a {
			d := Distance(a[i].Desc, b[j].Desc)
			if d < best {
				best, bi = d, i
			}
		}
		nnBA[j], distBA[j] = bi, best
	}
	out := make([]MatchPair, 0)
	for i := range a {
		j := nnAB[i]
		if j < 0 || nnBA[j] != i || secondAB[i] == -1 {
			continue
		}
		d1 := float64(distAB[i])
		d2 := float64(Distance(a[i].Desc, b[secondAB[i]].Desc))
		if d2 <= 0 {
			d2 = 1
		}
		r := d1 / d2
		if r <= ratio {
			out = append(out, MatchPair{
				Source: a[i].Point, Target: b[j].Point,
				Distance: distAB[i], Ratio: r,
			})
		}
	}
	return out
}

func ToPoints(matches []MatchPair) []model.PointPair {
	out := make([]model.PointPair, len(matches))
	for i, m := range matches {
		out[i] = model.PointPair{Source: model.Point{X: m.Source.X, Y: m.Source.Y}, Target: model.Point{X: m.Target.X, Y: m.Target.Y}}
	}
	return out
}

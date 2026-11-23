package terrainbake

import (
	"fmt"
	"math"
	"sort"
)

// GenerateProject creates a fully deterministic, self-contained terrain
// project from a seed. Four tiles with different resolutions, slopes and
// shared-edge constraints exercise the scheduler without external assets.
func GenerateProject(seed int64) (*Project, error) {
	materials := []Material{
		{ID: "rock", NormalScale: 1.15, RotationDeg: 12},
		{ID: "grass", NormalScale: 0.72, RotationDeg: -18},
		{ID: "snow", NormalScale: 0.48, RotationDeg: 31},
	}
	specs := []struct {
		id           string
		w, h, ox, oy int
		freq         float64
		amp          float64
		base         float64
		desc         string
	}{
		{"tl", 16, 12, 0, 0, 1.35, 3.4, 1.0, "wide ridged foothills"},
		{"tr", 11, 12, 16, 0, 2.15, 5.1, 1.7, "steep compact ridge"},
		{"bl", 16, 8, 0, 12, 0.85, 2.2, 0.4, "gentle low basin"},
		{"br", 11, 8, 16, 12, 2.85, 4.4, 2.1, "high-frequency snow cap"},
	}
	p := &Project{
		SchemaVersion: SchemaVersion,
		Name:          fmt.Sprintf("deterministic-terrain-%d", seed),
		Seed:          seed,
		Materials:     materials,
	}
	for _, s := range specs {
		b := Block{
			ID: s.id, Width: s.w, Height: s.h, OriginX: s.ox, OriginY: s.oy,
			Materials:   []string{"rock", "grass", "snow"},
			Description: s.desc,
		}
		b.Elevation = make([]float64, s.w*s.h)
		b.Weights = make([]int, s.w*s.h*len(b.Materials))
		for y := 0; y < s.h; y++ {
			for x := 0; x < s.w; x++ {
				wx := float64(s.ox + x)
				wy := float64(s.oy + y)
				noise := valueNoise(wx*s.freq, wy*s.freq, seed+int64(s.ox*17+s.oy*31), 4)
				ridge := 1 - math.Abs(valueNoise(wx*s.freq*0.53+19, wy*s.freq*0.53-7, seed+91, 3))
				h := s.base + s.amp*(0.72*noise+0.28*ridge)
				h += 0.16 * math.Sin((wx+float64(seed%11))*0.39) * math.Cos((wy-float64(seed%7))*0.31)
				b.Elevation[y*s.w+x] = h
				slope := math.Hypot(dxAt(&b, x, y), dyAt(&b, x, y))
				quantizeWeights(b.Weights[(y*s.w+x)*len(b.Materials):], h, slope, s.base)
			}
		}
		p.Blocks = append(p.Blocks, b)
	}
	p.Seams = []Seam{
		{ID: "tl-tr", BlockA: "tl", BlockB: "tr", EdgeA: EdgeRight, EdgeB: EdgeLeft, MaxAngleDeg: 5, MaxIterations: 8},
		{ID: "tl-bl", BlockA: "tl", BlockB: "bl", EdgeA: EdgeBottom, EdgeB: EdgeTop, MaxAngleDeg: 6, MaxIterations: 8},
		{ID: "tr-br", BlockA: "tr", BlockB: "br", EdgeA: EdgeBottom, EdgeB: EdgeTop, MaxAngleDeg: 4, MaxIterations: 9},
		{ID: "bl-br", BlockA: "bl", BlockB: "br", EdgeA: EdgeRight, EdgeB: EdgeLeft, MaxAngleDeg: 7, MaxIterations: 7},
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if err := populateTruth(p, DefaultBakeParams()); err != nil {
		return nil, err
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func populateTruth(p *Project, params BakeParams) error {
	raw := make(map[string]*RawBlock, len(p.Blocks))
	for _, b := range p.Blocks {
		r, err := bakeRawBlock(p, b.ID, params)
		if err != nil {
			return err
		}
		raw[b.ID] = r
	}
	res, err := resolveSeams(p, raw)
	if err != nil {
		return err
	}
	for _, b := range p.Blocks {
		points := []Point{
			{X: b.Width / 2, Y: b.Height / 2},
			{X: b.Width / 4, Y: b.Height / 3},
			{X: b.Width * 3 / 4, Y: b.Height * 2 / 3},
		}
		for _, pt := range points {
			key := pixelKey{b.ID, pt.X, pt.Y}
			n := raw[b.ID].Normals[pt.Y*b.Width+pt.X]
			if v, ok := res.Overrides[key]; ok {
				n = v
			}
			p.Truth = append(p.Truth, TruthPixel{
				BlockID: b.ID, X: pt.X, Y: pt.Y, Normal: n, SlopeDeg: slopeDegrees(n),
			})
		}
	}
	seamIDs := make([]string, 0, len(p.Seams))
	for _, s := range p.Seams {
		seamIDs = append(seamIDs, s.ID)
	}
	sort.Strings(seamIDs)
	for _, sid := range seamIDs {
		s, _ := findSeam(p, sid)
		b, _ := FindBlock(p, s.BlockA)
		n := edgeLength(b, s.EdgeA)
		indices := []int{n / 2}
		if n > 2 {
			indices = append(indices, 1)
		}
		for _, idx := range indices {
			angle := res.PairAngles[fmt.Sprintf("%s#%d", sid, idx)]
			p.SeamTruth = append(p.SeamTruth, TruthSeam{SeamID: sid, Index: idx, AngleDeg: angle})
		}
	}
	return nil
}

func dxAt(b *Block, x, y int) float64 {
	at := func(px, py int) float64 {
		if px < 0 {
			px = 0
		} else if px >= b.Width {
			px = b.Width - 1
		}
		if py < 0 {
			py = 0
		} else if py >= b.Height {
			py = b.Height - 1
		}
		return b.Elevation[py*b.Width+px]
	}
	return (at(x+1, y) - at(x-1, y)) * 0.5
}

func dyAt(b *Block, x, y int) float64 {
	at := func(px, py int) float64 {
		if px < 0 {
			px = 0
		} else if px >= b.Width {
			px = b.Width - 1
		}
		if py < 0 {
			py = 0
		} else if py >= b.Height {
			py = b.Height - 1
		}
		return b.Elevation[py*b.Width+px]
	}
	return (at(x, y+1) - at(x, y-1)) * 0.5
}

func quantizeWeights(dst []int, height, slope, base float64) {
	rel := height - base
	rock := 0.16 + 0.24*math.Min(1, slope*0.8)
	grass := 0.54 - 0.16*math.Min(1, rel*0.18) + 0.05*math.Cos(height*1.7)
	snow := 0.22 + 0.22*math.Min(1, math.Max(0, rel-1.1)*0.4)
	if grass < 0.08 {
		grass = 0.08
	}
	if snow < 0.03 {
		snow = 0.03
	}
	scores := []float64{rock, grass, snow}
	sum := scores[0] + scores[1] + scores[2]
	used := 0
	for i := 0; i < 2; i++ {
		dst[i] = int(math.Round(scores[i] / sum * WeightQuantum))
		used += dst[i]
	}
	dst[2] = WeightQuantum - used
	if dst[2] < 0 {
		dst[2] = 0
		excess := -dst[2]
		dst[1] -= excess
	}
}

func valueNoise(x, y float64, seed int64, octaves int) float64 {
	total, amp, norm := 0.0, 1.0, 0.0
	for o := 0; o < octaves; o++ {
		total += amp * smoothNoise(x, y, seed+int64(o)*1013)
		norm += amp
		x, y = x*2.03+7.1, y*2.03-3.7
		amp *= 0.52
	}
	return total / norm
}

func smoothNoise(x, y float64, seed int64) float64 {
	x0, y0 := int64(math.Floor(x)), int64(math.Floor(y))
	tx, ty := x-float64(x0), y-float64(y0)
	sx := tx * tx * (3 - 2*tx)
	sy := ty * ty * (3 - 2*ty)
	n00 := lattice01(x0, y0, seed)
	n10 := lattice01(x0+1, y0, seed)
	n01 := lattice01(x0, y0+1, seed)
	n11 := lattice01(x0+1, y0+1, seed)
	a := n00 + (n10-n00)*sx
	b := n01 + (n11-n01)*sx
	return a + (b-a)*sy
}

func lattice01(x, y, seed int64) float64 {
	z := uint64(seed) + 0x9e3779b97f4a7c15
	z ^= uint64(x) + 0x9e3779b97f4a7c15 + (z << 6) + (z >> 2)
	z ^= uint64(y) + 0x9e3779b97f4a7c15 + (z << 6) + (z >> 2)
	z += 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return float64(z>>11) / float64(uint64(1)<<53)
}

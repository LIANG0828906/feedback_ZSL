package maskqa

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

type shapeKind string

const (
	shapeRoundedRect shapeKind = "rounded_rect"
	shapeEllipse     shapeKind = "ellipse"
	shapeTriangle    shapeKind = "triangle"
)

type shapeSpec struct {
	kind   shapeKind
	id     string
	rect   Rect
	radius int
	color  [3]byte
	inner  *Rect
}

func GenerateDataset() *Dataset {
	p := DefaultParams()
	return GenerateDatasetWithParams(p)
}

func GenerateDatasetWithParams(p Params) *Dataset {
	d := &Dataset{
		SchemaVersion: SchemaVersion,
		AlgoVersion:   AlgoVersion,
		Seed:          p.Seed,
		Params:        p,
		RadiusBand:    [2]int{p.RadiusMin, p.RadiusMax},
	}

	configs := []struct {
		id     string
		w, h   int
		shapes []shapeSpec
	}{
		{
			id: "icon-blobs-96x72", w: 96, h: 72,
			shapes: []shapeSpec{
				{kind: shapeRoundedRect, id: "rr", rect: Rect{X: 11, Y: 15, Width: 35, Height: 29}, radius: 9, color: [3]byte{231, 84, 73}},
				{kind: shapeEllipse, id: "blue", rect: Rect{X: 53, Y: 22, Width: 30, Height: 30}, color: [3]byte{43, 135, 218}},
			},
		},
		{
			id: "icon-ring-80x80", w: 80, h: 80,
			shapes: []shapeSpec{
				{kind: shapeEllipse, id: "ring", rect: Rect{X: 13, Y: 9, Width: 54, Height: 62}, color: [3]byte{42, 171, 118}, inner: &Rect{X: 28, Y: 25, Width: 24, Height: 30}},
			},
		},
		{
			id: "icon-tri-112x80", w: 112, h: 80,
			shapes: []shapeSpec{
				{kind: shapeTriangle, id: "tri", rect: Rect{X: 14, Y: 13, Width: 62, Height: 54}, color: [3]byte{241, 166, 42}},
				{kind: shapeRoundedRect, id: "violet", rect: Rect{X: 84, Y: 31, Width: 18, Height: 27}, radius: 6, color: [3]byte{132, 94, 214}},
			},
		},
	}

	for i, cfg := range configs {
		raster := buildRaster(cfg.w, cfg.h, p.Seed+int64(i)*7919, cfg.shapes)
		truth := NewMask(cfg.w, cfg.h)
		for y := 0; y < cfg.h; y++ {
			for x := 0; x < cfg.w; x++ {
				for _, sh := range cfg.shapes {
					if shapeContains(sh, float64(x)+0.5, float64(y)+0.5) {
						truth.Set(x, y, 1)
						break
					}
				}
			}
		}
		comps := ComputeComponents(truth)
		truthData := &Truth{Mask: truth, Components: comps}
		rough := buildRoughMask(truth, p.Seed+int64(i)*104729)
		s := &Sample{
			ID:     cfg.id,
			Width:  cfg.w,
			Height: cfg.h,
			Raster: raster,
			Rough:  rough,
			Truth:  truthData,
			Blocks: BuildBlocks(cfg.w, cfg.h, p.BlockSize),
		}
		s.InputHash = computeSampleInputHash(s)
		d.Samples = append(d.Samples, s)
	}
	return d
}

func buildRaster(w, h int, seed int64, shapes []shapeSpec) *Raster {
	r := &Raster{Width: w, Height: h, RGBA: make([]byte, w*h*4)}
	rng := rand.New(rand.NewSource(seed))
	top := [3]int{239, 243, 248}
	bottom := [3]int{214, 223, 235}
	for y := 0; y < h; y++ {
		t := float64(y) / float64(maxInt(1, h-1))
		for x := 0; x < w; x++ {
			xt := 0.12 * math.Sin(float64(x)*0.09)
			mix := math.Max(0, math.Min(1, t+xt))
			rr := byte(float64(top[0])*(1-mix) + float64(bottom[0])*mix)
			gg := byte(float64(top[1])*(1-mix) + float64(bottom[1])*mix)
			bb := byte(float64(top[2])*(1-mix) + float64(bottom[2])*mix)
			i := (y*w + x) * 4
			r.RGBA[i], r.RGBA[i+1], r.RGBA[i+2], r.RGBA[i+3] = rr, gg, bb, 255
		}
	}

	for _, sh := range shapes {
		for y := sh.rect.Y; y < sh.rect.Bottom(); y++ {
			for x := sh.rect.X; x < sh.rect.Right(); x++ {
				if x < 0 || y < 0 || x >= w || y >= h || !shapeContains(sh, float64(x)+0.5, float64(y)+0.5) {
					continue
				}
				light := 1.0 + 0.08*math.Sin(float64(x)*0.21+float64(y)*0.13)
				col := [3]byte{
					clampByte(float64(sh.color[0]) * light),
					clampByte(float64(sh.color[1]) * light),
					clampByte(float64(sh.color[2]) * light),
				}
				i := (y*w + x) * 4
				r.RGBA[i], r.RGBA[i+1], r.RGBA[i+2] = col[0], col[1], col[2]
			}
		}
	}

	addSemiTransparentStrokes(r, seed)

	for i := 0; i < w*h; i++ {
		if rng.Float64() < 0.014 {
			idx := i * 4
			switch rng.Intn(3) {
			case 0:
				r.RGBA[idx] = clampByte(float64(r.RGBA[idx]) + float64(rng.Intn(70)-35))
				r.RGBA[idx+1] = clampByte(float64(r.RGBA[idx+1]) + float64(rng.Intn(70)-35))
				r.RGBA[idx+2] = clampByte(float64(r.RGBA[idx+2]) + float64(rng.Intn(70)-35))
			case 1:
				v := byte(rng.Intn(80) + 20)
				r.RGBA[idx], r.RGBA[idx+1], r.RGBA[idx+2] = v, v, clampByte(float64(v)+40)
			default:
				v := byte(rng.Intn(50) + 180)
				r.RGBA[idx], r.RGBA[idx+1], r.RGBA[idx+2] = v, v, v
			}
		}
	}
	return r
}

func addSemiTransparentStrokes(r *Raster, seed int64) {
	rng := rand.New(rand.NewSource(seed + 17))
	colors := [][3]byte{{255, 255, 255}, {30, 41, 59}, {255, 236, 170}}
	for s := 0; s < 3; s++ {
		col := colors[rng.Intn(len(colors))]
		alpha := 0.22 + rng.Float64()*0.18
		cx := float64(18 + rng.Intn(maxInt(1, r.Width-36)))
		cy := float64(16 + rng.Intn(maxInt(1, r.Height-32)))
		rx := float64(8 + rng.Intn(14))
		ry := float64(5 + rng.Intn(10))
		start := rng.Float64() * math.Pi
		arc := math.Pi * (0.55 + rng.Float64()*0.9)
		lastX, lastY := 0.0, 0.0
		for j := 0; j <= 42; j++ {
			a := start + arc*float64(j)/42
			x := cx + rx*math.Cos(a)
			y := cy + ry*math.Sin(a)
			if j > 0 {
				drawBlendedLine(r, lastX, lastY, x, y, col, alpha)
			}
			lastX, lastY = x, y
		}
	}
}

func drawBlendedLine(r *Raster, x0, y0, x1, y1 float64, col [3]byte, alpha float64) {
	steps := int(math.Max(math.Abs(x1-x0), math.Abs(y1-y0))) + 1
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(x0 + (x1-x0)*t))
		y := int(math.Round(y0 + (y1-y0)*t))
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				if dx*dx+dy*dy <= 2 {
					blend(r, x+dx, y+dy, col, alpha*0.72)
				}
			}
		}
	}
}

func blend(r *Raster, x, y int, col [3]byte, alpha float64) {
	if x < 0 || y < 0 || x >= r.Width || y >= r.Height {
		return
	}
	i := (y*r.Width + x) * 4
	for c := 0; c < 3; c++ {
		cur := float64(r.RGBA[i+c])
		add := float64(col[c])
		r.RGBA[i+c] = clampByte(cur*(1-alpha) + add*alpha)
	}
}

func shapeContains(sh shapeSpec, x, y float64) bool {
	r := sh.rect
	switch sh.kind {
	case shapeEllipse:
		cx := float64(r.X) + float64(r.Width)/2
		cy := float64(r.Y) + float64(r.Height)/2
		nx := (x - cx) / (float64(r.Width) / 2)
		ny := (y - cy) / (float64(r.Height) / 2)
		in := nx*nx+ny*ny <= 1
		if sh.inner != nil {
			ix := sh.inner.X + sh.inner.Width/2
			iy := sh.inner.Y + sh.inner.Height/2
			inx := (x - float64(ix)) / (float64(sh.inner.Width) / 2)
			iny := (y - float64(iy)) / (float64(sh.inner.Height) / 2)
			in = in && inx*inx+iny*iny >= 1
		}
		return in
	case shapeRoundedRect:
		rad := float64(sh.radius)
		if rad <= 0 {
			return x >= float64(r.X) && x < float64(r.Right()) && y >= float64(r.Y) && y < float64(r.Bottom())
		}
		qx := math.Abs(x-(float64(r.X)+float64(r.Width)/2)) - (float64(r.Width)/2 - rad)
		qy := math.Abs(y-(float64(r.Y)+float64(r.Height)/2)) - (float64(r.Height)/2 - rad)
		return math.Hypot(math.Max(qx, 0), math.Max(qy, 0))+math.Min(math.Max(qx, qy), 0) <= rad
	case shapeTriangle:
		x0 := float64(r.X + r.Width/2)
		y0 := float64(r.Y)
		x1 := float64(r.X)
		y1 := float64(r.Bottom())
		x2 := float64(r.Right())
		y2 := float64(r.Bottom())
		c1 := (x-x1)*(y0-y1) - (x0-x1)*(y-y1)
		c2 := (x-x2)*(y1-y2) - (x1-x2)*(y-y2)
		c3 := (x-x0)*(y2-y0) - (x2-x0)*(y-y0)
		neg := c1 < 0 || c2 < 0 || c3 < 0
		pos := c1 > 0 || c2 > 0 || c3 > 0
		return !(neg && pos)
	}
	return false
}

func buildRoughMask(truth *Mask, seed int64) *Mask {
	m := truth.Clone()
	rng := rand.New(rand.NewSource(seed))
	var boundary []imagePoint
	var interior []imagePoint
	var farBG []imagePoint
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.At(x, y) == 1 {
				if m.At(x-1, y) == 0 || m.At(x+1, y) == 0 || m.At(x, y-1) == 0 || m.At(x, y+1) == 0 {
					boundary = append(boundary, imagePoint{x, y})
				} else if m.At(x-1, y) == 1 && m.At(x+1, y) == 1 && m.At(x, y-1) == 1 && m.At(x, y+1) == 1 {
					interior = append(interior, imagePoint{x, y})
				}
			} else {
				d := nearestForegroundDistanceSquared(truth, x, y, 7)
				if d >= 25 && d <= 100 {
					farBG = append(farBG, imagePoint{x, y})
				}
			}
		}
	}
	rng.Shuffle(len(boundary), func(i, j int) { boundary[i], boundary[j] = boundary[j], boundary[i] })
	rng.Shuffle(len(interior), func(i, j int) { interior[i], interior[j] = interior[j], interior[i] })
	rng.Shuffle(len(farBG), func(i, j int) { farBG[i], farBG[j] = farBG[j], farBG[i] })

	used := map[imagePoint]bool{}
	notches, spills := 0, 0
	for _, p := range boundary {
		if used[p] {
			continue
		}
		if notches < 5 && rng.Float64() < 0.55 {
			rad := 1
			paintDisk(m, p.x, p.y, rad, 0)
			markUsed(used, p, 5)
			notches++
		} else if spills < 5 && rng.Float64() < 0.55 {
			rad := 1
			paintDisk(m, p.x, p.y, rad, 1)
			markUsed(used, p, 5)
			spills++
		}
		if notches >= 5 && spills >= 5 {
			break
		}
	}

	holes := 0
	for _, p := range interior {
		if holes >= 3 {
			break
		}
		if used[p] {
			continue
		}
		size := 1
		if rng.Intn(4) == 0 {
			size = 2
		}
		for dy := 0; dy < size; dy++ {
			for dx := 0; dx < size; dx++ {
				m.Set(p.x+dx, p.y+dy, 0)
			}
		}
		markUsed(used, p, 6)
		holes++
	}

	specks := 0
	for _, p := range farBG {
		if specks >= 5 {
			break
		}
		m.Set(p.x, p.y, 1)
		if rng.Intn(2) == 0 {
			m.Set(p.x+1, p.y, 1)
		} else {
			m.Set(p.x, p.y+1, 1)
		}
		specks++
	}
	return m
}

type imagePoint struct{ x, y int }

func paintDisk(m *Mask, cx, cy, r int, v byte) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r+r/2 {
				m.Set(cx+dx, cy+dy, v)
			}
		}
	}
}

func markUsed(used map[imagePoint]bool, p imagePoint, radius int) {
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			used[imagePoint{p.x + dx, p.y + dy}] = true
		}
	}
}

func nearestForegroundDistanceSquared(m *Mask, x, y, limit int) int {
	best := (limit + 1) * (limit + 1)
	for dy := -limit; dy <= limit; dy++ {
		for dx := -limit; dx <= limit; dx++ {
			if dx*dx+dy*dy >= best {
				continue
			}
			if m.At(x+dx, y+dy) == 1 {
				best = dx*dx + dy*dy
			}
		}
	}
	return best
}

func ComputeComponents(m *Mask) []Component {
	label := make([][]int, m.Height)
	for y := range label {
		label[y] = make([]int, m.Width)
	}
	type accum struct {
		id                     int
		sumX, sumY, area       int
		minX, minY, maxX, maxY int
		pixels                 []imagePoint
	}
	var comps []*accum
	dirs := []imagePoint{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.At(x, y) == 0 || label[y][x] != 0 {
				continue
			}
			id := len(comps) + 1
			a := &accum{id: id, minX: x, maxX: x, minY: y, maxY: y}
			queue := []imagePoint{{x, y}}
			label[y][x] = id
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				a.area++
				a.sumX += p.x
				a.sumY += p.y
				a.minX = minInt(a.minX, p.x)
				a.maxX = maxInt(a.maxX, p.x)
				a.minY = minInt(a.minY, p.y)
				a.maxY = maxInt(a.maxY, p.y)
				a.pixels = append(a.pixels, p)
				for _, d := range dirs {
					nx, ny := p.x+d.x, p.y+d.y
					if m.At(nx, ny) == 1 && label[ny][nx] == 0 {
						label[ny][nx] = id
						queue = append(queue, imagePoint{nx, ny})
					}
				}
			}
			comps = append(comps, a)
		}
	}

	bgLabel := make([][]int, m.Height)
	for y := range bgLabel {
		bgLabel[y] = make([]int, m.Width)
	}
	var holes [][]imagePoint
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.At(x, y) == 1 || bgLabel[y][x] != 0 {
				continue
			}
			hid := len(holes) + 1
			var pts []imagePoint
			touchesBorder := false
			queue := []imagePoint{{x, y}}
			bgLabel[y][x] = hid
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				pts = append(pts, p)
				if p.x == 0 || p.y == 0 || p.x == m.Width-1 || p.y == m.Height-1 {
					touchesBorder = true
				}
				for _, d := range dirs {
					nx, ny := p.x+d.x, p.y+d.y
					if m.InBounds(nx, ny) && m.At(nx, ny) == 0 && bgLabel[ny][nx] == 0 {
						bgLabel[ny][nx] = hid
						queue = append(queue, imagePoint{nx, ny})
					}
				}
			}
			if !touchesBorder {
				holes = append(holes, pts)
			}
		}
	}

	out := make([]Component, 0, len(comps))
	for i, a := range comps {
		out = append(out, Component{
			ID:        fmt.Sprintf("fg-%d", i+1),
			CentroidX: (float64(a.sumX) + 0.5*float64(a.area)) / float64(a.area),
			CentroidY: (float64(a.sumY) + 0.5*float64(a.area)) / float64(a.area),
			Area:      a.area,
			Bounds:    Rect{X: a.minX, Y: a.minY, Width: a.maxX - a.minX + 1, Height: a.maxY - a.minY + 1},
			Holes:     0,
		})
	}
	for _, hole := range holes {
		votes := map[int]int{}
		for _, p := range hole {
			for _, d := range dirs {
				nx, ny := p.x+d.x, p.y+d.y
				if ny >= 0 && ny < m.Height && nx >= 0 && nx < m.Width {
					if id := label[ny][nx]; id > 0 {
						votes[id]++
					}
				}
			}
		}
		bestID, bestVotes := 0, -1
		for id, v := range votes {
			if v > bestVotes || (v == bestVotes && (bestID == 0 || id < bestID)) {
				bestID, bestVotes = id, v
			}
		}
		if bestID > 0 {
			out[bestID-1].Holes++
		}
	}
	return out
}

func computeSampleInputHash(s *Sample) string {
	rasterPart := append([]byte(fmt.Sprintf("%dx%d", s.Width, s.Height)), s.Raster.RGBA...)
	roughPart := append([]byte(fmt.Sprintf("%dx%d", s.Rough.Width, s.Rough.Height)), s.Rough.Data...)
	return HashBytes("sample-input-v1", rasterPart, roughPart)
}

func SaveDataset(path string, d *Dataset) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadDataset(path string) (*Dataset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Dataset
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, schemaError("document", path, "invalid JSON dataset: %v", err)
	}
	if err := ValidateDataset(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func clampByte(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(math.Round(v))
}

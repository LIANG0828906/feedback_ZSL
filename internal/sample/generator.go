package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"edgerefine/internal/errs"
	"edgerefine/internal/imagecodec"
	"edgerefine/internal/model"
)

type Spec struct {
	ID     string
	Width  int
	Height int
	Seed   int64
}

func DefaultSpecs() []Spec {
	return []Spec{
		{ID: "step128", Width: 128, Height: 96, Seed: 42026801},
		{ID: "gradient160", Width: 160, Height: 128, Seed: 42026802},
		{ID: "spots96", Width: 96, Height: 96, Seed: 42026803},
	}
}

type rectF struct{ x1, y1, x2, y2 float64 }

type scene struct {
	main     rectF
	band     rectF
	triangle [3]model.Point
	edges    []model.TruthEdge
}

func makeScene(w, h int) scene {
	main := rectF{
		x1: math.Round(float64(w)*0.28) - 0.7,
		y1: math.Round(float64(h)*0.20) + 0.2,
		x2: math.Round(float64(w)*0.72) + 0.4,
		y2: math.Round(float64(h)*0.56) - 0.3,
	}
	band := rectF{
		x1: math.Round(float64(w)*0.55) - 0.2,
		y1: math.Round(float64(h)*0.70) + 0.4,
		x2: math.Round(float64(w)*0.88) + 0.5,
		y2: math.Round(float64(h)*0.86) - 0.4,
	}
	tri := [3]model.Point{
		{X: math.Round(float64(w)*0.12) + 0.5, Y: math.Round(float64(h)*0.72) - 0.4},
		{X: math.Round(float64(w)*0.42) + 0.2, Y: math.Round(float64(h)*0.62) + 0.5},
		{X: math.Round(float64(w)*0.46) - 0.3, Y: math.Round(float64(h)*0.90) - 0.2},
	}
	sc := scene{main: main, band: band, triangle: tri}
	id := 1
	addRect := func(r rectF, kind, polarity string) {
		sc.edges = append(sc.edges,
			model.TruthEdge{ID: id, Kind: kind + "-left", Polarity: polarity, X1: r.x1, Y1: r.y1, X2: r.x1, Y2: r.y2, SubpixelOffset: math.Mod(r.x1, 1), ReferenceLine: "x=" + fmt.Sprintf("%.4f", r.x1)},
		)
		id++
		sc.edges = append(sc.edges, model.TruthEdge{ID: id, Kind: kind + "-top", Polarity: polarity, X1: r.x1, Y1: r.y1, X2: r.x2, Y2: r.y1, SubpixelOffset: math.Mod(r.y1, 1), ReferenceLine: "y=" + fmt.Sprintf("%.4f", r.y1)})
		id++
		sc.edges = append(sc.edges, model.TruthEdge{ID: id, Kind: kind + "-right", Polarity: polarity + "-reverse", X1: r.x2, Y1: r.y1, X2: r.x2, Y2: r.y2, SubpixelOffset: math.Mod(r.x2, 1), ReferenceLine: "x=" + fmt.Sprintf("%.4f", r.x2)})
		id++
		sc.edges = append(sc.edges, model.TruthEdge{ID: id, Kind: kind + "-bottom", Polarity: polarity + "-reverse", X1: r.x1, Y1: r.y2, X2: r.x2, Y2: r.y2, SubpixelOffset: math.Mod(r.y2, 1), ReferenceLine: "y=" + fmt.Sprintf("%.4f", r.y2)})
		id++
	}
	addRect(main, "step", "bright")
	addRect(band, "gradient-band", "bright")
	for i := 0; i < 3; i++ {
		a := tri[i]
		b := tri[(i+1)%3]
		dx, dy := b.X-a.X, b.Y-a.Y
		l := math.Hypot(dx, dy)
		nx, ny := -dy/l, dx/l
		rho := math.Abs(nx*a.X + ny*a.Y)
		sc.edges = append(sc.edges, model.TruthEdge{
			ID: id, Kind: fmt.Sprintf("diagonal-%d", i+1), Polarity: "bright",
			X1: a.X, Y1: a.Y, X2: b.X, Y2: b.Y,
			SubpixelOffset: math.Mod(rho, 1),
			ReferenceLine:  fmt.Sprintf("normal=(%.4f,%.4f)", nx, ny),
		})
		id++
	}
	return sc
}

func inRect(x, y float64, r rectF) bool {
	return x >= r.x1 && x < r.x2 && y >= r.y1 && y < r.y2
}

func sign(p1, p2, p3 model.Point) float64 {
	return (p1.X-p3.X)*(p2.Y-p3.Y) - (p1.Y-p3.Y)*(p2.X-p3.X)
}

func inTriangle(x, y float64, t [3]model.Point) bool {
	p := model.Point{X: x, Y: y}
	d1 := sign(p, t[0], t[1])
	d2 := sign(p, t[1], t[2])
	d3 := sign(p, t[2], t[0])
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func (s scene) sample(x, y, w, h float64) (float64, bool) {
	base := 36 + 24*x/w + 7*math.Sin(math.Pi*y/h)
	fg := false
	v := base
	if inRect(x, y, s.band) {
		fg = true
		span := s.band.x2 - s.band.x1
		v = 150 + 65*(x-s.band.x1)/span
	}
	if inTriangle(x, y, s.triangle) {
		fg = true
		v = 184 + 8*math.Sin((x+y)*0.11)
	}
	if inRect(x, y, s.main) {
		fg = true
		v = 222 + 5*math.Sin(x*0.17)
	}
	return v, fg
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

func Generate(root string, spec Spec) (model.Manifest, error) {
	if spec.Width%32 != 0 || spec.Height%32 != 0 {
		return model.Manifest{}, errs.Para("resolution", [2]int{spec.Width, spec.Height}, [2]any{"multiples of", 32})
	}
	dir := filepath.Join(root, spec.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return model.Manifest{}, err
	}
	sc := makeScene(spec.Width, spec.Height)
	rng := rand.New(rand.NewSource(spec.Seed))
	im := &model.GrayImage{Width: spec.Width, Height: spec.Height, Channels: 1, BitDepth: 8, Pix: make([]uint8, spec.Width*spec.Height)}
	ref := make([]uint8, spec.Width*spec.Height)
	const ss = 4
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			var sumV, cov float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					xf := float64(x) + (float64(sx)+0.5)/ss
					yf := float64(y) + (float64(sy)+0.5)/ss
					v, fg := sc.sample(xf, yf, float64(spec.Width), float64(spec.Height))
					sumV += v
					if fg {
						cov++
					}
				}
			}
			cov /= ss * ss
			v := sumV / (ss * ss)
			v += rng.NormFloat64() * 4.5
			im.Pix[y*spec.Width+x] = clampByte(v)
			ref[y*spec.Width+x] = clampByte(cov * 255)
		}
	}
	setSpot := func(x, y int, v byte) {
		if x >= 0 && x < spec.Width && y >= 0 && y < spec.Height {
			im.Pix[y*spec.Width+x] = v
		}
	}
	setSpot(int(sc.main.x1+sc.main.x2)/2, int(sc.main.y1+sc.main.y2)/2, 8)
	setSpot(int(sc.main.x1+sc.main.x2)/2+1, int(sc.main.y1+sc.main.y2)/2, 16)
	bx, by := int(sc.main.x1)-8, int(sc.main.y1)-7
	setSpot(bx, by, 255)
	setSpot(bx+1, by, 255)
	setSpot(bx, by+1, 255)
	dx, dy := spec.Width/2, 9
	setSpot(dx, dy, 0)
	setSpot(dx+1, dy, 4)
	setSpot(dx, dy+1, 12)
	for i := 0; i < 24; i++ {
		x := rng.Intn(spec.Width)
		y := rng.Intn(spec.Height)
		if rng.Intn(2) == 0 {
			setSpot(x, y, 255)
		} else {
			setSpot(x, y, 0)
		}
	}

	imgPath := filepath.Join(dir, "image.png")
	if err := imagecodec.SaveGrayPNG(imgPath, im); err != nil {
		return model.Manifest{}, err
	}
	refPath := filepath.Join(dir, "reference_mask.png")
	refIm := &model.GrayImage{Width: spec.Width, Height: spec.Height, Channels: 1, BitDepth: 8, Pix: ref}
	if err := imagecodec.SaveGrayPNG(refPath, refIm); err != nil {
		return model.Manifest{}, err
	}
	refHash := model.HashBytes("reference-mask-v1:", ref)
	params := model.DefaultParams()
	m := model.Manifest{
		SchemaVersion: model.SchemaVersion, AlgorithmVersion: model.AlgorithmVersion,
		SampleID: spec.ID, Seed: spec.Seed, ImageFile: "image.png",
		Width: spec.Width, Height: spec.Height, Channels: 1, BitDepth: 8,
		ReferenceMaskFile: "reference_mask.png", ReferenceMaskHash: refHash,
		Params: params, Edges: sc.edges,
	}
	m.TruthFingerprint = Fingerprint(m)
	if err := WriteManifest(filepath.Join(dir, "truth.json"), m); err != nil {
		return model.Manifest{}, err
	}
	return m, nil
}

func Fingerprint(m model.Manifest) string {
	m.TruthFingerprint = ""
	b, _ := json.Marshal(m)
	return model.HashBytes("truth-manifest-v1:", b)
}

func WriteManifest(path string, m model.Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func GenerateAll(root string) ([]model.Manifest, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	var ms []model.Manifest
	for _, spec := range DefaultSpecs() {
		m, err := Generate(root, spec)
		if err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	index, err := json.MarshalIndent(ms, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), index, 0o644); err != nil {
		return nil, err
	}
	return ms, nil
}

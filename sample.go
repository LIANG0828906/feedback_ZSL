package iconcut

import (
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
)

type SampleSpec struct {
	ID    string
	Seed  int64
	W     int
	H     int
	TileW int
	TileH int
}

func DefaultSpecs() []SampleSpec {
	return []SampleSpec{
		{ID: "icon-144", Seed: 141414, W: 144, H: 108, TileW: 36, TileH: 36},
		{ID: "icon-192", Seed: 424242, W: 192, H: 144, TileW: 48, TileH: 48},
		{ID: "icon-256", Seed: 256256, W: 256, H: 192, TileW: 64, TileH: 48},
	}
}

type shapeSpec struct {
	id    int
	name  string
	color [3]uint8
	rect  Rect
	hole  *Rect
	seed  IPoint
}

type Sample struct {
	Manifest  *Manifest
	Source    *Image
	Reference *LabelMask
	Ideal     *Image
}

func noise(seed int64, x, y, channel int, amp float64) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte{byte(seed), byte(seed >> 8), byte(x), byte(x >> 8), byte(y), byte(y >> 8), byte(channel)})
	v := int(h.Sum32()%1000) - 500
	return float64(v) / 500 * amp
}

func scaleV(v float64, s float64) int { return int(math.Round(v * s)) }

func scaledRect(r Rect, sx, sy float64) Rect {
	x0 := scaleV(float64(r.X), sx)
	y0 := scaleV(float64(r.Y), sy)
	x1 := scaleV(float64(r.MaxX()), sx)
	y1 := scaleV(float64(r.MaxY()), sy)
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func baseShapes(w, h int) []shapeSpec {
	sx := float64(w) / 192
	sy := float64(h) / 144
	hole := scaledRect(Rect{X: 58, Y: 60, W: 14, H: 16}, sx, sy)
	return []shapeSpec{
		{1, "amber_plate", [3]uint8{231, 143, 66}, scaledRect(Rect{X: 19, Y: 23, W: 58, H: 55}, sx, sy), &hole, IPoint{X: scaleV(42, sx), Y: scaleV(48, sy)}},
		{2, "teal_gem", [3]uint8{61, 190, 177}, scaledRect(Rect{X: 117, Y: 20, W: 52, H: 45}, sx, sy), nil, IPoint{X: scaleV(143, sx), Y: scaleV(42, sy)}},
		{3, "low_contrast_tab", [3]uint8{99, 93, 111}, scaledRect(Rect{X: 65, Y: 95, W: 65, H: 29}, sx, sy), nil, IPoint{X: scaleV(97, sx), Y: scaleV(109, sy)}},
		{4, "rose_badge", [3]uint8{205, 83, 112}, scaledRect(Rect{X: 19, Y: 96, W: 35, H: 29}, sx, sy), nil, IPoint{X: scaleV(36, sx), Y: scaleV(110, sy)}},
	}
}

func geometryForSpec(sp SampleSpec) Geometry {
	if sp.ID == "icon-192" {
		return Geometry{
			Source: Size{W: 192, H: 144}, Corrected: Size{W: 192, H: 144},
			Homography: [3][3]float64{
				{1.0948, -0.0396, -6.2496},
				{0.0436, 0.9418, 0.0048},
				{0.0008, -0.0006, 0.9664},
			},
			Radial:     RadialParams{CenterX: 96, CenterY: 72, Radius: 112.32, K1: -0.055, K2: 0.022},
			CoordScale: 16,
		}
	}
	W, H := float64(sp.W), float64(sp.H)
	return Geometry{
		Source: Size{W: sp.W, H: sp.H}, Corrected: Size{W: sp.W, H: sp.H},
		Homography: [3][3]float64{
			{1.075, -0.0396, -0.03255 * W},
			{0.0436, 0.952, 0.0000333 * H},
			{0.1536 / W, -0.0864 / H, 0.9664},
		},
		Radial:     RadialParams{CenterX: W / 2, CenterY: H / 2, Radius: 0.78 * H, K1: -0.052, K2: 0.02},
		CoordScale: 16,
	}
}

func controlsForSpec(sp SampleSpec, m *Mapper) []ControlPoint {
	base := []Point{{X: 14, Y: 14}, {X: 177, Y: 15}, {X: 15, Y: 129}, {X: 177, Y: 129}, {X: 96, Y: 72}}
	sx := float64(sp.W) / 192
	sy := float64(sp.H) / 144
	out := make([]ControlPoint, 0, len(base))
	for _, p := range base {
		cp := Point{X: p.X * sx, Y: p.Y * sy}
		out = append(out, ControlPoint{Corrected: cp, Source: m.MapPoint(cp)})
	}
	return out
}

func GenerateSample(sp SampleSpec) (*Sample, error) {
	g := geometryForSpec(sp)
	m, err := NewMapper(g)
	if err != nil {
		return nil, err
	}
	shapes := baseShapes(sp.W, sp.H)
	ideal := NewImage(sp.W, sp.H, 3)
	ref := NewLabelMask(sp.W, sp.H)
	seed := sp.Seed

	for y := 0; y < sp.H; y++ {
		t := float64(y) / float64(maxInt(1, sp.H-1))
		bgTop := [3]float64{50, 45, 72}
		bgBottom := [3]float64{82, 73, 101}
		for x := 0; x < sp.W; x++ {
			bg := [3]byte{
				byte(clampFloat(bgTop[0]*(1-t)+bgBottom[0]*t+noise(seed, x, y, 0, 3.2), 0, 255)),
				byte(clampFloat(bgTop[1]*(1-t)+bgBottom[1]*t+noise(seed, x, y, 1, 3.0), 0, 255)),
				byte(clampFloat(bgTop[2]*(1-t)+bgBottom[2]*t+noise(seed, x, y, 2, 3.6), 0, 255)),
			}
			ideal.SetPixel(x, y, bg)
		}
	}

	for _, sh := range shapes {
		for y := sh.rect.Y; y < sh.rect.MaxY(); y++ {
			for x := sh.rect.X; x < sh.rect.MaxX(); x++ {
				if x < 0 || y < 0 || x >= sp.W || y >= sp.H {
					continue
				}
				if sh.hole != nil && sh.hole.Contains(x, y) {
					continue
				}
				ref.Set(x, y, uint8(sh.id))
				c := sh.color
				n0 := noise(seed+11, x, y, 3, 4.5)
				n1 := noise(seed+17, x, y, 4, 4.0)
				n2 := noise(seed+23, x, y, 5, 4.8)
				ideal.SetPixel(x, y, [3]byte{
					byte(clampFloat(float64(c[0])+n0, 0, 255)),
					byte(clampFloat(float64(c[1])+n1, 0, 255)),
					byte(clampFloat(float64(c[2])+n2, 0, 255)),
				})
			}
		}
	}

	src := NewImage(sp.W, sp.H, 3)
	rs := &Resampler{Src: ideal, M: m, Order: 0}
	for y := 0; y < sp.H; y++ {
		for x := 0; x < sp.W; x++ {
			cp := InverseMapPoint(g, Point{X: float64(x), Y: float64(y)})
			c := rs.Sample(cp.X, cp.Y)
			n := [3]float64{noise(seed+29, x, y, 0, 2.2), noise(seed+31, x, y, 1, 2.2), noise(seed+37, x, y, 2, 2.2)}
			src.SetPixel(x, y, [3]byte{
				byte(clampFloat(float64(c[0])+n[0], 0, 255)),
				byte(clampFloat(float64(c[1])+n[1], 0, 255)),
				byte(clampFloat(float64(c[2])+n[2], 0, 255)),
			})
		}
	}

	stats := AnalyzeLabels(ref)
	regions := make([]RegionTruth, 0, len(stats))
	for _, st := range stats {
		sh := shapes[st.ID-1]
		regions = append(regions, RegionTruth{
			ID: st.ID, Name: sh.name, Color: sh.color, Seed: sh.seed,
			CentroidX: st.Centroid.X, CentroidY: st.Centroid.Y, Area: st.Area,
			BBox: BBox{X: st.BBox.X, Y: st.BBox.Y, W: st.BBox.W, H: st.BBox.H}, Holes: st.Holes,
		})
	}
	seeds := make([]IPoint, 0, len(shapes))
	for _, sh := range shapes {
		seeds = append(seeds, sh.seed)
	}
	manifest := &Manifest{
		SchemaVersion: "iconcut.sample/v1", ImageID: sp.ID, Seed: sp.Seed,
		Source: Size{W: sp.W, H: sp.H}, Corrected: Size{W: sp.W, H: sp.H}, Channels: 3,
		Geometry: g, Controls: controlsForSpec(sp, m), Regions: regions,
		Declared: Declared{
			TileWidth: sp.TileW, TileHeight: sp.TileH, BandPX: 5, InterpOrder: 1,
			Segment:   SegmentParams{ColorTolerance: 22, GradientThreshold: 14, MorphSize: 3, SeedPoints: seeds},
			Budget:    Budget{Workers: 4, MaxThreadPixels: 262144, MaxQueuedTasks: 4, PreviewDelayMaxMS: 1000},
			Tolerance: Tolerances{ControlPX: 1.0, MapPixelAbs: 0, PreviewCentroidPX: 2.0, PreviewAreaRel: 0.08, RegionCentroidPX: 1.25, RegionAreaRel: 0.04, HoleCountDelta: 0},
			Precision: Precision{CoordScale: 16},
		},
	}
	if sp.ID == "icon-192" {
		manifest.Declared.Segment.ColorTolerance = 18
		manifest.Declared.Segment.GradientThreshold = 12
	}
	return &Sample{Manifest: manifest, Source: src, Reference: ref, Ideal: ideal}, nil
}

func TruthFingerprint(m *Manifest, ref *LabelMask) string {
	h := HashJSON(struct {
		Regions  []RegionTruth  `json:"regions"`
		Controls []ControlPoint `json:"controls"`
	}{Regions: m.Regions, Controls: m.Controls})
	return SHA256Bytes(append([]byte(h+":"), ref.Labels...))
}

func GenerateBundle(root string, sp SampleSpec) (string, *Sample, error) {
	s, err := GenerateSample(sp)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(root, sp.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	if err := WriteJSON(filepath.Join(dir, "manifest.json"), s.Manifest); err != nil {
		return dir, s, err
	}
	if err := WritePNG(filepath.Join(dir, "source.png"), s.Source); err != nil {
		return dir, s, err
	}
	if err := WritePNG(filepath.Join(dir, "corrected_ideal.png"), s.Ideal); err != nil {
		return dir, s, err
	}
	if err := WriteLabelPNG(filepath.Join(dir, "reference.png"), s.Reference); err != nil {
		return dir, s, err
	}
	fp := TruthFingerprint(s.Manifest, s.Reference)
	if err := os.WriteFile(filepath.Join(dir, "truth.sha256"), []byte(fp+"\n"), 0o644); err != nil {
		return dir, s, err
	}
	return dir, s, nil
}

func WriteJSON(path string, v any) error {
	b, err := jsonMarshalIndent(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

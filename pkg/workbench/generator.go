package workbench

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
)

type splitMix64 struct {
	state uint64
}

func newSplitMix(seed int64) *splitMix64 {
	return &splitMix64{state: uint64(seed)}
}

func (s *splitMix64) next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (s *splitMix64) float() float64 {
	return float64(s.next()>>11) / float64(uint64(1)<<53)
}

func hash2D(seed int64, x, y int) uint64 {
	h := fnv.New64a()
	var buf [24]byte
	put := func(offset int, v uint64) {
		for i := 0; i < 8; i++ {
			buf[offset+i] = byte(v >> (8 * i))
		}
	}
	put(0, uint64(seed))
	put(8, uint64(uint32(x)))
	put(16, uint64(uint32(y)))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

func smoothNoise(seed int64, x, y float64) float64 {
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	fx := x - float64(x0)
	fy := y - float64(y0)
	fx = fx * fx * (3 - 2*fx)
	fy = fy * fy * (3 - 2*fy)
	value := func(ix, iy int) float64 {
		v := hash2D(seed, ix, iy)
		return float64(v>>11)/float64(uint64(1)<<53)*2 - 1
	}
	v00 := value(x0, y0)
	v10 := value(x0+1, y0)
	v01 := value(x0, y0+1)
	v11 := value(x0+1, y0+1)
	top := v00 + (v10-v00)*fx
	bottom := v01 + (v11-v01)*fx
	detail := top + (bottom-top)*fy
	return 0.55*detail + 0.30*smoothNoiseOne(seed+17, x*2.07, y*2.07) + 0.15*smoothNoiseOne(seed+37, x*4.13, y*4.13)
}

func smoothNoiseOne(seed int64, x, y float64) float64 {
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	fx := x - float64(x0)
	fy := y - float64(y0)
	fx = fx * fx * (3 - 2*fx)
	fy = fy * fy * (3 - 2*fy)
	value := func(ix, iy int) float64 {
		v := hash2D(seed, ix, iy)
		return float64(v>>11)/float64(uint64(1)<<53)*2 - 1
	}
	v00 := value(x0, y0)
	v10 := value(x0+1, y0)
	v01 := value(x0, y0+1)
	v11 := value(x0+1, y0+1)
	top := v00 + (v10-v00)*fx
	bottom := v01 + (v11-v01)*fx
	return top + (bottom-top)*fy
}

type patchSpec struct {
	id        string
	x, y      int
	w, h      int
	coeffs    [3]float64
	intensity float64
}

func patchLayout(width, height int) []patchSpec {
	pw := maxInt(8, minInt(160, width/6))
	ph := maxInt(8, minInt(120, height/6))
	gapX := maxInt(2, (width-4*pw)/5)
	gapY := maxInt(2, (height-3*ph)/4)
	specs := []patchSpec{
		{id: "neutral", x: gapX, y: gapY, w: pw, h: ph, coeffs: [3]float64{0.55, 0.55, 0.55}, intensity: 0.85},
		{id: "ember", x: gapX*2 + pw, y: gapY, w: pw, h: ph, coeffs: [3]float64{1.0, 0.37, 0.12}, intensity: 0.72},
		{id: "foliage", x: gapX*3 + 2*pw, y: gapY, w: pw, h: ph, coeffs: [3]float64{0.15, 0.95, 0.24}, intensity: 0.68},
		{id: "cobalt", x: gapX*4 + 3*pw, y: gapY, w: pw, h: ph, coeffs: [3]float64{0.10, 0.28, 1.0}, intensity: 0.78},
		{id: "amber", x: gapX, y: gapY*2 + ph, w: pw, h: ph, coeffs: [3]float64{0.95, 0.66, 0.18}, intensity: 0.74},
		{id: "cyan", x: gapX*2 + pw, y: gapY*2 + ph, w: pw, h: ph, coeffs: [3]float64{0.10, 0.82, 0.90}, intensity: 0.70},
	}
	for i := range specs {
		if specs[i].x+specs[i].w >= width {
			specs[i].x = maxInt(1, width-specs[i].w-2)
		}
		if specs[i].y+specs[i].h >= height {
			specs[i].y = maxInt(1, height-specs[i].h-2)
		}
	}
	return specs
}

func spectralWeights(band int, bands int, x, y float64, width, height int, patches []patchSpec) (float64, bool) {
	for _, p := range patches {
		if x >= float64(p.x) && x < float64(p.x+p.w) && y >= float64(p.y) && y < float64(p.y+p.h) {
			return patchBandWeight(band, bands, p.coeffs), true
		}
	}
	t := 0.0
	if bands > 1 {
		t = float64(band) / float64(bands-1)
	}
	u := x / math.Max(1, float64(width-1))
	v := y / math.Max(1, float64(height-1))
	phase := 2 * math.Pi * (0.25 + 0.55*u + 0.35*v)
	base := 0.80 + 0.22*math.Sin(phase+t*math.Pi)
	if band == 0 {
		base += 0.15 * math.Exp(-((u-0.18)*(u-0.18)+(v-0.22)*(v-0.22))/0.04)
	}
	if band == bands-1 {
		base += 0.18 * math.Exp(-((u-0.82)*(u-0.82)+(v-0.76)*(v-0.76))/0.05)
	}
	return math.Max(0.04, base), false
}

func patchBandWeight(band, bands int, coeffs [3]float64) float64 {
	t := 0.0
	if bands > 1 {
		t = float64(band) / float64(bands-1)
	}
	red := t * t
	blue := (1 - t) * (1 - t)
	green := math.Max(0, 1-math.Abs(2*t-1))
	return math.Max(0.025, 0.04+coeffs[0]*red+coeffs[1]*green+coeffs[2]*blue)
}

func (g *generatorScene) baseLuminance(x, y float64) float64 {
	u := x / math.Max(1, float64(g.meta.Width-1))
	v := y / math.Max(1, float64(g.meta.Height-1))
	base := 0.42 + 0.34*math.Sin(2*math.Pi*(0.15*u+0.11*v)+0.7)
	r1 := math.Exp(-((u-0.28)*(u-0.28) + (v-0.33)*(v-0.33)) / 0.018)
	r2 := math.Exp(-((u-0.76)*(u-0.76) + (v-0.68)*(v-0.68)) / 0.010)
	return math.Max(0.08, base+2.9*r1+5.2*r2)
}

type generatorScene struct {
	meta        Metadata
	patches     []patchSpec
	offsets     []RegistrationOffset
	radianceAmp float64
	detailAmp   float64
}

func (g *generatorScene) bandRadiance(band int, x, y float64) float64 {
	off := g.offsets[band]
	sx := x + off.DX
	sy := y + off.DY
	weight, _ := spectralWeights(band, g.meta.BandCount, sx, sy, g.meta.Width, g.meta.Height, g.patches)
	return g.baseLuminance(sx, sy) * weight
}

func (g *generatorScene) referenceBandRadiance(band int, x, y float64) float64 {
	weight, _ := spectralWeights(band, g.meta.BandCount, x, y, g.meta.Width, g.meta.Height, g.patches)
	return g.baseLuminance(x, y) * weight
}

func (g *generatorScene) luminance(x, y float64) float64 {
	var sum float64
	for b := 0; b < g.meta.BandCount; b++ {
		sum += g.referenceBandRadiance(b, x, y)
	}
	return sum / float64(g.meta.BandCount)
}

func (g *generatorScene) insidePatch(x, y float64) bool {
	for _, p := range g.patches {
		if x >= float64(p.x) && x < float64(p.x+p.w) && y >= float64(p.y) && y < float64(p.y+p.h) {
			return true
		}
	}
	return false
}

func (g *generatorScene) detailAt(x, y int) float64 {
	if g.insidePatch(float64(x), float64(y)) {
		return 0
	}
	return g.detailAmp * smoothNoise(g.meta.BandMetadataVersionSeed(), float64(x), float64(y))
}

func (m Metadata) BandMetadataVersionSeed() int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%d|%d|%d|%s|%.6f", m.BandMetadataVersion, m.Width, m.Height, m.BandCount, m.TargetGamut, m.WhitePoint.X)
	return int64(h.Sum64())
}

func GenerateScene(cfg GenerateConfig) (*Scene, error) {
	if cfg.Width == 0 {
		cfg.Width = DefaultGenerateConfig().Width
	}
	if cfg.Height == 0 {
		cfg.Height = DefaultGenerateConfig().Height
	}
	if cfg.Bands == 0 {
		cfg.Bands = DefaultGenerateConfig().Bands
	}
	if cfg.Exposures == 0 {
		cfg.Exposures = DefaultGenerateConfig().Exposures
	}
	if cfg.BitDepth == 0 {
		cfg.BitDepth = 16
	}
	if len(cfg.BandCenters) == 0 {
		switch cfg.Bands {
		case 3:
			cfg.BandCenters = []float64{450, 550, 650}
		case 4:
			cfg.BandCenters = []float64{430, 510, 590, 670}
		default:
			cfg.BandCenters = make([]float64, cfg.Bands)
			for i := range cfg.BandCenters {
				cfg.BandCenters[i] = 410 + float64(i)*300/float64(maxInt(1, cfg.Bands-1))
			}
		}
	}
	if len(cfg.ExposureTimes) == 0 {
		cfg.ExposureTimes = []float64{1.0 / 250.0, 1.0 / 60.0, 1.0 / 12.0}
		if cfg.Exposures > 3 {
			cfg.ExposureTimes = append([]float64{1.0 / 1000.0}, cfg.ExposureTimes...)
		}
		for len(cfg.ExposureTimes) < cfg.Exposures {
			cfg.ExposureTimes = append(cfg.ExposureTimes, cfg.ExposureTimes[len(cfg.ExposureTimes)-1]*3.2)
		}
	}
	if cfg.TargetGamut == "" {
		cfg.TargetGamut = "sRGB"
	}
	if cfg.Width < 16 || cfg.Height < 16 {
		return nil, fieldErr(CodePara, "width/height", []int{cfg.Width, cfg.Height}, ">=16", "scene dimensions are too small")
	}
	if cfg.Width*cfg.Height > 40_000_000 {
		return nil, fieldErr(CodePara, "width*height", cfg.Width*cfg.Height, "<=40000000", "scene exceeds supported pixel count")
	}
	if cfg.Bands < 3 || cfg.Bands > 8 {
		return nil, fieldErr(CodePara, "bands", cfg.Bands, "3..8", "band count out of range")
	}
	if cfg.Exposures < 3 || cfg.Exposures > 9 {
		return nil, fieldErr(CodePara, "exposures", cfg.Exposures, "3..9", "exposure count out of range")
	}
	if cfg.BitDepth < 8 || cfg.BitDepth > 16 {
		return nil, fieldErr(CodePara, "bitDepth", cfg.BitDepth, "8..16", "bit depth out of range")
	}
	for i := 1; i < len(cfg.ExposureTimes); i++ {
		if cfg.ExposureTimes[i] <= cfg.ExposureTimes[i-1] {
			return nil, fieldErr(CodeOrder, fmt.Sprintf("exposureTimes[%d]", i), cfg.ExposureTimes[i], ">"+strconv.FormatFloat(cfg.ExposureTimes[i-1], 'g', -1, 64), "exposure times must be strictly increasing")
		}
	}
	if len(cfg.ExposureTimes) != cfg.Exposures {
		return nil, fieldErr(CodeBand, "exposureTimes", len(cfg.ExposureTimes), cfg.Exposures, "exposure count does not match metadata")
	}
	if len(cfg.BandCenters) != cfg.Bands {
		return nil, fieldErr(CodeBand, "bandCentersNm", len(cfg.BandCenters), cfg.Bands, "band count does not match metadata")
	}
	for b, center := range cfg.BandCenters {
		if center < 380 || center > 780 {
			return nil, fieldErr(CodeMeta, fmt.Sprintf("bands[%d].centerWavelengthNm", b), center, "380..780", "band center wavelength outside visible range")
		}
	}
	gamut := normalizeGamutName(cfg.TargetGamut)
	if _, _, ok := gamutMatrices(gamut); !ok {
		return nil, fieldErr(CodePara, "targetGamut", cfg.TargetGamut, []string{"sRGB", "DisplayP3", "Rec2020"}, "unsupported target gamut")
	}

	rand := newSplitMix(cfg.Seed)
	offsets := make([]RegistrationOffset, cfg.Bands)
	for b := 0; b < cfg.Bands; b++ {
		if b == 0 {
			offsets[b] = RegistrationOffset{FromBand: 0, ToBand: 0}
			continue
		}
		offsets[b] = RegistrationOffset{
			FromBand: 0,
			ToBand:   b,
			DX:       math.Round((rand.float()*2.8-1.4)*100) / 100,
			DY:       math.Round((rand.float()*2.8-1.4)*100) / 100,
		}
	}
	white := D65()
	bandMeta := make([]BandMeta, cfg.Bands)
	for i, center := range cfg.BandCenters {
		bandMeta[i] = BandMeta{Index: i, CenterWavelength: center, FWHM: 38}
	}
	versionSeed := fmt.Sprintf("%d-%d-%d-%d-%s-layout2", cfg.Seed, cfg.Width, cfg.Height, cfg.Bands, gamut)
	meta := Metadata{
		Width:                 cfg.Width,
		Height:                cfg.Height,
		BitDepth:              cfg.BitDepth,
		BandCount:             cfg.Bands,
		ExposureCount:         cfg.Exposures,
		Bands:                 bandMeta,
		ExposureTimes:         append([]float64(nil), cfg.ExposureTimes...),
		PixelPitchMicrons:     3.76,
		WhitePoint:            white,
		SourceWhitePoint:      white,
		ResponseGamma:         2.2,
		BandRadianceScale:     10,
		MaxRegistrationOffset: 4,
		TargetGamut:           gamut,
		DefaultGamutIntent:    IntentCompress,
		DefaultDetailStrength: 0.25,
		BandMetadataVersion:   "bands-" + versionSeed,
	}
	g := &generatorScene{
		meta:        meta,
		patches:     patchLayout(cfg.Width, cfg.Height),
		offsets:     offsets,
		radianceAmp: 1,
		detailAmp:   0.18,
	}
	maxQ := maxCode(cfg.BitDepth)
	bands := make([]BandImage, cfg.Bands)
	for b := 0; b < cfg.Bands; b++ {
		img := BandImage{
			Index:            b,
			CenterWavelength: bandMeta[b].CenterWavelength,
			Width:            cfg.Width,
			Height:           cfg.Height,
			BitDepth:         cfg.BitDepth,
			Data:             make([]uint16, cfg.Width*cfg.Height),
		}
		for y := 0; y < cfg.Height; y++ {
			for x := 0; x < cfg.Width; x++ {
				v := g.bandRadiance(b, float64(x), float64(y)) / meta.BandRadianceScale
				img.Data[y*cfg.Width+x] = uint16(math.Round(clamp(v, 0, 1) * float64(maxQ)))
			}
		}
		bands[b] = img
	}
	exposures := make([]ExposureFrame, cfg.Exposures)
	for e, t := range cfg.ExposureTimes {
		frame := ExposureFrame{
			Index:        e,
			ExposureTime: t,
			Width:        cfg.Width,
			Height:       cfg.Height,
			BitDepth:     cfg.BitDepth,
			Data:         make([]uint16, cfg.Width*cfg.Height),
		}
		for y := 0; y < cfg.Height; y++ {
			for x := 0; x < cfg.Width; x++ {
				sceneY := g.luminance(float64(x), float64(y))
				// A tiny deterministic sensor offset makes longer exposures
				// marginally noisier without dominating the radiance estimate.
				sensor := 1 + 0.00015*float64(e-1) + 0.00004*smoothNoiseOne(cfg.Seed+int64(e*31), float64(x), float64(y))
				norm := math.Pow(math.Max(0, sceneY*t*sensor), 1/meta.ResponseGamma)
				frame.Data[y*cfg.Width+x] = uint16(math.Round(clamp(norm, 0, 1) * float64(maxQ)))
			}
		}
		exposures[e] = frame
	}
	detail := make([]float64, cfg.Width*cfg.Height)
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			detail[y*cfg.Width+x] = g.detailAt(x, y)
		}
	}

	basis := buildSpectralBasis(bandMeta)
	_, xyzToRGB, _ := gamutMatrices(gamut)
	truthPatches := make([]PatchTruth, 0, len(g.patches))
	for _, p := range g.patches {
		cx := float64(p.x + p.w/2)
		cy := float64(p.y + p.h/2)
		var xyz [3]float64
		for b := 0; b < cfg.Bands; b++ {
			xyz[0] += basis.bandXYZ[b][0] * g.referenceBandRadiance(b, cx, cy)
			xyz[1] += basis.bandXYZ[b][1] * g.referenceBandRadiance(b, cx, cy)
			xyz[2] += basis.bandXYZ[b][2] * g.referenceBandRadiance(b, cx, cy)
		}
		xyz = adaptXYZ(xyz, meta.SourceWhitePoint, meta.WhitePoint)
		rgb := mulMatVec(xyzToRGB, xyz)
		rgb, _, _ = gamutMap(rgb)
		truthPatches = append(truthPatches, PatchTruth{
			ID: p.id, X: p.x, Y: p.y, Width: p.w, Height: p.h, LinearRGB: rgb,
		})
	}
	peak := 0.0
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			val := g.luminance(float64(x), float64(y)) + meta.DefaultDetailStrength*detail[y*cfg.Width+x]
			peak = math.Max(peak, math.Max(0, val))
		}
	}
	id := fmt.Sprintf("scene-%d-%dx%d-b%d-e%d-%s", cfg.Seed, cfg.Width, cfg.Height, cfg.Bands, cfg.Exposures, sceneFingerprint(cfg))
	scene := &Scene{
		ID:        id,
		Seed:      cfg.Seed,
		Meta:      meta,
		Bands:     bands,
		Exposures: exposures,
		Detail:    detail,
		Truth: Truth{
			Patches:         truthPatches,
			PeakRadiance:    peak,
			Registration:    offsets,
			WhitePoint:      white,
			DetailStrength:  meta.DefaultDetailStrength,
			Tolerance:       DefaultTolerance(),
			GamutBoundaryXY: gamutBoundary(gamut),
		},
	}
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

func sceneFingerprint(cfg GenerateConfig) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%d|%d|%d|%s|", cfg.Seed, cfg.Width, cfg.Height, cfg.Bands, cfg.Exposures, normalizeGamutName(cfg.TargetGamut))
	for _, center := range cfg.BandCenters {
		fmt.Fprintf(h, "%.6f,", center)
	}
	h.Write([]byte("|"))
	for _, exposure := range cfg.ExposureTimes {
		fmt.Fprintf(h, "%.12g,", exposure)
	}
	fmt.Fprintf(h, "|%d", cfg.BitDepth)
	sum := h.Sum64()
	return fmt.Sprintf("%08x", uint32(sum^(sum>>32)))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

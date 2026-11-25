package samples

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"ms-workbench/internal/color"
	"ms-workbench/internal/scene"
)

const (
	GeneratorVersion = "samples/v1"
	FullWellRadiance = 0.05
)

type Package struct {
	Key       string         `json:"key"`
	Scene     *scene.Scene   `json:"-"`
	Files     map[int][]byte `json:"-"`
	TruthJSON []byte         `json:"-"`
}

type Options struct {
	Seed   uint64
	Width  int
	Height int
}

var bandTemplate = []struct {
	ID       string
	CenterNM float64
	Resp     scene.Vec3
	DX       float64
	DY       float64
}{
	{"B450", 450, scene.Vec3{0.2500, 0.0600, 0.7500}, 0.00, 0.00},
	{"B530", 530, scene.Vec3{0.2000, 0.7000, 0.0200}, 1.00, -0.50},
	{"B610", 610, scene.Vec3{0.4000, 0.2000, 0.0100}, -1.50, 1.00},
	{"B680", 680, scene.Vec3{0.1000, 0.0350, 0.0300}, 0.75, 0.75},
	{"B750", 750, scene.Vec3{0.0142, 0.0050, 0.0149}, -0.50, -1.25},
}

var exposureUS = []int64{1500, 6000, 24000}

var patchTemplate = []struct {
	Name string
	Rect scene.Rect
	L    []float64
}{
	{"Studio Neutral", scene.Rect{X: 4, Y: 5, W: 9, H: 7}, []float64{0.42, 0.42, 0.42, 0.42, 0.42}},
	{"Warm Skin", scene.Rect{X: 17, Y: 5, W: 9, H: 7}, []float64{0.50, 0.46, 0.62, 0.56, 0.38}},
	{"Emerald", scene.Rect{X: 30, Y: 5, W: 9, H: 7}, []float64{0.16, 0.76, 0.34, 0.18, 0.08}},
	{"Signal Red", scene.Rect{X: 43, Y: 5, W: 9, H: 7}, []float64{0.14, 0.10, 0.86, 0.72, 0.26}},
	{"Cyan Gel", scene.Rect{X: 10, Y: 25, W: 9, H: 7}, []float64{0.28, 0.82, 0.26, 0.11, 0.06}},
	{"Spectral Magenta", scene.Rect{X: 25, Y: 25, W: 9, H: 7}, []float64{1.80, 0.15, 1.40, 0.50, 0.10}},
	{"Clip White", scene.Rect{X: 40, Y: 25, W: 9, H: 7}, []float64{3.00, 3.00, 3.00, 3.00, 3.00}},
}

func DefaultPackages(seed uint64) ([]*Package, error) {
	defs := []struct {
		Key  string
		Name string
		Seed uint64
	}{
		{"neon-foundry", "Neon Foundry", seed},
		{"glasshouse", "Glasshouse Spectral Study", seed ^ 0x9e3779b97f4a7c15},
	}
	out := make([]*Package, 0, len(defs))
	for _, def := range defs {
		p, err := Generate(Options{Seed: def.Seed, Width: 64, Height: 48}, def.Key, def.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func Generate(opt Options, key, name string) (*Package, error) {
	if opt.Width < 32 || opt.Height < 24 {
		return nil, fmt.Errorf("sample dimensions too small")
	}
	s := &scene.Scene{
		SourceVersion: 3,
		SchemaVersion: 3,
		SceneID:       key,
		Name:          name,
		Width:         opt.Width,
		Height:        opt.Height,
		BitDepth:      12,
		PixelPitchUM:  3.45,
		WhitePoint:    scene.Vec3{0.9642, 1.0, 0.8249},
		Response: scene.CameraResponse{
			InverseGamma:     2.2,
			FullWellRadiance: FullWellRadiance,
			BlackLevel:       16,
		},
		Settings: scene.DefaultSettings(),
		Present: map[string]bool{
			"render.interpolationOrder":    true,
			"render.maxRegistrationOffset": true,
			"render.fusionPower":           true,
			"render.fusionStrength":        true,
			"render.gamutIntent":           true,
			"render.targetGamut":           true,
			"render.detailGain":            true,
		},
	}
	responses := make([]scene.Vec3, len(bandTemplate))
	for i, b := range bandTemplate {
		responses[i] = b.Resp
	}
	for _, pt := range patchTemplate {
		mapped, raw, oog := color.ReconstructLinearRGB(pt.L, responses, s.WhitePoint, s.Settings.GamutIntent)
		byIntent := map[string]scene.Vec3{}
		for _, intent := range []string{"relative-colorimetric", "perceptual", "saturation"} {
			candidate, _, _ := color.ReconstructLinearRGB(pt.L, responses, s.WhitePoint, intent)
			byIntent[intent] = candidate
		}
		s.Patches = append(s.Patches, scene.Patch{
			Name:              pt.Name,
			Rect:              pt.Rect,
			BandRadiance:      append([]float64(nil), pt.L...),
			ExpectedLinearRGB: mapped,
			ExpectedByIntent:  byIntent,
			RawLinearRGB:      raw,
			OutOfGamut:        oog,
		})
		if pt.Name == "Clip White" {
			s.Truth.PeakRadiance = 3.0
		}
	}
	s.Truth.PatchTolerance = 0.0035
	reference := bandTemplate[0].ID
	for _, b := range bandTemplate[1:] {
		s.Truth.Registration = append(s.Truth.Registration, scene.RegistrationTruth{
			Reference: reference,
			Band:      b.ID,
			OffsetX:   b.DX,
			OffsetY:   b.DY,
		})
	}

	base := make([][]float64, len(bandTemplate))
	detail := make([][]float64, len(bandTemplate))
	for bi := range bandTemplate {
		base[bi] = make([]float64, opt.Width*opt.Height)
		detail[bi] = make([]float64, opt.Width*opt.Height)
		for y := 0; y < opt.Height; y++ {
			for x := 0; x < opt.Width; x++ {
				idx := y*opt.Width + x
				base[bi][idx] = field(opt.Seed, x, y, opt.Width, opt.Height, bi)
				detail[bi][idx] = detailNoise(opt.Seed, x, y, bi)
			}
		}
	}
	for pi, pt := range patchTemplate {
		for bi := range bandTemplate {
			for y := pt.Rect.Y; y < pt.Rect.Y+pt.Rect.H; y++ {
				for x := pt.Rect.X; x < pt.Rect.X+pt.Rect.W; x++ {
					if x < 0 || y < 0 || x >= opt.Width || y >= opt.Height {
						continue
					}
					idx := y*opt.Width + x
					base[bi][idx] = pt.L[bi]
					detail[bi][idx] = 0
				}
			}
			_ = pi
		}
	}
	maxCode := float64(uint32(1)<<12 - 1)
	for bi, b := range bandTemplate {
		band := scene.Band{
			ID:          b.ID,
			CenterNM:    b.CenterNM,
			ResponseXYZ: b.Resp,
			OffsetX:     b.DX,
			OffsetY:     b.DY,
			Detail:      make([]float64, opt.Width*opt.Height),
		}
		for ey, e := range exposureUS {
			band.Exposures = append(band.Exposures, scene.Exposure{
				DurationUS: e,
				Codes:      make([]uint16, opt.Width*opt.Height),
			})
			for y := 0; y < opt.Height; y++ {
				for x := 0; x < opt.Width; x++ {
					radiance := sampleGrid(base[bi], opt.Width, opt.Height, float64(x)+b.DX, float64(y)+b.DY)
					normalized := radiance * (float64(e) / 1_000_000) / s.Response.FullWellRadiance
					if normalized < 0 {
						normalized = 0
					}
					if normalized > 1 {
						normalized = 1
					}
					encoded := s.Response.BlackLevel + (maxCode-s.Response.BlackLevel)*math.Pow(normalized, 1/s.Response.InverseGamma)
					code := int(math.Round(encoded))
					if code < 0 {
						code = 0
					}
					if code > int(maxCode) {
						code = int(maxCode)
					}
					band.Exposures[ey].Codes[y*opt.Width+x] = uint16(code)
				}
			}
		}
		for y := 0; y < opt.Height; y++ {
			for x := 0; x < opt.Width; x++ {
				band.Detail[y*opt.Width+x] = sampleGrid(detail[bi], opt.Width, opt.Height, float64(x)+b.DX, float64(y)+b.DY)
			}
		}
		s.Bands = append(s.Bands, band)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}

	pkg := &Package{Key: key, Scene: s, Files: map[int][]byte{}}
	for version := 1; version <= 3; version++ {
		data, _, err := scene.ExportAllowLossy(s, version)
		if err != nil {
			return nil, err
		}
		pkg.Files[version] = data
	}
	truth := map[string]any{
		"generator":      GeneratorVersion,
		"seed":           opt.Seed,
		"sceneID":        key,
		"peakRadiance":   s.Truth.PeakRadiance,
		"patchTolerance": s.Truth.PatchTolerance,
		"patches":        s.Patches,
		"registration":   s.Truth.Registration,
	}
	pkg.TruthJSON, _ = json.MarshalIndent(truth, "", "  ")
	return pkg, nil
}

func field(seed uint64, x, y, w, h, band int) float64 {
	u := float64(x) / float64(w-1)
	v := float64(y) / float64(h-1)
	phase := float64(band)*0.61 + float64(seed%997)/997.0*math.Pi*2
	base := 0.055 + 0.10*(0.5+0.5*math.Sin(u*9.2+phase))*(0.35+0.65*v)
	base += 0.055 * (0.5 + 0.5*math.Cos(v*13.7-phase*0.8))
	cx := 0.24 + 0.09*math.Sin(phase)
	cy := 0.32 + 0.08*math.Cos(phase*1.3)
	dx, dy := u-cx, v-cy
	base += 0.48 * math.Exp(-(dx*dx+dy*dy)/0.028)
	cx2 := 0.76 + 0.06*math.Cos(phase*0.7)
	cy2 := 0.68 + 0.06*math.Sin(phase)
	dx2, dy2 := u-cx2, v-cy2
	base += 0.26 * math.Exp(-(dx2*dx2+dy2*dy2)/0.010)
	check := float64((x/7+y/5+band)%2) * 0.012
	return base + check
}

func detailNoise(seed uint64, x, y, band int) float64 {
	u := splitmix(seed + uint64(x)*0x9e3779b97f4a7c15 + uint64(y)*0xd1b54a32d192ed03 + uint64(band)*0x94d049bb133111eb)
	return (float64(u>>11)/float64(uint64(1)<<53) - 0.5) * 0.003
}

func splitmix(v uint64) uint64 {
	v += 0x9e3779b97f4a7c15
	v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
	v = (v ^ (v >> 27)) * 0x94d049bb133111eb
	return v ^ (v >> 31)
}

func sampleGrid(grid []float64, w, h int, x, y float64) float64 {
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	fx := x - float64(x0)
	fy := y - float64(y0)
	at := func(xx, yy int) float64 {
		if xx < 0 {
			xx = 0
		}
		if yy < 0 {
			yy = 0
		}
		if xx >= w {
			xx = w - 1
		}
		if yy >= h {
			yy = h - 1
		}
		return grid[yy*w+xx]
	}
	a := at(x0, y0)*(1-fx) + at(x0+1, y0)*fx
	b := at(x0, y0+1)*(1-fx) + at(x0+1, y0+1)*fx
	return a*(1-fy) + b*fy
}

func Fingerprint(files map[int][]byte) string {
	h := sha256.New()
	for v := 1; v <= 3; v++ {
		h.Write([]byte(fmt.Sprintf("v%d:%d:", v, len(files[v]))))
		h.Write(files[v])
	}
	return hex.EncodeToString(h.Sum(nil))
}

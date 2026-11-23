package synth

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
)

const GeneratorVersion = "synthtext-local/1.0"

type lineSpec struct {
	text  string
	scale int
	xf    float64
	yf    float64
	angle float64
}

type sampleSpec struct {
	id          string
	title       string
	description string
	width       int
	height      int
	channels    int
	lowContrast bool
	noise       int
	lines       []lineSpec
}

var sampleSpecs = []sampleSpec{
	{
		id:          "sparse_single_960x540",
		title:       "Sparse single line",
		description: "RGBA game HUD frame with one sparse, slightly tilted text line.",
		width:       960,
		height:      540,
		channels:    4,
		noise:       4,
		lines: []lineSpec{
			{text: "STAGE-01", scale: 10, xf: 0.50, yf: 0.46, angle: -2.0},
		},
	},
	{
		id:          "multiline_1280x720",
		title:       "Multiline HUD",
		description: "Four-channel three-line layout with mild deterministic skew.",
		width:       1280,
		height:      720,
		channels:    4,
		noise:       6,
		lines: []lineSpec{
			{text: "NEW-GAME", scale: 7, xf: 0.34, yf: 0.27, angle: 1.4},
			{text: "HIGH-SCORE", scale: 7, xf: 0.61, yf: 0.50, angle: -1.1},
			{text: "PRESS-START", scale: 6, xf: 0.47, yf: 0.72, angle: 2.2},
		},
	},
	{
		id:          "tilted_low_1024x640",
		title:       "Tilted low contrast",
		description: "Two tilted lines rendered with deliberately low foreground/background contrast.",
		width:       1024,
		height:      640,
		channels:    4,
		lowContrast: true,
		noise:       5,
		lines: []lineSpec{
			{text: "LOW-CONTRAST", scale: 6, xf: 0.44, yf: 0.37, angle: -6.5},
			{text: "STAGE-07", scale: 9, xf: 0.61, yf: 0.69, angle: 5.0},
		},
	},
	{
		id:          "dense_1600x900",
		title:       "Dense multiline",
		description: "Six dense text lines to exercise merge ordering and reading indices.",
		width:       1600,
		height:      900,
		channels:    4,
		noise:       7,
		lines: []lineSpec{
			{text: "SCORE-0123456789", scale: 7, xf: 0.31, yf: 0.16, angle: -0.7},
			{text: "WAVE-ALPHA-0042", scale: 7, xf: 0.38, yf: 0.29, angle: 0.5},
			{text: "PLAYER-ONE-READY", scale: 7, xf: 0.45, yf: 0.42, angle: -0.4},
			{text: "ENEMY-COUNT-1200", scale: 7, xf: 0.61, yf: 0.56, angle: 0.8},
			{text: "BONUS-STAGE-CLEAR", scale: 7, xf: 0.57, yf: 0.70, angle: -1.0},
			{text: "CONTINUE-09-SECONDS", scale: 6, xf: 0.53, yf: 0.84, angle: 0.3},
		},
	},
	{
		id:          "sparse_gray_800x600",
		title:       "Sparse grayscale",
		description: "Single-channel grayscale image with two short sparse HUD values.",
		width:       800,
		height:      600,
		channels:    1,
		noise:       8,
		lines: []lineSpec{
			{text: "HP-100", scale: 9, xf: 0.31, yf: 0.36, angle: 0.9},
			{text: "LVL-09", scale: 9, xf: 0.68, yf: 0.64, angle: -1.3},
		},
	},
}

func Generate(dir string, seed int64) (domain.Manifest, *domain.AppError) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return domain.Manifest{}, domain.DecodeError("cannot create sample directory", err)
	}
	manifest := domain.Manifest{
		SchemaVersion:    domain.ManifestSchema,
		GeneratorVersion: GeneratorVersion,
		Seed:             seed,
		FontVersion:      fontlib.Version,
		GeneratedLocally: true,
		NetworkUsed:      false,
		Samples:          make([]domain.SampleMeta, 0, len(sampleSpecs)),
	}
	for i, spec := range sampleSpecs {
		sampleSeed := seed + int64((i+1)*104729)
		meta, appErr := generateSample(dir, spec, sampleSeed)
		if appErr != nil {
			return domain.Manifest{}, appErr
		}
		manifest.Samples = append(manifest.Samples, meta)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return domain.Manifest{}, domain.DecodeError("cannot encode manifest", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		return domain.Manifest{}, domain.DecodeError("cannot write manifest", err)
	}
	return manifest, nil
}

func Ensure(dir string, seed int64) (domain.Manifest, *domain.AppError) {
	if manifest, appErr := imaging.ReadManifest(dir); appErr == nil {
		valid := true
		for _, sample := range manifest.Samples {
			if !imaging.SampleFileExists(dir, sample.ID) {
				valid = false
				break
			}
		}
		if valid {
			return manifest, nil
		}
	}
	return Generate(dir, seed)
}

func generateSample(dir string, spec sampleSpec, seed int64) (domain.SampleMeta, *domain.AppError) {
	rng := rand.New(rand.NewSource(seed))
	canvas := image.NewNRGBA(image.Rect(0, 0, spec.width, spec.height))
	drawBackground(canvas, rng, spec.lowContrast, spec.noise)

	truth := domain.TruthFile{
		SchemaVersion: domain.TruthSchema,
		ImageID:       spec.id,
		Width:         spec.width,
		Height:        spec.height,
		Channels:      spec.channels,
		RegionCount:   len(spec.lines),
		Regions:       make([]domain.TruthRegion, 0, len(spec.lines)),
	}
	maxAngle := 0.0
	for i, line := range spec.lines {
		if math.Abs(line.angle) > maxAngle {
			maxAngle = math.Abs(line.angle)
		}
		region := drawLine(canvas, line, i, rng, spec.lowContrast)
		truth.Regions = append(truth.Regions, region)
	}

	imageName := spec.id + ".png"
	truthName := spec.id + ".truth.json"
	imagePath := filepath.Join(dir, imageName)
	truthPath := filepath.Join(dir, truthName)
	outputImage := image.Image(canvas)
	if spec.channels == 1 {
		outputImage = imaging.ToGray(canvas)
	}
	if err := writePNG(imagePath, outputImage); err != nil {
		return domain.SampleMeta{}, domain.DecodeError("cannot write generated image", err)
	}
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return domain.SampleMeta{}, domain.DecodeError("cannot encode truth", err)
	}
	truthData = append(truthData, '\n')
	if err := os.WriteFile(truthPath, truthData, 0o644); err != nil {
		return domain.SampleMeta{}, domain.DecodeError("cannot write truth", err)
	}

	return domain.SampleMeta{
		ID:             spec.id,
		Title:          spec.title,
		Description:    spec.description,
		Image:          imageName,
		Truth:          truthName,
		Width:          spec.width,
		Height:         spec.height,
		Channels:       spec.channels,
		RegionCount:    len(spec.lines),
		LineCount:      len(spec.lines),
		Seed:           seed,
		Generator:      GeneratorVersion,
		RotationDegree: domain.Round4(maxAngle),
		ImageHash:      imaging.HashImage(outputImage),
	}, nil
}

func drawBackground(canvas *image.NRGBA, rng *rand.Rand, lowContrast bool, noise int) {
	var bg, fg [3]int
	if lowContrast {
		base := 136 + rng.Intn(22)
		bg = [3]int{base, base + 1, base - 2}
		fg = [3]int{72 + rng.Intn(18), 70 + rng.Intn(18), 76 + rng.Intn(18)}
	} else {
		base := 17 + rng.Intn(18)
		bg = [3]int{base, base + 2, base + 6}
		fg = [3]int{218 + rng.Intn(28), 222 + rng.Intn(24), 205 + rng.Intn(35)}
	}
	_ = fg
	b := canvas.Bounds()
	for y := 0; y < b.Dy(); y++ {
		gradient := int(float64(y) / float64(maxInt(1, b.Dy())) * 8)
		for x := 0; x < b.Dx(); x++ {
			n := rng.Intn(2*noise+1) - noise
			if y%5 == 0 {
				n -= 1
			}
			canvas.SetNRGBA(x, y, color.NRGBA{
				R: clampByte(bg[0] + gradient + n),
				G: clampByte(bg[1] + gradient + n),
				B: clampByte(bg[2] + gradient + n/2),
				A: 255,
			})
		}
	}
}

var fgPalettes = [][3]uint8{
	{235, 241, 255},
	{255, 224, 154},
	{180, 246, 226},
	{244, 197, 255},
	{255, 255, 255},
}

func drawLine(canvas *image.NRGBA, spec lineSpec, index int, rng *rand.Rand, lowContrast bool) domain.TruthRegion {
	scale := spec.scale
	runes := []rune(spec.text)
	width := fontlib.TextCellWidth(spec.text) * scale
	height := fontlib.CellHeight * scale
	layer := image.NewNRGBA(image.Rect(0, 0, width, height))
	var fg color.NRGBA
	if lowContrast {
		fg = color.NRGBA{R: uint8(72 + rng.Intn(18)), G: uint8(68 + rng.Intn(18)), B: uint8(78 + rng.Intn(16)), A: 255}
	} else {
		pal := fgPalettes[rng.Intn(len(fgPalettes))]
		jitter := rng.Intn(11) - 5
		fg = color.NRGBA{R: clampByte(int(pal[0]) + jitter), G: clampByte(int(pal[1]) + jitter), B: clampByte(int(pal[2]) + jitter), A: 255}
	}
	cellX := 0
	for _, r := range runes {
		glyph, ok := fontlib.GlyphFor(r)
		if !ok {
			continue
		}
		baseX := cellX * scale
		cellX += glyph.Advance()
		for gy, row := range glyph.Pattern {
			for gx, ch := range row {
				if ch != '#' {
					continue
				}
				for py := 0; py < scale; py++ {
					for px := 0; px < scale; px++ {
						x := baseX + gx*scale + px
						y := gy*scale + py
						if image.Pt(x, y).In(layer.Bounds()) {
							layer.SetNRGBA(x, y, fg)
						}
					}
				}
			}
		}
	}
	rotated, rotatedW, rotatedH := rotateLayer(layer, spec.angle)
	centerX := int(math.Round(float64(canvas.Bounds().Dx()) * spec.xf))
	centerY := int(math.Round(float64(canvas.Bounds().Dy()) * spec.yf))
	margin := 8 + 2*scale
	halfW := rotatedW / 2
	halfH := rotatedH / 2
	centerX = clampInt(centerX, halfW+margin, canvas.Bounds().Dx()-halfW-margin)
	centerY = clampInt(centerY, halfH+margin, canvas.Bounds().Dy()-halfH-margin)
	x0 := centerX - rotatedW/2
	y0 := centerY - rotatedH/2
	for y := 0; y < rotatedH; y++ {
		for x := 0; x < rotatedW; x++ {
			p := rotated.NRGBAAt(x, y)
			if p.A == 0 {
				continue
			}
			dx, dy := x0+x, y0+y
			if image.Pt(dx, dy).In(canvas.Bounds()) {
				canvas.SetNRGBA(dx, dy, p)
			}
		}
	}
	corners := [4]domain.Point{
		{X: -width / 2, Y: -height / 2},
		{X: width - width/2, Y: -height / 2},
		{X: width - width/2, Y: height - height/2},
		{X: -width / 2, Y: height - height/2},
	}
	angle := spec.angle * math.Pi / 180
	cosA, sinA := math.Cos(angle), math.Sin(angle)
	var quad [4]domain.Point
	minX, minY := canvas.Bounds().Dx(), canvas.Bounds().Dy()
	maxX, maxY := 0, 0
	for i, corner := range corners {
		rx := float64(corner.X)*cosA - float64(corner.Y)*sinA
		ry := float64(corner.X)*sinA + float64(corner.Y)*cosA
		px := int(math.Round(rx)) + centerX
		py := int(math.Round(ry)) + centerY
		quad[i] = domain.Point{X: px, Y: py}
		minX = minInt(minX, px)
		minY = minInt(minY, py)
		maxX = maxInt(maxX, px)
		maxY = maxInt(maxY, py)
	}
	return domain.TruthRegion{
		ID:             fmt.Sprintf("%s-line-%02d", strings.TrimSpace(spec.text), index),
		LineIndex:      index,
		Text:           spec.text,
		CharCount:      len(runes),
		AvgStrokeWidth: float64(scale),
		Quad:           quad,
		BBox:           domain.Rect{X: minX, Y: minY, W: maxX - minX, H: maxY - minY},
	}
}

func rotateLayer(src *image.NRGBA, degrees float64) (*image.NRGBA, int, int) {
	if math.Abs(degrees) < 1e-9 {
		b := src.Bounds()
		return src, b.Dx(), b.Dy()
	}
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	angle := degrees * math.Pi / 180
	cosA, sinA := math.Cos(angle), math.Sin(angle)
	dw := int(math.Ceil(math.Abs(float64(sw)*cosA) + math.Abs(float64(sh)*sinA)))
	dh := int(math.Ceil(math.Abs(float64(sw)*sinA) + math.Abs(float64(sh)*cosA)))
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	scx, scy := float64(sw-1)/2, float64(sh-1)/2
	dcx, dcy := float64(dw-1)/2, float64(dh-1)/2
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			dx, dy := float64(x)-dcx, float64(y)-dcy
			sx := dx*cosA + dy*sinA + scx
			sy := -dx*sinA + dy*cosA + scy
			ix, iy := int(math.Round(sx)), int(math.Round(sy))
			if ix < 0 || iy < 0 || ix >= sw || iy >= sh {
				continue
			}
			p := src.NRGBAAt(ix, iy)
			if p.A > 0 {
				dst.SetNRGBA(x, y, p)
			}
		}
	}
	return dst, dw, dh
}

func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	return encoder.Encode(file, img)
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return (lo + hi) / 2
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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

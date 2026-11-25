package ocrflow

import (
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"unicode/utf8"
)

const FixedSeed int64 = 42

type TruthLine struct {
	LineNo                int    `json:"line_no"`
	LineID                string `json:"line_id"`
	Text                  string `json:"text"`
	Quad                  Quad   `json:"quad"`
	CharCount             int    `json:"char_count"`
	AvgStrokeWidthPx      int    `json:"avg_stroke_width_px"`
	AvgStrokeWidthMilliPx int    `json:"avg_stroke_width_milli_px"`
	ContrastBPS           int    `json:"contrast_bps"`
}

type Screenshot struct {
	ShotID      string      `json:"shot_id"`
	Seed        int64       `json:"seed"`
	Width       int         `json:"width"`
	Height      int         `json:"height"`
	ContentHash string      `json:"content_hash"`
	Lines       []TruthLine `json:"lines"`
	RGBA        *image.RGBA `json:"-"`
}

type LineSpec struct {
	Text        string
	Scale       int
	X           int
	Y           int
	TopShift    int
	BottomShift int
	ContrastBPS int
}

type shotSpec struct {
	id    string
	lines []LineSpec
}

func DefaultShotSpecs() []shotSpec {
	return []shotSpec{
		{
			id: "shot-0001",
			lines: []LineSpec{
				{Text: "VICTORY", Scale: 7, X: 216, Y: 176, TopShift: -3, BottomShift: 3, ContrastBPS: 8200},
			},
		},
		{
			id: "shot-0002",
			lines: []LineSpec{
				{Text: "SCORE 12345", Scale: 4, X: 72, Y: 58, TopShift: -2, BottomShift: 1, ContrastBPS: 7600},
				{Text: "RANK 01", Scale: 4, X: 72, Y: 176, TopShift: 1, BottomShift: -2, ContrastBPS: 6800},
				{Text: "MVP 07", Scale: 4, X: 72, Y: 294, TopShift: -1, BottomShift: 2, ContrastBPS: 6200},
			},
		},
		{
			id: "shot-0003",
			lines: []LineSpec{
				{Text: "PLAYER01 9820", Scale: 4, X: 46, Y: 48, TopShift: -1, BottomShift: 2, ContrastBPS: 7400},
				{Text: "PLAYER02 8765", Scale: 4, X: 46, Y: 166, TopShift: 2, BottomShift: -1, ContrastBPS: 6900},
				{Text: "PLAYER03 7650", Scale: 4, X: 46, Y: 284, TopShift: -2, BottomShift: 1, ContrastBPS: 6400},
			},
		},
		{
			id: "shot-0004",
			lines: []LineSpec{
				{Text: "DEFEAT", Scale: 7, X: 238, Y: 176, TopShift: 3, BottomShift: -3, ContrastBPS: 5600},
			},
		},
	}
}

// GenerateSampleSet deterministically creates all screenshots and their only
// authoritative truth records from the generator's construction.
func GenerateSampleSet(seed int64) ([]*Screenshot, error) {
	if seed == 0 {
		seed = FixedSeed
	}
	specs := DefaultShotSpecs()
	shots := make([]*Screenshot, 0, len(specs))
	for i, spec := range specs {
		shotSeed := seed + int64(i*1009)
		img := image.NewRGBA(image.Rect(0, 0, 720, 420))
		rng := rand.New(rand.NewSource(shotSeed))
		drawBackground(img, rng)
		truth, err := drawSpecLines(img, spec.lines)
		if err != nil {
			return nil, err
		}
		hash := HashRGBA(img)
		shots = append(shots, &Screenshot{
			ShotID:      spec.id,
			Seed:        shotSeed,
			Width:       720,
			Height:      420,
			ContentHash: hash,
			Lines:       truth,
			RGBA:        img,
		})
	}
	return shots, nil
}

func backgroundAt(x, y int) color.RGBA {
	const w, h = 720, 420
	t := y * 10000 / h
	if x > w/2 {
		t = (t + x*1500/w) % 10000
	}
	r := 18 + 34*t/10000
	g := 24 + 42*t/10000
	b := 48 + 58*t/10000
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
}

func drawBackground(img *image.RGBA, rng *rand.Rand) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.SetRGBA(x, y, backgroundAt(x, y))
		}
	}

	edge := color.RGBA{R: 86, G: 118, B: 170, A: 255}
	drawRectOutline(img, 18, 18, 701, 401, 2, edge)
	drawRectOutline(img, 30, 30, 689, 390, 1, color.RGBA{R: 54, G: 76, B: 122, A: 255})
	drawFilledRect(img, 36, 36, 684, 8, color.RGBA{R: 34, G: 48, B: 92, A: 255})
	drawFilledRect(img, 36, 376, 684, 8, color.RGBA{R: 34, G: 48, B: 92, A: 255})

	for i := 0; i < 22; i++ {
		x := 40 + rng.Intn(620)
		y := 54 + rng.Intn(310)
		w := 8 + rng.Intn(46)
		h := 4 + rng.Intn(28)
		c := color.RGBA{
			R: uint8(28 + rng.Intn(28)),
			G: uint8(40 + rng.Intn(34)),
			B: uint8(78 + rng.Intn(42)),
			A: 255,
		}
		switch rng.Intn(3) {
		case 0:
			drawFilledRect(img, x, y, w, h, c)
		case 1:
			drawRectOutline(img, x, y, x+w, y+h, 1, c)
		default:
			drawCircle(img, x+w/2, y+h/2, 3+rng.Intn(12), c)
		}
	}

	drawLine(img, 42, 350, 678, 72, color.RGBA{R: 42, G: 60, B: 108, A: 255})
	drawLine(img, 42, 74, 678, 352, color.RGBA{R: 38, G: 55, B: 100, A: 255})
}

func blendToward(fg, bg color.RGBA, contrastBPS int) color.RGBA {
	t := clampInt(contrastBPS, 0, 10000)
	mix := func(a, b uint8) uint8 {
		return uint8((int(a)*t + int(b)*(10000-t)) / 10000)
	}
	return color.RGBA{R: mix(fg.R, bg.R), G: mix(fg.G, bg.G), B: mix(fg.B, bg.B), A: 255}
}

func drawSpecLines(img *image.RGBA, specs []LineSpec) ([]TruthLine, error) {
	out := make([]TruthLine, 0, len(specs))
	fg := color.RGBA{R: 238, G: 242, B: 248, A: 255}
	for i, spec := range specs {
		if spec.Scale <= 0 {
			return nil, schemaError(FixedSeedVersion, fmt.Sprintf("lines[%d].scale", i), "scale must be positive")
		}
		if spec.Text == "" {
			return nil, schemaError(FixedSeedVersion, fmt.Sprintf("lines[%d].text", i), "truth text must not be empty")
		}
		runes := []rune(spec.Text)
		for _, r := range runes {
			if _, ok := bitmapFont[r]; !ok {
				return nil, schemaError(FixedSeedVersion, fmt.Sprintf("lines[%d].text", i), "unsupported deterministic glyph %q", r)
			}
		}
		for idx, r := range runes {
			glyph := bitmapFont[r]
			for row := 0; row < 7; row++ {
				for col := 0; col < 5; col++ {
					if glyph[row][col] != '#' {
						continue
					}
					baseX := spec.X + idx*6*spec.Scale + col*spec.Scale
					y0 := spec.Y + row*spec.Scale
					bg := backgroundAt(baseX+spec.Scale/2, y0+spec.Scale/2)
					c := blendToward(fg, bg, spec.ContrastBPS)
					for dy := 0; dy < spec.Scale; dy++ {
						relY := row*spec.Scale + dy
						shift := spec.TopShift + (spec.BottomShift-spec.TopShift)*relY/(7*spec.Scale)
						for dx := 0; dx < spec.Scale; dx++ {
							img.SetRGBA(baseX+dx+shift, y0+dy, c)
						}
					}
				}
			}
		}
		unitsW := len(runes)*6 - 1
		w := unitsW * spec.Scale
		h := 7 * spec.Scale
		quad := Quad{
			{X: spec.X + spec.TopShift, Y: spec.Y},
			{X: spec.X + w + spec.TopShift, Y: spec.Y},
			{X: spec.X + w + spec.BottomShift, Y: spec.Y + h},
			{X: spec.X + spec.BottomShift, Y: spec.Y + h},
		}
		if !ValidQuad(quad) {
			return nil, schemaError(FixedSeedVersion, fmt.Sprintf("lines[%d].quad", i), "generated quad is degenerate")
		}
		out = append(out, TruthLine{
			LineNo:                i + 1,
			LineID:                fmt.Sprintf("L%02d", i+1),
			Text:                  spec.Text,
			Quad:                  quad,
			CharCount:             utf8.RuneCountInString(spec.Text),
			AvgStrokeWidthPx:      spec.Scale,
			AvgStrokeWidthMilliPx: spec.Scale * 1000,
			ContrastBPS:           spec.ContrastBPS,
		})
	}
	return out, nil
}

func drawFilledRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			img.SetRGBA(xx, yy, c)
		}
	}
}

func drawRectOutline(img *image.RGBA, x1, y1, x2, y2, thick int, c color.RGBA) {
	drawFilledRect(img, x1, y1, x2-x1, thick, c)
	drawFilledRect(img, x1, y2-thick, x2-x1, thick, c)
	drawFilledRect(img, x1, y1, thick, y2-y1, c)
	drawFilledRect(img, x2-thick, y1, thick, y2-y1, c)
}

func drawLine(img *image.RGBA, x1, y1, x2, y2 int, c color.RGBA) {
	dx := absInt(x2 - x1)
	dy := -absInt(y2 - y1)
	sx := 1
	if x1 > x2 {
		sx = -1
	}
	sy := 1
	if y1 > y2 {
		sy = -1
	}
	err := dx + dy
	for {
		img.SetRGBA(x1, y1, c)
		if x1 == x2 && y1 == y2 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x1 += sx
		}
		if e2 <= dx {
			err += dx
			y1 += sy
		}
	}
}

func drawCircle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r && x*x+y*y >= (r-1)*(r-1) {
				img.SetRGBA(cx+x, cy+y, c)
			}
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

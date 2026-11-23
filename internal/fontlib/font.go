package fontlib

import (
	"math"
	"sort"

	"ocrworkbench/internal/domain"
)

const (
	Version    = "bitmap5x7-v1"
	CellHeight = 7
)

type Glyph struct {
	Rune    rune
	Pattern [CellHeight]string
}

func (g Glyph) Width() int {
	width := 0
	for _, row := range g.Pattern {
		if len(row) > width {
			width = len(row)
		}
	}
	return width
}

func (g Glyph) Advance() int {
	return g.Width() + 1
}

func TextCellWidth(text string) int {
	width := 0
	count := 0
	for _, r := range text {
		glyph, ok := GlyphFor(r)
		if !ok {
			continue
		}
		width += glyph.Advance()
		count++
	}
	if count > 0 {
		width--
	}
	return width
}

type Template struct {
	Rune   rune
	Width  int
	Height int
	Mask   []float64
	Pixels int
}

type Library struct {
	Version   string
	Templates []Template
	byRune    map[rune]Template
}

var glyphList = []Glyph{
	{'A', [7]string{".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"}},
	{'B', [7]string{"####.", "#...#", "#...#", "####.", "#...#", "#...#", "####."}},
	{'C', [7]string{".###.", "#...#", "#....", "#....", "#....", "#...#", ".###."}},
	{'D', [7]string{"######.", "#.....#", "#.....#", "#.....#", "#.....#", "#.....#", "######."}},
	{'E', [7]string{"#####", "#....", "#....", "####.", "#....", "#....", "#####"}},
	{'F', [7]string{"#####", "#....", "#....", "####.", "#....", "#....", "#...."}},
	{'G', [7]string{".###.", "#...#", "#....", "#.###", "#...#", "#...#", ".###."}},
	{'H', [7]string{"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"}},
	{'I', [7]string{"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "#####"}},
	{'J', [7]string{"..###", "...#.", "...#.", "...#.", "...#.", "#..#.", ".##.."}},
	{'K', [7]string{"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"}},
	{'L', [7]string{"#....", "#....", "#....", "#....", "#....", "#....", "#####"}},
	{'M', [7]string{"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"}},
	{'N', [7]string{"#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#", "#...#"}},
	{'O', [7]string{"..###..", ".#...#.", "#.....#", "#.....#", "#.....#", ".#...#.", "..###.."}},
	{'P', [7]string{"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."}},
	{'Q', [7]string{".###.", "#...#", "#...#", "#...#", "#.#.#", "#..#.", ".##.#"}},
	{'R', [7]string{"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"}},
	{'S', [7]string{".###.", "#...#", "#....", ".###.", "....#", "#...#", ".###."}},
	{'T', [7]string{"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."}},
	{'U', [7]string{"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."}},
	{'V', [7]string{"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."}},
	{'W', [7]string{"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "##.##", "#...#"}},
	{'X', [7]string{"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"}},
	{'Y', [7]string{"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."}},
	{'Z', [7]string{"#####", "....#", "...#.", "..#..", ".#...", "#....", "#####"}},
	{'0', [7]string{".###.", "#...#", "#..##", "#.#.#", "##..#", "#...#", ".###."}},
	{'1', [7]string{"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."}},
	{'2', [7]string{".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"}},
	{'3', [7]string{"####.", "....#", "....#", ".###.", "....#", "....#", "####."}},
	{'4', [7]string{"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."}},
	{'5', [7]string{"#####", "#....", "####.", "....#", "....#", "#...#", ".###."}},
	{'6', [7]string{"..##.", ".#...", "#....", "####.", "#...#", "#...#", ".###."}},
	{'7', [7]string{"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."}},
	{'8', [7]string{".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."}},
	{'9', [7]string{".###.", "#...#", "#...#", ".####", "....#", "...#.", ".##.."}},
	{'-', [7]string{".....", ".....", ".....", "#####", ".....", ".....", "....."}},
	{'.', [7]string{".....", ".....", ".....", ".....", ".....", "..#..", "..#.."}},
	{':', [7]string{".....", "..#..", "..#..", ".....", "..#..", "..#..", "....."}},
	{'!', [7]string{"..#..", "..#..", "..#..", "..#..", "..#..", ".....", "..#.."}},
	{'?', [7]string{".###.", "#...#", "....#", "...#.", "..#..", ".....", "..#.."}},
	{'_', [7]string{".....", ".....", ".....", ".....", ".....", ".....", "#####"}},
}

var glyphs = func() map[rune]Glyph {
	m := make(map[rune]Glyph, len(glyphList))
	for _, glyph := range glyphList {
		m[glyph.Rune] = glyph
	}
	return m
}()

func GlyphFor(r rune) (Glyph, bool) {
	glyph, ok := glyphs[r]
	return glyph, ok
}

func Charset() []rune {
	chars := make([]rune, 0, len(glyphList))
	for _, glyph := range glyphList {
		chars = append(chars, glyph.Rune)
	}
	sort.Slice(chars, func(i, j int) bool { return chars[i] < chars[j] })
	return chars
}

func Load(version string, templateWidth, templateHeight int) (*Library, *domain.AppError) {
	if version != Version {
		e := domain.Errorf(domain.CodeTmpl, "template library version is unavailable")
		e.Field = "template_version"
		e.Expected = Version
		e.Actual = version
		return nil, e
	}
	if templateWidth < 8 || templateHeight < 8 {
		e := domain.Errorf(domain.CodeTmpl, "template dimensions are invalid")
		e.Field = "split.template_size"
		e.Expected = ">=8x8"
		e.Actual = "smaller"
		return nil, e
	}
	lib := &Library{Version: version, byRune: make(map[rune]Template, len(glyphList))}
	for _, glyph := range glyphList {
		if glyph.Rune == '_' {
			// The underscore pattern is intentionally outside the generated
			// corpus and has no stable tight aspect ratio after normalization.
			continue
		}
		w, h, mask := patternMask(glyph.Pattern)
		if w == 0 || h == 0 {
			continue
		}
		normalized := NormalizeMask(mask, w, h, templateWidth, templateHeight, 0.12)
		tmpl := Template{
			Rune:   glyph.Rune,
			Width:  templateWidth,
			Height: templateHeight,
			Mask:   normalized,
			Pixels: countMask(normalized),
		}
		lib.Templates = append(lib.Templates, tmpl)
		lib.byRune[glyph.Rune] = tmpl
	}
	if len(lib.Templates) == 0 {
		return nil, domain.Errorf(domain.CodeTmpl, "template library contains no usable templates")
	}
	return lib, nil
}

func (l *Library) TemplateFor(r rune) (Template, bool) {
	t, ok := l.byRune[r]
	return t, ok
}

// Match returns a Dice coefficient with a one-pixel tolerance. The score is in
// [0,1] and increases monotonically when the candidate mask becomes closer to
// a template.
func (l *Library) Match(mask []float64, width, height int) (Template, float64, Template, float64) {
	type scored struct {
		t     Template
		score float64
	}
	scores := make([]scored, 0, len(l.Templates))
	for _, template := range l.Templates {
		scores = append(scores, scored{t: template, score: Similarity(mask, width, height, template.Mask, template.Width, template.Height)})
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		return scores[i].t.Rune < scores[j].t.Rune
	})
	if len(scores) == 0 {
		return Template{}, 0, Template{}, 0
	}
	if len(scores) == 1 {
		return scores[0].t, scores[0].score, Template{}, 0
	}
	return scores[0].t, scores[0].score, scores[1].t, scores[1].score
}

func Similarity(a []float64, aw, ah int, b []float64, bw, bh int) float64 {
	if aw != bw || ah != bh || aw <= 0 || ah <= 0 {
		return 0
	}
	var sumA, sumB float64
	for y := 0; y < ah; y++ {
		for x := 0; x < aw; x++ {
			sumA += a[y*aw+x]
			sumB += b[y*bw+x]
		}
	}
	if sumA+sumB == 0 {
		return 1
	}
	best := 0.0
	for shiftY := -2; shiftY <= 2; shiftY++ {
		for shiftX := -2; shiftX <= 2; shiftX++ {
			exact, matchedA, matchedB := 0.0, 0.0, 0.0
			for y := 0; y < ah; y++ {
				for x := 0; x < aw; x++ {
					if a[y*aw+x] <= 0.5 {
						continue
					}
					if shiftedValue(b, bw, bh, x, y, shiftX, shiftY) > 0.5 {
						exact++
						matchedA++
					} else if shiftedNeighborOn(b, bw, bh, x, y, shiftX, shiftY) {
						matchedA++
					}
				}
			}
			for y := 0; y < bh; y++ {
				for x := 0; x < bw; x++ {
					if b[y*bw+x] <= 0.5 {
						continue
					}
					if shiftedNeighborOn(a, aw, ah, x, y, -shiftX, -shiftY) {
						matchedB++
					}
				}
			}
			exactDice := 2 * exact / (sumA + sumB)
			tolerantDice := (matchedA + matchedB) / (sumA + sumB)
			cornerScore := cornerSimilarity(a, aw, ah, b, bw, bh, shiftX, shiftY)
			score := 0.60*exactDice + 0.25*tolerantDice + 0.15*cornerScore
			if score > best {
				best = score
			}
		}
	}
	return domain.Clamp01(best)
}

func cornerSimilarity(a []float64, aw, ah int, b []float64, bw, bh int, shiftX, shiftY int) float64 {
	cw, ch := maxInt(2, aw/4), maxInt(2, ah/4)
	corners := [4][2]int{
		{0, 0},
		{aw - cw, 0},
		{0, ah - ch},
		{aw - cw, ah - ch},
	}
	diff := 0.0
	for _, corner := range corners {
		aRatio := regionRatio(a, aw, ah, corner[0], corner[1], cw, ch, 0, 0)
		bRatio := regionRatio(b, bw, bh, corner[0], corner[1], cw, ch, shiftX, shiftY)
		diff += math.Abs(aRatio - bRatio)
	}
	return domain.Clamp01(1 - diff/float64(len(corners)))
}

func regionRatio(mask []float64, width, height, x0, y0, w, h, shiftX, shiftY int) float64 {
	if w <= 0 || h <= 0 {
		return 0
	}
	count := 0
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			if shiftedValue(mask, width, height, x, y, shiftX, shiftY) > 0.5 {
				count++
			}
		}
	}
	return float64(count) / float64(w*h)
}

func shiftedValue(mask []float64, width, height, x, y, shiftX, shiftY int) float64 {
	nx, ny := x+shiftX, y+shiftY
	if nx < 0 || ny < 0 || nx >= width || ny >= height {
		return 0
	}
	return mask[ny*width+nx]
}

func shiftedNeighborOn(mask []float64, width, height, x, y, shiftX, shiftY int) bool {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if shiftedValue(mask, width, height, x, y, shiftX+dx, shiftY+dy) > 0.5 {
				return true
			}
		}
	}
	return false
}

func patternMask(pattern [CellHeight]string) (int, int, []float64) {
	minX, minY := maxPatternWidth(pattern), CellHeight
	maxX, maxY := -1, -1
	for y, row := range pattern {
		for x, ch := range row {
			if ch == '#' {
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < minX || maxY < minY {
		return 0, 0, nil
	}
	w, h := maxX-minX+1, maxY-minY+1
	mask := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if pattern[minY+y][minX+x] == '#' {
				mask[y*w+x] = 1
			}
		}
	}
	return w, h, mask
}

func maxPatternWidth(pattern [CellHeight]string) int {
	width := 0
	for _, row := range pattern {
		if len(row) > width {
			width = len(row)
		}
	}
	return width
}

// NormalizeMask scales an ink mask into a fixed-size centered canvas while
// preserving its aspect ratio.
func NormalizeMask(src []float64, sw, sh, tw, th int, padRatio float64) []float64 {
	dst := make([]float64, tw*th)
	if sw <= 0 || sh <= 0 {
		return dst
	}
	pad := int(math.Round(float64(minInt(tw, th)) * padRatio))
	availW := tw - 2*pad
	availH := th - 2*pad
	if availW < 1 {
		availW = tw
	}
	if availH < 1 {
		availH = th
	}
	scale := math.Min(float64(availW)/float64(sw), float64(availH)/float64(sh))
	dw := maxInt(1, int(math.Round(float64(sw)*scale)))
	dh := maxInt(1, int(math.Round(float64(sh)*scale)))
	offX := (tw - dw) / 2
	offY := (th - dh) / 2
	for dy := 0; dy < dh; dy++ {
		sy := minInt(sh-1, int(float64(dy)/float64(dh)*float64(sh)))
		for dx := 0; dx < dw; dx++ {
			sx := minInt(sw-1, int(float64(dx)/float64(dw)*float64(sw)))
			if src[sy*sw+sx] > 0.5 {
				dst[(offY+dy)*tw+offX+dx] = 1
			}
		}
	}
	return dst
}

func countMask(mask []float64) int {
	count := 0
	for _, v := range mask {
		if v > 0.5 {
			count++
		}
	}
	return count
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

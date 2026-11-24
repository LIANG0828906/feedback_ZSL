// Package font provides a small, fully deterministic 5x7 bitmap template
// library. Both the synthetic image generator and OCR matcher use it, making
// the generated samples self-contained and offline-safe.
package font

import (
	"fmt"
	"sort"
)

const (
	Width   = 5
	Height  = 7
	Version = "bitmap5x7-v1"
)

var glyphs = map[rune][Height]string{
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'B': {"####.", "#...#", "#...#", "####.", "#...#", "#...#", "####."},
	'C': {".###.", "#...#", "#....", "#....", "#....", "#...#", ".###."},
	'D': {"####.", "#...#", "#...#", "#...#", "#...#", "#...#", "####."},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'G': {".###.", "#...#", "#....", "#.###", "#...#", "#...#", ".###."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'I': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "#####"},
	'J': {"..###", "...#.", "...#.", "...#.", "...#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'L': {"#....", "#....", "#....", "#....", "#....", "#....", "#####"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#", "#...#"},
	'O': {".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'Q': {".###.", "#...#", "#...#", "#...#", "#.#.#", "#..#.", ".##.#"},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'S': {".####", "#....", "#....", ".###.", "....#", "....#", "####."},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#...#", "#.#.#", "#.#.#", ".#.#."},
	'X': {"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"},
	'Y': {"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."},
	'Z': {"#####", "....#", "...#.", "..#..", ".#...", "#....", "#####"},
	'0': {".###.", "#...#", "#..##", "#.#.#", "##..#", "#...#", ".###."},
	'1': {"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'2': {".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"},
	'3': {"#####", "...#.", "..#..", "...#.", "....#", "#...#", ".###."},
	'4': {"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
	'5': {"#####", "#....", "####.", "....#", "....#", "#...#", ".###."},
	'6': {"..##.", ".#...", "#....", "####.", "#...#", "#...#", ".###."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'8': {".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "...#.", ".##.."},
	'-': {".....", ".....", ".....", "#####", ".....", ".....", "....."},
	'.': {".....", ".....", ".....", ".....", ".....", ".##..", ".##.."},
}

func Pattern(r rune) ([Height]string, bool) {
	p, ok := glyphs[r]
	return p, ok
}

func Runes() []rune {
	out := make([]rune, 0, len(glyphs))
	for r := range glyphs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func Validate() error {
	for r, rows := range glyphs {
		for y, row := range rows {
			if len(row) != Width {
				return fmt.Errorf("glyph %q row %d has width %d, want %d", r, y, len(row), Width)
			}
			for _, c := range row {
				if c != '.' && c != '#' {
					return fmt.Errorf("glyph %q row %d has invalid pixel %q", r, y, c)
				}
			}
		}
	}
	return nil
}

// BinaryCells samples an unrotated, normalized glyph candidate into the 5x7
// template grid. Coordinates are normalized to [0,1).
func BinaryCells(normalized []bool, width, height int) ([Height][Width]bool, error) {
	var cells [Height][Width]bool
	if width <= 0 || height <= 0 || len(normalized) != width*height {
		return cells, fmt.Errorf("invalid normalized glyph buffer")
	}
	for ty := 0; ty < Height; ty++ {
		for tx := 0; tx < Width; tx++ {
			x0 := tx * width / Width
			x1 := maxInt(x0+1, (tx+1)*width/Width)
			y0 := ty * height / Height
			y1 := maxInt(y0+1, (ty+1)*height/Height)
			dark, total := 0, 0
			for y := y0; y < y1 && y < height; y++ {
				for x := x0; x < x1 && x < width; x++ {
					total++
					if normalized[y*width+x] {
						dark++
					}
				}
			}
			cells[ty][tx] = total > 0 && dark*2 >= total
		}
	}
	return cells, nil
}

func Score(candidate [Height][Width]bool, r rune) float64 {
	pattern, ok := glyphs[r]
	if !ok {
		return 0
	}
	matches, union := 0, 0
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			want := pattern[y][x] == '#'
			if candidate[y][x] || want {
				union++
			}
			if candidate[y][x] == want {
				matches++
			}
		}
	}
	if union == 0 {
		return 0
	}
	pixelAgreement := float64(matches) / float64(Width*Height)
	dice := 0.0
	darkWant, darkGot := 0, 0
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			want := pattern[y][x] == '#'
			if want {
				darkWant++
			}
			if candidate[y][x] {
				darkGot++
			}
		}
	}
	if darkWant+darkGot > 0 {
		both := 0
		for y := 0; y < Height; y++ {
			for x := 0; x < Width; x++ {
				if candidate[y][x] && pattern[y][x] == '#' {
					both++
				}
			}
		}
		dice = 2 * float64(both) / float64(darkWant+darkGot)
	}
	// Reward overlap and pixel agreement equally; both are deterministic.
	return 0.65*dice + 0.35*pixelAgreement
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

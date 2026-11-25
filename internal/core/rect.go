package core

import (
	"fmt"
)

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

func (r Rect) String() string {
	return fmt.Sprintf("%dx%d+%d+%d", r.W, r.H, r.X, r.Y)
}

func (r Rect) Right() int  { return r.X + r.W }
func (r Rect) Bottom() int { return r.Y + r.H }

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.Right() && y >= r.Y && y < r.Bottom()
}

func (r Rect) InFrame(w, h int) bool {
	return !r.Empty() && r.X >= 0 && r.Y >= 0 && r.Right() <= w && r.Bottom() <= h
}

func (r Rect) Intersects(other Rect) bool {
	return r.X < other.Right() && other.X < r.Right() &&
		r.Y < other.Bottom() && other.Y < r.Bottom()
}

func Intersection(a, b Rect) (Rect, bool) {
	x0 := max(a.X, b.X)
	y0 := max(a.Y, b.Y)
	x1 := min(a.Right(), b.Right())
	y1 := min(a.Bottom(), b.Bottom())
	if x1 <= x0 || y1 <= y0 {
		return Rect{}, false
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}, true
}

func (r Rect) Expand(n int) Rect {
	if n <= 0 {
		return r
	}
	return Rect{X: r.X - n, Y: r.Y - n, W: r.W + 2*n, H: r.H + 2*n}
}

func (r Rect) ExpandClamped(n, w, h int) Rect {
	e := r.Expand(n)
	x0 := max(0, e.X)
	y0 := max(0, e.Y)
	x1 := min(w, e.Right())
	y1 := min(h, e.Bottom())
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func (r Rect) Clamp(w, h int) Rect {
	x0 := max(0, r.X)
	y0 := max(0, r.Y)
	x1 := min(w, r.Right())
	y1 := min(h, r.Bottom())
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func CommonDivisors(a, b int) []int {
	if a <= 0 || b <= 0 {
		return nil
	}
	g := gcd(a, b)
	var out []int
	for i := 1; i*i <= g; i++ {
		if g%i == 0 {
			out = append(out, i)
			if i*i != g {
				out = append(out, g/i)
			}
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

package ocrflow

import (
	"encoding/json"
	"math"
)

// Point uses integer image coordinates.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Quad is ordered top-left, top-right, bottom-right, bottom-left.
type Quad [4]Point

func (q Quad) MarshalJSON() ([]byte, error) {
	return json.Marshal([4][2]int{
		{q[0].X, q[0].Y},
		{q[1].X, q[1].Y},
		{q[2].X, q[2].Y},
		{q[3].X, q[3].Y},
	})
}

func (q *Quad) UnmarshalJSON(data []byte) error {
	var raw [4][2]int
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for i, p := range raw {
		q[i] = Point{X: p[0], Y: p[1]}
	}
	return nil
}

// Rect is an inclusive-ish integer bounding rectangle [MinX,MaxX) style in
// width/height helpers, while Max coordinates are stored as exclusive bounds.
type Rect struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func (r Rect) Width() int  { return r.MaxX - r.MinX }
func (r Rect) Height() int { return r.MaxY - r.MinY }

func (r Rect) Empty() bool { return r.MaxX <= r.MinX || r.MaxY <= r.MinY }

func (r Rect) Area() int {
	if r.Empty() {
		return 0
	}
	return r.Width() * r.Height()
}

func (q Quad) Rect() Rect {
	minX, minY := q[0].X, q[0].Y
	maxX, maxY := q[0].X, q[0].Y
	for _, p := range q[1:] {
		if p.X < minX {
			minX = p.X
		}
		if p.X > maxX {
			maxX = p.X
		}
		if p.Y < minY {
			minY = p.Y
		}
		if p.Y > maxY {
			maxY = p.Y
		}
	}
	return Rect{MinX: minX, MinY: minY, MaxX: maxX + 1, MaxY: maxY + 1}
}

func (q Quad) Area2() int {
	a := 0
	for i := 0; i < 4; i++ {
		j := (i + 1) % 4
		a += q[i].X*q[j].Y - q[j].X*q[i].Y
	}
	if a < 0 {
		return -a
	}
	return a
}

// ValidQuad reports that all integer points are finite by type and that the
// quad is not point/line degenerate.
func ValidQuad(q Quad) bool {
	r := q.Rect()
	return q.Area2() > 0 && r.Width() > 1 && r.Height() > 1
}

func floatFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func rectIntersection(a, b Rect) Rect {
	r := Rect{
		MinX: maxInt(a.MinX, b.MinX),
		MinY: maxInt(a.MinY, b.MinY),
		MaxX: minInt(a.MaxX, b.MaxX),
		MaxY: minInt(a.MaxY, b.MaxY),
	}
	if r.MaxX < r.MinX {
		r.MaxX = r.MinX
	}
	if r.MaxY < r.MinY {
		r.MaxY = r.MinY
	}
	return r
}

// OverlapRatio returns intersection area divided by the smaller box area.
func OverlapRatio(a, b Rect) int {
	inter := rectIntersection(a, b).Area()
	if inter == 0 {
		return 0
	}
	den := minInt(a.Area(), b.Area())
	if den <= 0 {
		return 0
	}
	return inter * 10000 / den
}

// VerticalOverlapBPS measures shared vertical span over the smaller height.
func VerticalOverlapBPS(a, b Rect) int {
	lo := maxInt(a.MinY, b.MinY)
	hi := minInt(a.MaxY, b.MaxY)
	shared := hi - lo
	if shared <= 0 {
		return 0
	}
	den := minInt(a.Height(), b.Height())
	if den <= 0 {
		return 0
	}
	return shared * 10000 / den
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

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

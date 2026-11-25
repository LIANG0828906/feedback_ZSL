package vectorcore

import "fmt"

type Diff struct {
	TimeIndex int    `json:"timeIndex"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	PathID    string `json:"pathId,omitempty"`
	Stage     Stage  `json:"stage,omitempty"`
	Reason    string `json:"reason"`
}

func diffError(d *Diff) error {
	if d == nil {
		return nil
	}
	return &Error{Code: CodeDiff, Op: "compare", TimeIndex: d.TimeIndex, X: d.X, Y: d.Y, PathID: d.PathID,
		Stage: d.Stage, Keyframe: -1, Reason: d.Reason}
}

func sameBitmap(a, b *Bitmap) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Width != b.Width || a.Height != b.Height || len(a.Pixels) != len(b.Pixels) {
		return false
	}
	for i := range a.Pixels {
		if a.Pixels[i] != b.Pixels[i] {
			return false
		}
	}
	return true
}

func firstBitmapDiff(a, b *Bitmap) (x, y int, ok bool) {
	if a == nil || b == nil {
		return 0, 0, a != b
	}
	w, h := a.Width, a.Height
	if b.Width < w {
		w = b.Width
	}
	if b.Height < h {
		h = b.Height
	}
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			i := (py*a.Width + px) * 4
			j := (py*b.Width + px) * 4
			for ch := 0; ch < 4; ch++ {
				if a.Pixels[i+ch] != b.Pixels[j+ch] {
					return px, py, true
				}
			}
		}
	}
	if a.Width != b.Width || a.Height != b.Height {
		return w, h, true
	}
	return 0, 0, false
}

func firstCoverageDiff(a, b Coverage) (x, y int, ok bool) {
	if a.Width != b.Width || a.Height != b.Height || len(a.Values) != len(b.Values) {
		return 0, 0, true
	}
	for i, v := range a.Values {
		if v != b.Values[i] {
			return i % a.Width, i / a.Width, true
		}
	}
	return 0, 0, false
}

// CompareTimelines checks the commit sequence, per-path coverage and
// composition layers, and final pixels. It returns the first concrete
// inconsistency with its time index, pixel coordinate and path id.
func CompareTimelines(incremental, full *TimelineResult) (*Diff, error) {
	if incremental == nil || full == nil {
		return nil, &Error{Code: CodeDiff, Op: "compare", Keyframe: -1, TimeIndex: -1, Reason: "nil timeline"}
	}
	if len(incremental.Times) != len(full.Times) {
		return &Diff{TimeIndex: minInt(len(incremental.Times), len(full.Times)), Reason: "different committed time count"},
			&Error{Code: CodeDiff, Op: "compare", Keyframe: -1, TimeIndex: minInt(len(incremental.Times), len(full.Times)), Reason: "different committed time count"}
	}
	for i := range incremental.Times {
		a, b := incremental.Times[i], full.Times[i]
		if a.TimeIndex != b.TimeIndex || a.TimeTicks != b.TimeTicks {
			d := &Diff{TimeIndex: i, Reason: "sample time differs"}
			return d, diffError(d)
		}
		if a.CommitSeq != b.CommitSeq {
			d := &Diff{TimeIndex: a.TimeIndex, Reason: fmt.Sprintf("commit sequence %d != %d", a.CommitSeq, b.CommitSeq)}
			return d, diffError(d)
		}
		if a.Viewport.Width != b.Viewport.Width || a.Viewport.Height != b.Viewport.Height || a.Viewport.Bounds != b.Viewport.Bounds {
			d := &Diff{TimeIndex: a.TimeIndex, Reason: "viewport differs"}
			return d, diffError(d)
		}
		if len(a.Paths) != len(b.Paths) {
			d := &Diff{TimeIndex: a.TimeIndex, Reason: "path count differs"}
			return d, diffError(d)
		}
		for pi := range a.Paths {
			ap, bp := a.Paths[pi], b.Paths[pi]
			if ap.PathID != bp.PathID {
				d := &Diff{TimeIndex: a.TimeIndex, PathID: ap.PathID, Reason: "path ordering differs"}
				return d, diffError(d)
			}
			if x, y, ok := firstCoverageDiff(ap.Coverage, bp.Coverage); ok {
				d := &Diff{TimeIndex: a.TimeIndex, X: x, Y: y, PathID: ap.PathID, Stage: StageCoverage, Reason: "coverage differs"}
				return d, diffError(d)
			}
			if x, y, ok := firstBitmapDiff(ap.CompositeAfter, bp.CompositeAfter); ok {
				d := &Diff{TimeIndex: a.TimeIndex, X: x, Y: y, PathID: ap.PathID, Stage: StageComposite, Reason: "composite layer differs"}
				return d, diffError(d)
			}
		}
		if x, y, ok := firstBitmapDiff(a.Bitmap, b.Bitmap); ok {
			pathID := ""
			if len(a.Paths) > 0 {
				pathID = a.Paths[len(a.Paths)-1].PathID
			}
			d := &Diff{TimeIndex: a.TimeIndex, X: x, Y: y, PathID: pathID, Stage: StageComposite, Reason: "final bitmap differs"}
			return d, diffError(d)
		}
	}
	return nil, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

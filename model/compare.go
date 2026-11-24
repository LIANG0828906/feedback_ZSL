package model

import (
	"fmt"
	"reflect"
)

type RunFrameSet struct {
	Frames  []Frame
	Commits []int
}

type FirstMismatch struct {
	WindowIndex int    `json:"window_index"`
	Frame       int    `json:"frame"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Field       string `json:"field"`
	Expected    string `json:"expected"`
	Actual      string `json:"actual"`
}

func (m *FirstMismatch) Error() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%s: first mismatch window=%d frame=%d pixel=(%d,%d) expected=%s actual=%s",
		CodeCompare, m.WindowIndex, m.Frame, m.X, m.Y, m.Expected, m.Actual)
}

// CompareRunSets compares frames, sample stats and window commit order. The
// first differing pixel is reported with its window and coordinate.
func CompareRunSets(expected, actual RunFrameSet, windowSize int) *FirstMismatch {
	if windowSize <= 0 {
		windowSize = 1
	}
	if !reflect.DeepEqual(expected.Commits, actual.Commits) {
		return &FirstMismatch{WindowIndex: -1, Frame: -1, X: -1, Y: -1, Field: "commits", Expected: fmt.Sprint(expected.Commits), Actual: fmt.Sprint(actual.Commits)}
	}
	if len(expected.Frames) != len(actual.Frames) {
		return &FirstMismatch{WindowIndex: -1, Frame: -1, X: -1, Y: -1, Field: "frame_count", Expected: fmt.Sprint(len(expected.Frames)), Actual: fmt.Sprint(len(actual.Frames))}
	}
	for i := range expected.Frames {
		e, a := expected.Frames[i], actual.Frames[i]
		window := i / windowSize
		if e.Index != a.Index || e.Width != a.Width || e.Height != a.Height || e.Channels != a.Channels || e.TimeNS != a.TimeNS {
			return &FirstMismatch{WindowIndex: window, Frame: i, X: -1, Y: -1, Field: "frame_metadata", Expected: fmt.Sprintf("%d/%dx%dx%d@%d", e.Index, e.Width, e.Height, e.Channels, e.TimeNS), Actual: fmt.Sprintf("%d/%dx%dx%d@%d", a.Index, a.Width, a.Height, a.Channels, a.TimeNS)}
		}
		if e.Hash != a.Hash {
			n := len(e.RGBA)
			if len(a.RGBA) < n {
				n = len(a.RGBA)
			}
			for off := 0; off+3 < n; off += 4 {
				if e.RGBA[off] != a.RGBA[off] || e.RGBA[off+1] != a.RGBA[off+1] || e.RGBA[off+2] != a.RGBA[off+2] || e.RGBA[off+3] != a.RGBA[off+3] {
					px := off / 4
					return &FirstMismatch{
						WindowIndex: window,
						Frame:       i,
						X:           px % e.Width,
						Y:           px / e.Width,
						Field:       "rgba",
						Expected:    fmt.Sprintf("%v", e.RGBA[off:off+4]),
						Actual:      fmt.Sprintf("%v", a.RGBA[off:off+4]),
					}
				}
			}
			return &FirstMismatch{WindowIndex: window, Frame: i, Field: "hash", Expected: e.Hash, Actual: a.Hash}
		}
		if !reflect.DeepEqual(e.Stats, a.Stats) {
			return &FirstMismatch{WindowIndex: window, Frame: i, X: -1, Y: -1, Field: "stats", Expected: fmt.Sprintf("%+v", e.Stats), Actual: fmt.Sprintf("%+v", a.Stats)}
		}
	}
	return nil
}

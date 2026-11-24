package compare

import (
	"math"
	"sort"

	"terrainnormalverify/internal/bake"
	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/seam"
)

type Difference struct {
	BlockID int    `json:"blockId"`
	Field   string `json:"field"`
	Pixel   int    `json:"pixel"`
	Left    any    `json:"left"`
	Right   any    `json:"right"`
}

func Snapshots(left, right engine.Snapshot) (*Difference, error) {
	leftIDs := blockIDs(left.Blocks)
	rightIDs := blockIDs(right.Blocks)
	if len(leftIDs) != len(rightIDs) {
		return diff(-1, "blockCount", -1, len(leftIDs), len(rightIDs)), nil
	}
	for i := range leftIDs {
		if leftIDs[i] != rightIDs[i] {
			return diff(leftIDs[i], "blockId", -1, leftIDs[i], rightIDs[i]), nil
		}
	}
	for _, id := range leftIDs {
		a := left.Blocks[id]
		b := right.Blocks[id]
		if a.Width != b.Width || a.Height != b.Height {
			return diff(id, "size", -1, [2]int{a.Width, a.Height}, [2]int{b.Width, b.Height}), nil
		}
		for i := 0; i < a.PixelCount(); i++ {
			if !a.Normals[i].Equal(b.Normals[i]) {
				return diff(id, "normal", i, a.Normals[i], b.Normals[i]), nil
			}
			if math.Float64bits(a.Slopes[i]) != math.Float64bits(b.Slopes[i]) {
				return diff(id, "slope", i, a.Slopes[i], b.Slopes[i]), nil
			}
		}
		if !a.BBox.Min.Equal(b.BBox.Min) {
			return diff(id, "bbox.min", -1, a.BBox.Min, b.BBox.Min), nil
		}
		if !a.BBox.Max.Equal(b.BBox.Max) {
			return diff(id, "bbox.max", -1, a.BBox.Max, b.BBox.Max), nil
		}
	}
	if d := compareSeams(left.Seams, right.Seams); d != nil {
		return d, nil
	}
	return nil, nil
}

func compareSeams(left, right []seam.SeamReport) *Difference {
	if len(left) != len(right) {
		return diff(-1, "seamCount", -1, len(left), len(right))
	}
	a := append([]seam.SeamReport(nil), left...)
	b := append([]seam.SeamReport(nil), right...)
	sort.Slice(a, func(i, j int) bool { return a[i].SeamID < a[j].SeamID })
	sort.Slice(b, func(i, j int) bool { return b[i].SeamID < b[j].SeamID })
	for i := range a {
		if a[i].SeamID != b[i].SeamID {
			return diff(a[i].BlockA, "seam.id", i, a[i].SeamID, b[i].SeamID)
		}
		if a[i].MaxAngleDeg != b[i].MaxAngleDeg {
			return diff(a[i].BlockA, "seam.maxAngleDeg", i, a[i].MaxAngleDeg, b[i].MaxAngleDeg)
		}
		if a[i].FinalMaxAngleDeg != b[i].FinalMaxAngleDeg {
			return diff(a[i].BlockA, "seam.finalMaxAngleDeg", i, a[i].FinalMaxAngleDeg, b[i].FinalMaxAngleDeg)
		}
		if len(a[i].Violations) != len(b[i].Violations) {
			return diff(a[i].BlockA, "seam.violationCount", i, len(a[i].Violations), len(b[i].Violations))
		}
		for j := range a[i].Violations {
			va := a[i].Violations[j]
			vb := b[i].Violations[j]
			if va.AX != vb.AX || va.AY != vb.AY || va.BX != vb.BX || va.BY != vb.BY {
				return diff(a[i].BlockA, "seam.violationPixel", j,
					[2]int{va.AX, va.AY}, [2]int{vb.AX, vb.AY})
			}
			if va.AngleDeg != vb.AngleDeg {
				return diff(a[i].BlockA, "seam.violationAngle", j, va.AngleDeg, vb.AngleDeg)
			}
			if va.Attribution != vb.Attribution {
				return diff(a[i].BlockA, "seam.attribution", j, va.Attribution, vb.Attribution)
			}
		}
	}
	return nil
}

func diff(blockID int, field string, pixel int, left, right any) *Difference {
	return &Difference{BlockID: blockID, Field: field, Pixel: pixel, Left: left, Right: right}
}

func AsError(d *Difference) *model.Error {
	if d == nil {
		return nil
	}
	e := model.NewError(model.ErrDiff, d.BlockID, d.Field, "snapshot mismatch")
	e.Value = map[string]any{
		"pixel": d.Pixel,
		"left":  d.Left,
		"right": d.Right,
	}
	e.Want = d.Left
	return e
}

func blockIDs(blocks map[int]*bake.BlockResult) []int {
	ids := make([]int, 0, len(blocks))
	for id := range blocks {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

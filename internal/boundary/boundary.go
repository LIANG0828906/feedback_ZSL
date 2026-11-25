package boundary

import (
	"math"
	"sort"

	"terrainpatch/internal/model"
)

type PointZ struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type AlignedPoint struct {
	Order  int   `json:"order"`
	X      int64 `json:"x"`
	Y      int64 `json:"y"`
	ZA     int64 `json:"zA"`
	ZB     int64 `json:"zB"`
	DeltaZ int64 `json:"deltaZ"`
}

type Stat struct {
	BoundaryID              string         `json:"boundaryId"`
	TileA                   string         `json:"tileA"`
	TileB                   string         `json:"tileB"`
	Axis                    string         `json:"axis"`
	EdgeA                   string         `json:"edgeA"`
	EdgeB                   string         `json:"edgeB"`
	ThresholdMicroDeg       int64          `json:"angleThresholdMicroDeg"`
	MaxAngleMicroDeg        int64          `json:"maxAngleMicroDeg"`
	Exceeded                bool           `json:"exceeded"`
	Position                *PointZ        `json:"position,omitempty"`
	Category                string         `json:"category,omitempty"`
	Code                    string         `json:"code,omitempty"`
	Message                 string         `json:"message,omitempty"`
	AlignedCount            int            `json:"alignedCount"`
	Aligned                 []AlignedPoint `json:"aligned,omitempty"`
	LengthA                 int            `json:"lengthA"`
	LengthB                 int            `json:"lengthB"`
	SequenceA               string         `json:"sequenceA"`
	SequenceB               string         `json:"sequenceB"`
	AlignmentMismatchCount  int            `json:"alignmentMismatchCount"`
	FirstMismatchIndex      int            `json:"firstMismatchIndex"`
	FirstMismatchA          *PointZ        `json:"firstMismatchA,omitempty"`
	FirstMismatchB          *PointZ        `json:"firstMismatchB,omitempty"`
	HeightErrorA            int64          `json:"heightErrorA"`
	HeightErrorB            int64          `json:"heightErrorB"`
	ToleranceA              int64          `json:"toleranceA"`
	ToleranceB              int64          `json:"toleranceB"`
	HeightCriterionExceeded bool           `json:"heightCriterionExceeded"`
}

func ExtractSeries(m *model.Mesh, edge string) []PointZ {
	if m == nil {
		return nil
	}
	var constant int64
	useX := edge == "left" || edge == "right"
	if useX {
		constant = m.Vertices[0].X
		for _, v := range m.Vertices {
			if edge == "left" && v.X < constant {
				constant = v.X
			}
			if edge == "right" && v.X > constant {
				constant = v.X
			}
		}
	} else {
		constant = m.Vertices[0].Y
		for _, v := range m.Vertices {
			if edge == "top" && v.Y < constant {
				constant = v.Y
			}
			if edge == "bottom" && v.Y > constant {
				constant = v.Y
			}
		}
	}

	out := []PointZ{}
	for _, v := range m.Vertices {
		if useX && v.X == constant {
			out = append(out, PointZ{X: v.X, Y: v.Y, Z: v.Z})
		}
		if !useX && v.Y == constant {
			out = append(out, PointZ{X: v.X, Y: v.Y, Z: v.Z})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if useX {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}

func axisT(p PointZ, axis string) int64 {
	if axis == "horizontal" {
		return p.X
	}
	return p.Y
}

func withT(axis string, constant, t, z int64) PointZ {
	if axis == "horizontal" {
		return PointZ{X: t, Y: constant, Z: z}
	}
	return PointZ{X: constant, Y: t, Z: z}
}

func SampleZ(series []PointZ, axis string, t int64) (int64, bool) {
	if len(series) == 0 {
		return 0, false
	}
	ts := make([]int64, len(series))
	for i, p := range series {
		ts[i] = axisT(p, axis)
	}
	idx := sort.Search(len(ts), func(i int) bool { return ts[i] >= t })
	if idx < len(ts) && ts[idx] == t {
		return series[idx].Z, true
	}
	if idx == 0 {
		return series[0].Z, true
	}
	if idx >= len(ts) {
		return series[len(series)-1].Z, true
	}
	p0, p1 := series[idx-1], series[idx]
	t0, t1 := ts[idx-1], ts[idx]
	den := t1 - t0
	if den == 0 {
		return p1.Z, true
	}
	num := p0.Z*(t1-t) + p1.Z*(t-t0)
	z := num / den
	if num%den != 0 && ((num > 0) == (den > 0)) {
		z++
	}
	return z, true
}

func BoundaryHeightError(tile model.Tile, m *model.Mesh, edge string) int64 {
	series := ExtractSeries(m, edge)
	if len(series) < 2 {
		return 0
	}
	gx0, gx1 := 0, tile.Grid.Width-1
	gy0, gy1 := 0, tile.Grid.Height-1
	switch edge {
	case "left":
		gx1 = 0
	case "right":
		gx0 = gx1
	case "top":
		gy1 = 0
	case "bottom":
		gy0 = gy1
	}
	var maxErr int64
	for gy := gy0; gy <= gy1; gy++ {
		for gx := gx0; gx <= gx1; gx++ {
			x := tile.OriginX + int64(gx)*tile.Grid.CellSize
			y := tile.OriginY + int64(gy)*tile.Grid.CellSize
			t, h := y, tile.Grid.Heights[gy][gx]
			axis := "vertical"
			if edge == "top" || edge == "bottom" {
				axis = "horizontal"
				t = x
			}
			z, _ := SampleZ(series, axis, t)
			err := h - z
			if err < 0 {
				err = -err
			}
			if err > maxErr {
				maxErr = err
			}
		}
	}
	return maxErr
}

func CompareOrder(a, b []PointZ) (mismatch int, first int, pa, pb *PointZ, sigA, sigB string) {
	first = -1
	ca := make([][2]int64, len(a))
	cb := make([][2]int64, len(b))
	for i, p := range a {
		ca[i] = [2]int64{p.X, p.Y}
	}
	for i, p := range b {
		cb[i] = [2]int64{p.X, p.Y}
	}
	sigA = model.DigestJSON(ca)[:16]
	sigB = model.DigestJSON(cb)[:16]
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].X != b[i].X || a[i].Y != b[i].Y {
			mismatch++
			if first == -1 {
				first = i
				ap, bp := a[i], b[i]
				pa, pb = &ap, &bp
			}
		}
	}
	if len(a) != len(b) {
		mismatch += abs(len(a) - len(b))
		if first == -1 && n > 0 {
			first = n
			if n < len(a) {
				ap := a[n]
				pa = &ap
			}
			if n < len(b) {
				bp := b[n]
				pb = &bp
			}
		}
	}
	return
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func unionTAxis(axis string, a, b []PointZ) []int64 {
	set := map[int64]struct{}{}
	for _, p := range a {
		set[axisT(p, axis)] = struct{}{}
	}
	for _, p := range b {
		set[axisT(p, axis)] = struct{}{}
	}
	out := make([]int64, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func deflectionMicroDeg(dt1, dz1, dt2, dz2 float64) int64 {
	dot := dt1*dt2 + dz1*dz2
	n1 := math.Hypot(dt1, dz1)
	n2 := math.Hypot(dt2, dz2)
	if n1 == 0 || n2 == 0 {
		return 0
	}
	cos := dot / (n1 * n2)
	if cos > 1 {
		cos = 1
	}
	if cos < -1 {
		cos = -1
	}
	rad := math.Acos(cos)
	return int64(math.Round(rad * 180 / math.Pi * 1_000_000))
}

func maxFoldUnion(axis string, constant int64, a, b []PointZ) (int64, *PointZ) {
	ts := unionTAxis(axis, a, b)
	if len(ts) < 3 {
		return 0, nil
	}
	var maxAngle int64
	var pos PointZ
	for i := 1; i < len(ts)-1; i++ {
		t0, t1, t2 := ts[i-1], ts[i], ts[i+1]
		za0, _ := SampleZ(a, axis, t0)
		za1, _ := SampleZ(a, axis, t1)
		za2, _ := SampleZ(a, axis, t2)
		zb0, _ := SampleZ(b, axis, t0)
		zb1, _ := SampleZ(b, axis, t1)
		zb2, _ := SampleZ(b, axis, t2)

		angle1 := deflectionMicroDeg(
			float64(t1-t0), float64(za1-za0),
			float64(t2-t1), float64(zb2-zb1),
		)
		angle2 := deflectionMicroDeg(
			float64(t1-t0), float64(zb1-zb0),
			float64(t2-t1), float64(za2-za1),
		)
		angle := angle1
		if angle2 > angle {
			angle = angle2
		}
		if angle > maxAngle {
			maxAngle = angle
			pos = withT(axis, constant, t1, za1)
		}
	}
	return maxAngle, &pos
}

func maxFoldOrdered(axis string, a, b []PointZ) (int64, *PointZ) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 3 {
		return 0, nil
	}
	var maxAngle int64
	var pos PointZ
	for i := 1; i < n-1; i++ {
		ai0, ai1 := a[i-1], a[i]
		bi1, bi2 := b[i], b[i+1]
		angle1 := deflectionMicroDeg(
			float64(axisT(ai1, axis)-axisT(ai0, axis)), float64(ai1.Z-ai0.Z),
			float64(axisT(bi2, axis)-axisT(bi1, axis)), float64(bi2.Z-bi1.Z),
		)
		bi0 := b[i-1]
		ai2 := a[i+1]
		angle2 := deflectionMicroDeg(
			float64(axisT(bi1, axis)-axisT(bi0, axis)), float64(bi1.Z-bi0.Z),
			float64(axisT(ai2, axis)-axisT(ai1, axis)), float64(ai2.Z-ai1.Z),
		)
		angle := angle1
		if angle2 > angle {
			angle = angle2
		}
		if angle > maxAngle {
			maxAngle = angle
			p := a[i]
			pos = p
		}
	}
	return maxAngle, &pos
}

func Diagnose(b model.Boundary, tileA, tileB model.Tile, meshA, meshB *model.Mesh) (*Stat, *model.AppError) {
	a := ExtractSeries(meshA, b.SideA.Edge)
	bb := ExtractSeries(meshB, b.SideB.Edge)
	mismatch, first, pa, pb, sigA, sigB := CompareOrder(a, bb)
	errA := BoundaryHeightError(tileA, meshA, b.SideA.Edge)
	errB := BoundaryHeightError(tileB, meshB, b.SideB.Edge)
	threshold := b.AngleThresholdMicroDeg
	if threshold == 0 {
		threshold = 100_000
	}
	maxAngle, pos := maxFoldUnion(b.Axis, b.Value, a, bb)
	aligned := forceAligned(b, a, bb)
	st := &Stat{
		BoundaryID: b.ID, TileA: b.SideA.TileID, TileB: b.SideB.TileID,
		Axis: b.Axis, EdgeA: b.SideA.Edge, EdgeB: b.SideB.Edge,
		ThresholdMicroDeg: threshold, MaxAngleMicroDeg: maxAngle, Position: pos,
		AlignedCount: len(b.SideA.Columns), Aligned: aligned,
		LengthA: len(a), LengthB: len(bb), SequenceA: sigA, SequenceB: sigB,
		AlignmentMismatchCount: mismatch, FirstMismatchIndex: first,
		FirstMismatchA: pa, FirstMismatchB: pb,
		HeightErrorA: errA, HeightErrorB: errB,
		ToleranceA: tileA.Tolerance, ToleranceB: tileB.Tolerance,
	}
	st.HeightCriterionExceeded = errA > tileA.Tolerance || errB > tileB.Tolerance
	classify(st)
	if st.Code == model.CodeAttr {
		return st, model.NewError(model.CodeAttr, st.Message).
			WithBoundary(b.ID, "angleThresholdMicroDeg").
			WithActual(map[string]any{
				"maxAngleMicroDeg": st.MaxAngleMicroDeg,
				"heightErrorA":     errA, "toleranceA": tileA.Tolerance,
				"heightErrorB": errB, "toleranceB": tileB.Tolerance,
				"alignmentMismatchCount": mismatch,
			})
	}
	return st, nil
}

func forceAligned(b model.Boundary, a, bb []PointZ) []AlignedPoint {
	out := make([]AlignedPoint, 0, len(b.SideA.Columns))
	for _, c := range b.SideA.Columns {
		t := c.Y
		if b.Axis == "horizontal" {
			t = c.X
		}
		za, _ := SampleZ(a, b.Axis, t)
		zb, _ := SampleZ(bb, b.Axis, t)
		out = append(out, AlignedPoint{Order: c.Order, X: c.X, Y: c.Y, ZA: za, ZB: zb, DeltaZ: za - zb})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func DiagnoseProbe(p model.AttributionProbe) (*Stat, *model.AppError) {
	a := make([]PointZ, len(p.SideA))
	bb := make([]PointZ, len(p.SideB))
	for i, q := range p.SideA {
		a[i] = PointZ{X: q.X, Y: q.Y, Z: q.Z}
	}
	for i, q := range p.SideB {
		bb[i] = PointZ{X: q.X, Y: q.Y, Z: q.Z}
	}
	mismatch, first, pa, pb, sigA, sigB := CompareOrder(a, bb)
	errA := ReferenceError(a, p.Axis, p.ReferenceA)
	errB := ReferenceError(bb, p.Axis, p.ReferenceB)
	maxAngle, pos := maxFoldOrdered(p.Axis, a, bb)
	st := &Stat{
		BoundaryID: p.BoundaryID, TileA: "probe-a", TileB: "probe-b",
		Axis: p.Axis, ThresholdMicroDeg: p.Threshold,
		MaxAngleMicroDeg: maxAngle, Position: pos,
		LengthA: len(a), LengthB: len(bb), SequenceA: sigA, SequenceB: sigB,
		AlignmentMismatchCount: mismatch, FirstMismatchIndex: first,
		FirstMismatchA: pa, FirstMismatchB: pb,
		HeightErrorA: errA, HeightErrorB: errB,
		ToleranceA: p.ToleranceA, ToleranceB: p.ToleranceB,
	}
	st.HeightCriterionExceeded = errA > p.ToleranceA || errB > p.ToleranceB
	classify(st)
	if st.Code == model.CodeAttr {
		return st, model.NewError(model.CodeAttr, st.Message).
			WithBoundary(p.BoundaryID, "attribution").
			WithActual(map[string]any{
				"maxAngleMicroDeg": st.MaxAngleMicroDeg,
				"heightErrorA":     errA, "toleranceA": p.ToleranceA,
				"heightErrorB": errB, "toleranceB": p.ToleranceB,
				"alignmentMismatchCount": mismatch,
			})
	}
	return st, nil
}

func ReferenceError(series []PointZ, axis string, refs []model.ProbeReference) int64 {
	var maxErr int64
	for _, r := range refs {
		t := r.Y
		if axis == "horizontal" {
			t = r.X
		}
		z, ok := SampleZ(series, axis, t)
		if !ok {
			continue
		}
		err := r.Z - z
		if err < 0 {
			err = -err
		}
		if err > maxErr {
			maxErr = err
		}
	}
	return maxErr
}

func classify(st *Stat) {
	if st.MaxAngleMicroDeg <= st.ThresholdMicroDeg {
		st.Exceeded = false
		st.Category = ""
		return
	}
	st.Exceeded = true
	switch {
	case st.HeightCriterionExceeded:
		st.Category = "undersampling"
		st.Code = ""
		st.Message = "fold angle exceeds threshold and boundary interpolation error exceeds tolerance"
	case st.AlignmentMismatchCount > 0 || st.SequenceA != st.SequenceB:
		st.Category = "alignment_mismatch"
		st.Code = ""
		st.Message = "fold angle exceeds threshold and ordered boundary coordinates do not align"
	default:
		st.Category = "unclassified"
		st.Code = model.CodeAttr
		st.Message = "fold angle exceeds threshold but neither undersampling nor alignment criteria are exceeded"
	}
}

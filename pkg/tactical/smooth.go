package tactical

import (
	"fmt"
	"sort"
)

type SmoothingOptions struct {
	MaxSpacingMilli  int64
	FormationSamples int
}

// SmoothPath applies deterministic line-of-sight simplification using the
// slot tolerance as a fixed-point geometric epsilon. The source minimum cost
// is retained verbatim; smoothing never reprices the route.
func SmoothPath(def MapDef, member Member, slot Slot, path MemberPath, opts SmoothingOptions) (*SmoothedPath, error) {
	if len(path.Cells) == 0 {
		return nil, memberError(CodeForm, member.ID, "cannot smooth an empty path")
	}
	epsilon, ok := MilliFloat(slot.Tolerance)
	if !ok || epsilon < 0 {
		return nil, memberError(CodeForm, member.ID, "slot tolerance is not a finite non-negative fixed-point number")
	}
	smoothed := simplifyLineOfSight(def, path.Cells, epsilon)
	corners := make([]Corner, 0, len(smoothed))
	for _, point := range smoothed {
		fixed := GridToFixed(point)
		corners = append(corners, Corner{
			Grid:  point,
			Fixed: fixed,
			World: fixed.World(),
		})
	}
	return &SmoothedPath{
		MemberID:       member.ID,
		SourceCost:     path.Cost,
		ToleranceMilli: epsilon,
		Corners:        corners,
	}, nil
}

func simplifyLineOfSight(def MapDef, cells []GridPoint, epsilon int64) []GridPoint {
	if len(cells) <= 2 {
		return append([]GridPoint(nil), cells...)
	}
	out := make([]GridPoint, 0, len(cells))
	out = append(out, cells[0])
	anchor := 0
	for anchor < len(cells)-1 {
		best := anchor + 1
		for candidate := anchor + 2; candidate < len(cells); candidate++ {
			if !lineOfSight(def, cells[anchor], cells[candidate]) {
				continue
			}
			if !allWithinTolerance(cells[anchor], cells[candidate], cells[anchor+1:candidate], epsilon) {
				continue
			}
			best = candidate
		}
		out = append(out, cells[best])
		anchor = best
	}
	return out
}

func allWithinTolerance(a, b GridPoint, points []GridPoint, epsilon int64) bool {
	fa := GridToFixed(a)
	fb := GridToFixed(b)
	for _, point := range points {
		if !DistanceSquaredBelow(fa, fb, GridToFixed(point), epsilon) {
			return false
		}
	}
	return true
}

func lineOfSight(def MapDef, a, b GridPoint) bool {
	cells := lineCells(a, b)
	for i, point := range cells {
		cell, ok := CellAt(def, point)
		if !ok || !cell.Passable {
			return false
		}
		if i == 0 {
			continue
		}
		previous := cells[i-1]
		dx := point.X - previous.X
		dy := point.Y - previous.Y
		if dx != 0 && dy != 0 {
			sideA, okA := CellAt(def, GridPoint{X: previous.X + dx, Y: previous.Y})
			sideB, okB := CellAt(def, GridPoint{X: previous.X, Y: previous.Y + dy})
			if !okA || !okB || !sideA.Passable || !sideB.Passable {
				return false
			}
		}
	}
	return true
}

// lineCells returns the integer grid cells touched by the center-to-center
// segment in deterministic order.
func lineCells(a, b GridPoint) []GridPoint {
	x0, y0 := a.X, a.Y
	x1, y1 := b.X, b.Y
	dx := absInt(x1 - x0)
	dy := absInt(y1 - y0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx - dy
	out := make([]GridPoint, 0, dx+dy+1)
	for {
		out = append(out, GridPoint{X: x0, Y: y0})
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
	return out
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// CheckFormation validates slot-relative tolerance and pairwise maximum
// spacing at normalized progress samples. It returns E_FORM with the first
// violating member pair in stable order.
func CheckFormation(s Scenario, smooth map[string]SmoothedPath, opts SmoothingOptions) error {
	samples := opts.FormationSamples
	if samples <= 1 {
		samples = 65
	}
	maxSpacing := opts.MaxSpacingMilli
	if maxSpacing <= 0 {
		maxSpacing = s.Formation.MaxSpacingMilli
	}
	byGroup := make(map[string][]Member)
	for _, member := range s.Formation.Members {
		byGroup[member.Group] = append(byGroup[member.Group], member)
	}
	groups := make([]string, 0, len(byGroup))
	for group := range byGroup {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		members := byGroup[group]
		for _, member := range members {
			slot, ok := SlotByID(s, member.SlotID)
			if !ok {
				return memberError(CodeForm, member.ID, "member references missing slot "+member.SlotID)
			}
			anchor, ok := MemberByID(s, slot.Anchor)
			if !ok {
				return memberError(CodeForm, member.ID, "slot references missing anchor "+slot.Anchor)
			}
			anchorSmooth, ok := smooth[anchor.ID]
			if !ok || len(anchorSmooth.Corners) < 2 {
				return memberError(CodeForm, anchor.ID, "missing smoothed anchor path")
			}
			memberSmooth, ok := smooth[member.ID]
			if !ok || len(memberSmooth.Corners) < 2 {
				return memberError(CodeForm, member.ID, "missing smoothed member path")
			}
			dx, dxOK := MilliFloat(slot.DX)
			dy, dyOK := MilliFloat(slot.DY)
			tolerance, tolOK := MilliFloat(slot.Tolerance)
			if !dxOK || !dyOK || !tolOK {
				return memberError(CodeForm, member.ID, "slot offset or tolerance is not finite")
			}
			for sample := 0; sample <= samples; sample++ {
				expected := pointAtFraction(anchorSmooth.Corners, int64(sample), int64(samples))
				expected.X += dx
				expected.Y += dy
				actual := pointAtFraction(memberSmooth.Corners, int64(sample), int64(samples))
				deviation := DistanceMilli(expected, actual)
				if deviation > tolerance {
					return &Error{
						Code: CodeForm, MemberID: member.ID,
						Pair: [2]string{anchor.ID, member.ID}, DistanceMilli: deviation,
						Message: fmt.Sprintf("slot deviation %d exceeds tolerance %d at progress sample %d", deviation, tolerance, sample),
					}
				}
			}
		}
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				a := smooth[members[i].ID]
				b := smooth[members[j].ID]
				if len(a.Corners) < 2 || len(b.Corners) < 2 {
					continue
				}
				for sample := 0; sample <= samples; sample++ {
					pa := pointAtFraction(a.Corners, int64(sample), int64(samples))
					pb := pointAtFraction(b.Corners, int64(sample), int64(samples))
					spacing := DistanceMilli(pa, pb)
					if spacing > maxSpacing {
						return &Error{
							Code: CodeForm, DistanceMilli: spacing,
							Pair:    [2]string{members[i].ID, members[j].ID},
							Message: fmt.Sprintf("member spacing %d exceeds maximum %d at progress sample %d", spacing, maxSpacing, sample),
						}
					}
				}
			}
		}
	}
	return nil
}

func pointAtFraction(corners []Corner, numerator, denominator int64) FixPt {
	if len(corners) == 0 {
		return FixPt{}
	}
	if len(corners) == 1 || denominator <= 0 {
		return corners[0].Fixed
	}
	lengths := make([]int64, len(corners)-1)
	var total int64
	for i := 0; i < len(corners)-1; i++ {
		lengths[i] = DistanceMilli(corners[i].Fixed, corners[i+1].Fixed)
		total += lengths[i]
	}
	if total == 0 {
		return corners[0].Fixed
	}
	target := total * numerator / denominator
	var walked int64
	for i, length := range lengths {
		if target <= walked+length || i == len(lengths)-1 {
			remain := target - walked
			if remain < 0 {
				remain = 0
			}
			if remain > length {
				remain = length
			}
			if length == 0 {
				return corners[i].Fixed
			}
			a := corners[i].Fixed
			b := corners[i+1].Fixed
			return FixPt{
				X: a.X + (b.X-a.X)*remain/length,
				Y: a.Y + (b.Y-a.Y)*remain/length,
			}
		}
		walked += length
	}
	return corners[len(corners)-1].Fixed
}

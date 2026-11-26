package maskqa

import (
	"image"
	"math"
)

// DirectionReport records boundary-normal versus stroke-travel consistency.
type DirectionReport struct {
	DomainNo       int            `json:"domain_no"`
	TruthNo        int            `json:"truth_no"`
	Expected       DirectionClass `json:"expected"`
	Declared       DirectionClass `json:"declared"`
	Observed       DirectionClass `json:"observed"`
	Consistent     bool           `json:"consistent"`
	ScorePermyriad int            `json:"score_permyriad"`
	BoundaryPixels int            `json:"boundary_pixels"`
	Votes          map[string]int `json:"votes"`
	Reason         string         `json:"reason,omitempty"`
}

// VerifyDirections uses boundary Sobel normals and declared stroke vectors.
func VerifyDirections(mask *image.Gray, topo *TopologyReport, project *Project, mapping map[int]int) []DirectionReport {
	if mask == nil || topo == nil || project == nil {
		return nil
	}
	strokeByID := map[string]Stroke{}
	for _, stroke := range project.Strokes {
		strokeByID[stroke.ID] = stroke
	}
	truthByNo := map[int]DomainTruth{}
	for _, td := range project.Truth {
		truthByNo[td.DomainNo] = td
	}

	var reports []DirectionReport
	for _, comp := range topo.Components {
		truthNo := mapping[comp.DomainNo]
		truth := truthByNo[truthNo]
		declared := DirUnknown
		var vectors []pointInt
		for _, id := range truth.StrokeIDs {
			if stroke, ok := strokeByID[id]; ok {
				declared = stroke.Direction
				vectors = append(vectors, pointInt{
					x: stroke.End.X - stroke.Start.X,
					y: stroke.End.Y - stroke.Start.Y,
				})
			}
		}
		if declared == DirUnknown && len(project.Strokes) > 0 {
			declared = project.Strokes[0].Direction
		}

		votes := map[string]int{
			string(DirHorizontal): 0, string(DirVertical): 0,
			string(DirDownRight): 0, string(DirDownLeft): 0,
		}
		boundaryPixels := 0
		validPixels := 0
		for y := comp.MinY; y < comp.MaxY; y++ {
			for x := comp.MinX; x < comp.MaxX; x++ {
				idx := y*topo.Width + x
				if idx < 0 || idx >= len(topo.Labels) || topo.Labels[idx] != comp.DomainNo {
					continue
				}
				if !isBoundaryLabel(topo, x, y, comp.DomainNo) {
					continue
				}
				gx, gy := sobelGray(mask, x, y)
				if gx == 0 && gy == 0 {
					continue
				}
				boundaryPixels++
				tx, ty := -gy, gx
				voteDirection(tx, ty, votes)
				if normalMatchesAnyStroke(gx, gy, vectors) {
					validPixels++
				}
			}
		}

		observed := winningDirection(votes, comp.Holes)
		score := 0
		if boundaryPixels > 0 {
			score = validPixels * 10000 / boundaryPixels
		}
		minScore := project.Tolerances.DirectionScorePermyriad
		if minScore == 0 {
			minScore = DefaultTolerances().DirectionScorePermyriad
		}
		consistent := observed == truth.Direction && score >= minScore
		reason := ""
		if observed != truth.Direction {
			reason = "boundary normal vote does not match truth direction"
		} else if score < minScore {
			reason = "boundary normal/stroke alignment score below tolerance"
		}
		reports = append(reports, DirectionReport{
			DomainNo: comp.DomainNo, TruthNo: truthNo,
			Expected: truth.Direction, Declared: declared, Observed: observed,
			Consistent: consistent, ScorePermyriad: score,
			BoundaryPixels: boundaryPixels, Votes: votes, Reason: reason,
		})
	}
	return reports
}

type pointInt struct{ x, y int }

func isBoundaryLabel(topo *TopologyReport, x, y, label int) bool {
	w, h := topo.Width, topo.Height
	for _, d := range []pointInt{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
		nx, ny := x+d.x, y+d.y
		if nx < 0 || ny < 0 || nx >= w || ny >= h {
			return true
		}
		if topo.Labels[ny*w+nx] != label {
			return true
		}
	}
	return false
}

func sobelGray(mask *image.Gray, x, y int) (int, int) {
	at := func(dx, dy int) int {
		return int(GrayAt(mask, x+dx, y+dy))
	}
	gx := -at(-1, -1) - 2*at(-1, 0) - at(-1, 1) +
		at(1, -1) + 2*at(1, 0) + at(1, 1)
	gy := -at(-1, -1) - 2*at(0, -1) - at(1, -1) +
		at(-1, 1) + 2*at(0, 1) + at(1, 1)
	return gx, gy
}

func voteDirection(tx, ty int, votes map[string]int) {
	ax := absInt(tx)
	ay := absInt(ty)
	switch {
	case ax > 2*ay:
		votes[string(DirHorizontal)]++
	case ay > 2*ax:
		votes[string(DirVertical)]++
	case tx*ty >= 0:
		votes[string(DirDownRight)]++
	default:
		votes[string(DirDownLeft)]++
	}
}

func winningDirection(votes map[string]int, holes int) DirectionClass {
	total := 0
	best := DirUnknown
	bestVotes := -1
	for name, count := range votes {
		total += count
		if count > bestVotes {
			bestVotes = count
			best = DirectionClass(name)
		}
	}
	if holes > 0 && total > 0 {
		spread := 0
		for _, count := range votes {
			if count > total/8 {
				spread++
			}
		}
		if spread >= 3 && bestVotes*10000/total < 6200 {
			return DirClosedRing
		}
	}
	if total == 0 {
		return DirUnknown
	}
	return best
}

func normalMatchesAnyStroke(gx, gy int, strokes []pointInt) bool {
	if len(strokes) == 0 {
		return true
	}
	ng2 := gx*gx + gy*gy
	for _, d := range strokes {
		dd2 := d.x*d.x + d.y*d.y
		if dd2 == 0 {
			continue
		}
		dot := absInt(gx*d.x + gy*d.y)
		dot2 := dot * dot
		scaled := ng2 * dd2
		// Perpendicular side-wall normal: |cos(angle)| <= 0.5.
		if dot2*10000 <= scaled*2500 {
			return true
		}
		// Round cap normal parallel to travel: |cos(angle)| >= 0.7.
		if dot2*10000 >= scaled*4900 {
			return true
		}
	}
	return false
}

// integerSqrt is retained for fixed-point diagnostics and avoids float use.
func integerSqrt(v int64) int64 {
	if v <= 0 {
		return 0
	}
	r := int64(math.Sqrt(float64(v)))
	for (r+1)*(r+1) <= v {
		r++
	}
	for r*r > v {
		r--
	}
	return r
}

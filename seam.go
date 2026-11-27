package terrainbake

import (
	"fmt"
	"math"
	"sort"
)

type BoundaryPoint struct {
	Position int64 `json:"position"`
	HeightMM int64 `json:"height_mm"`
}

type SeamReport struct {
	ID            string          `json:"id"`
	BlockA        string          `json:"block_a"`
	BlockB        string          `json:"block_b"`
	Axis          SeamAxis        `json:"axis"`
	Coordinate    int64           `json:"coordinate"`
	CountA        int             `json:"count_a"`
	CountB        int             `json:"count_b"`
	Aligned       bool            `json:"aligned"`
	MaxFoldDeg    float64         `json:"max_fold_deg"`
	AngleLimitDeg float64         `json:"angle_limit_deg"`
	Exceeded      []int64         `json:"exceeded_positions"`
	Attribution   string          `json:"attribution"`
	AttributionAt int64           `json:"attribution_at"`
	ErrorAUM      int64           `json:"error_a_um"`
	ErrorBUM      int64           `json:"error_b_um"`
	ToleranceUM   int64           `json:"tolerance_um"`
	Points        []BoundaryPoint `json:"points"`
	TruthAligned  bool            `json:"truth_aligned"`
	Message       string          `json:"message,omitempty"`
}

// AnalyzeSeams compares the two boundary vertex sequences point by point,
// computes the profile fold angle, and attributes over-limit folds to
// undersampling or alignment mismatch. A fold that cannot be explained by
// either is returned as E_ATTR with both vertex counts.
func AnalyzeSeams(p *Project, meshes map[string]*Mesh) ([]SeamReport, error) {
	if p == nil {
		return nil, errSchema("project", "nil project")
	}
	reports := make([]SeamReport, 0, len(p.Seams))
	for _, s := range p.Seams {
		a, okA := meshes[s.A]
		b, okB := meshes[s.B]
		if !okA || !okB || a == nil || b == nil {
			return nil, &Error{
				Code:     CodeBlock,
				Boundary: s.ID,
				BlockA:   s.A,
				BlockB:   s.B,
				Message:  "seam analysis requires both block meshes",
			}
		}
		pa := extractBoundary(a, s)
		pb := extractBoundary(b, s)
		report := SeamReport{
			ID: s.ID, BlockA: s.A, BlockB: s.B, Axis: s.Axis,
			Coordinate: s.Coordinate, CountA: len(pa), CountB: len(pb),
			AngleLimitDeg: s.AngleLimitDeg,
			ErrorAUM:      a.BoundaryErrorMaxUM, ErrorBUM: b.BoundaryErrorMaxUM,
			ToleranceUM: minTolerance(a, b),
			Points:      pa,
		}
		report.Aligned = profilesEqual(pa, pb)
		report.MaxFoldDeg, report.Exceeded = profileFold(pa, s.AngleLimitDeg)
		report.TruthAligned = seamMatchesTruth(p, s, pa)
		if !report.Aligned {
			report.Attribution = "alignment_mismatch"
			report.AttributionAt = firstProfileMismatch(pa, pb)
			report.Message = "boundary vertex count, order, position or height differs"
			reports = append(reports, report)
			continue
		}
		if len(report.Exceeded) == 0 {
			if report.ErrorAUM > report.ToleranceUM || report.ErrorBUM > report.ToleranceUM {
				report.Attribution = "undersampling"
				if len(report.Points) > 0 {
					report.AttributionAt = report.Points[len(report.Points)/2].Position
				}
			} else {
				report.Attribution = "ok"
			}
			reports = append(reports, report)
			continue
		}
		report.AttributionAt = report.Exceeded[0]
		if (report.ErrorAUM > report.ToleranceUM || report.ErrorBUM > report.ToleranceUM) ||
			a.Undersampled || b.Undersampled {
			report.Attribution = "undersampling"
			report.Message = "boundary height error exceeds the declared chord tolerance"
			reports = append(reports, report)
			continue
		}
		report.Attribution = "E_ATTR"
		report.Message = "fold exceeds limit but neither undersampling nor alignment mismatch explains it"
		reports = append(reports, report)
		return reports, &Error{
			Code:     CodeAttr,
			Boundary: s.ID,
			BlockA:   s.A,
			BlockB:   s.B,
			CountA:   len(pa),
			CountB:   len(pb),
			Message:  "unattributable boundary fold",
		}
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ID < reports[j].ID })
	return reports, nil
}

func minTolerance(a, b *Mesh) int64 {
	if a.ToleranceUM <= 0 {
		return b.ToleranceUM
	}
	if b.ToleranceUM <= 0 {
		return a.ToleranceUM
	}
	if a.ToleranceUM < b.ToleranceUM {
		return a.ToleranceUM
	}
	return b.ToleranceUM
}

func extractBoundary(m *Mesh, s SeamSpec) []BoundaryPoint {
	out := make([]BoundaryPoint, 0, int((s.End-s.Start)/s.SampleSpacing)+1)
	for _, v := range m.Vertices {
		switch s.Axis {
		case AxisVertical:
			if v.X == s.Coordinate && v.Y >= s.Start && v.Y <= s.End {
				out = append(out, BoundaryPoint{Position: v.Y, HeightMM: v.HeightMM})
			}
		case AxisHorizontal:
			if v.Y == s.Coordinate && v.X >= s.Start && v.X <= s.End {
				out = append(out, BoundaryPoint{Position: v.X, HeightMM: v.HeightMM})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

func profilesEqual(a, b []BoundaryPoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstProfileMismatch(a, b []BoundaryPoint) int64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i].Position != b[i].Position {
				if a[i].Position < b[i].Position {
					return a[i].Position
				}
				return b[i].Position
			}
			return a[i].Position
		}
	}
	if len(a) > n {
		return a[n].Position
	}
	if len(b) > n {
		return b[n].Position
	}
	return 0
}

// profileFold returns the maximum turn angle in the boundary height profile.
// Heights are millimetres and lattice distance is treated as one millimetre
// per unit. This is a deliberate 1000x vertical exaggeration for diagnostics;
// the raw 1-unit = 1-metre slope would be visually indistinguishable.
func profileFold(profile []BoundaryPoint, limit float64) (float64, []int64) {
	if len(profile) < 3 {
		return 0, nil
	}
	maxAngle := 0.0
	var exceeded []int64
	for i := 1; i+1 < len(profile); i++ {
		d1 := float64(profile[i].Position - profile[i-1].Position)
		h1 := float64(profile[i].HeightMM - profile[i-1].HeightMM)
		d2 := float64(profile[i+1].Position - profile[i].Position)
		h2 := float64(profile[i+1].HeightMM - profile[i].HeightMM)
		n1 := math.Hypot(d1, h1)
		n2 := math.Hypot(d2, h2)
		if n1 == 0 || n2 == 0 {
			continue
		}
		dot := d1*d2 + h1*h2
		cos := dot / (n1 * n2)
		if cos > 1 {
			cos = 1
		}
		if cos < -1 {
			cos = -1
		}
		deg := math.Acos(cos) * 180 / math.Pi
		if deg > maxAngle {
			maxAngle = deg
		}
		if deg > limit {
			exceeded = append(exceeded, profile[i].Position)
		}
	}
	return maxAngle, exceeded
}

func seamMatchesTruth(p *Project, s SeamSpec, profile []BoundaryPoint) bool {
	for _, t := range p.Truth.Seams {
		if t.SeamID != s.ID {
			continue
		}
		if len(profile) != len(t.Points) || len(t.HeightsMM) != len(t.Points) {
			return false
		}
		for i := range profile {
			if profile[i].Position != t.Points[i] || profile[i].HeightMM != t.HeightsMM[i] {
				return false
			}
		}
		return true
	}
	return false
}

type TruthCheck struct {
	SamplesChecked int      `json:"samples_checked"`
	SeamsChecked   int      `json:"seams_checked"`
	MaxTrianglesOK bool     `json:"max_triangles_ok"`
	Failures       []string `json:"failures"`
}

// VerifyTruth checks the generated mesh against the only accepted truth
// source: declared sample heights, per-block triangle caps and shared-boundary
// aligned vertex sequences.
func VerifyTruth(p *Project, meshes map[string]*Mesh, seams []SeamReport) (*TruthCheck, error) {
	check := &TruthCheck{MaxTrianglesOK: true}
	for _, sample := range p.Truth.Samples {
		m := meshes[sample.BlockID]
		if m == nil {
			return nil, &Error{Code: CodeBlock, BlockID: sample.BlockID, Message: "missing mesh for truth sample"}
		}
		found := false
		for _, v := range m.Vertices {
			if v.X == sample.X && v.Y == sample.Y {
				found = true
				check.SamplesChecked++
				if v.HeightMM != sample.HeightMM {
					check.Failures = append(check.Failures,
						fmt.Sprintf("%s sample (%d,%d): got %d want %d", sample.BlockID, sample.X, sample.Y, v.HeightMM, sample.HeightMM))
				}
				break
			}
		}
		if !found {
			check.Failures = append(check.Failures,
				fmt.Sprintf("%s sample (%d,%d): not a mesh vertex", sample.BlockID, sample.X, sample.Y))
		}
	}
	for id, cap := range p.Truth.MaxTriangles {
		m := meshes[id]
		if m == nil {
			check.MaxTrianglesOK = false
			check.Failures = append(check.Failures, id+": missing mesh for triangle cap")
			continue
		}
		if len(m.Triangles) > cap {
			check.MaxTrianglesOK = false
			check.Failures = append(check.Failures,
				fmt.Sprintf("%s: %d triangles exceed cap %d", id, len(m.Triangles), cap))
		}
	}
	for _, s := range seams {
		if s.TruthAligned {
			check.SeamsChecked++
		} else {
			check.Failures = append(check.Failures, s.ID+": boundary profile disagrees with truth sequence")
		}
	}
	if len(check.Failures) > 0 {
		return check, &Error{
			Code:     CodeDiff,
			Field:    "truth",
			Mismatch: "truth_verification",
			Message:  check.Failures[0],
			LHS:      "generated mesh",
			RHS:      "analytic truth",
		}
	}
	return check, nil
}

package seams

import (
	"math"
	"sort"

	"terrainpipeline/internal/bake"
	"terrainpipeline/internal/model"
)

type Signals struct {
	ThresholdDeg        float64
	RawAngleDeg         float64
	GeometryAngleDeg    float64
	CorrectedAngleDeg   float64
	AlternativeAngleDeg float64
	EdgeJump            float64
	CrossSlopeDeg       float64
}

func EdgeCoordinate(b *model.Block, edge model.Edge, k int) (int, int) {
	switch edge {
	case model.EdgeLeft:
		return 0, k
	case model.EdgeRight:
		return b.NX - 1, k
	case model.EdgeTop:
		return k, 0
	default:
		return k, b.NZ - 1
	}
}

func inwardCoordinate(b *model.Block, edge model.Edge, k, radius int) (int, int) {
	switch edge {
	case model.EdgeLeft:
		return min(radius, b.NX-1), k
	case model.EdgeRight:
		return max(0, b.NX-1-radius), k
	case model.EdgeTop:
		return k, min(radius, b.NZ-1)
	default:
		return k, max(0, b.NZ-1-radius)
	}
}

func Analyze(project *model.Project, raw map[string]*model.BlockResult) ([]*model.SeamReport, []*model.Error) {
	ordered := append([]model.SeamConstraint(nil), project.Seams...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	reports := make([]*model.SeamReport, 0, len(ordered))
	var seamErrors []*model.Error
	for _, seam := range ordered {
		report, errs := analyzeOne(project, &seam, raw)
		reports = append(reports, report)
		seamErrors = append(seamErrors, errs...)
	}
	return reports, seamErrors
}

func Process(project *model.Project, raw map[string]*model.BlockResult) (map[string]*model.BlockResult, []*model.SeamReport, []*model.Error) {
	reports, errs := Analyze(project, raw)
	final := map[string]*model.BlockResult{}
	for id, result := range raw {
		final[id] = cloneResult(result)
	}
	for _, block := range project.Blocks {
		if final[block.ID] != nil {
			ComposeBlock(project, block.ID, final[block.ID], raw, reports)
			bake.Finalize(final[block.ID])
		}
	}
	for _, seam := range project.Seams {
		report := FindReport(reports, seam.ID)
		if report != nil && final[seam.BlockA] != nil && final[seam.BlockB] != nil {
			recomputeWeldedAngles(seam, final[seam.BlockA], final[seam.BlockB], report)
		}
	}
	return final, reports, errs
}

func analyzeOne(project *model.Project, seam *model.SeamConstraint, results map[string]*model.BlockResult) (*model.SeamReport, []*model.Error) {
	a := project.BlockByID(seam.BlockA)
	b := project.BlockByID(seam.BlockB)
	ra := results[a.ID]
	rb := results[b.ID]
	threshold := seam.ThresholdDeg
	if threshold <= 0 {
		threshold = project.Params.SeamThresholdDeg
	}
	report := &model.SeamReport{
		SeamID: seam.ID, BlockA: a.ID, BlockB: b.ID,
		AttributionCounts: map[model.Attribution]int{},
	}
	var errs []*model.Error

	for k := 0; k < seam.SampleCount; k++ {
		ia, ja := EdgeCoordinate(a, seam.EdgeA, k)
		ib, jb := EdgeCoordinate(b, seam.EdgeB, k)
		pa := ra.Pixels[a.Index(ia, ja)]
		pb := rb.Pixels[b.Index(ib, jb)]
		rawAngle := bake.AngleDeg(pa.Normal, pb.Normal)
		signals := measureSignals(project, seam, a, b, k, rawAngle)
		attribution, classErr := Classify(signals)
		point := model.SeamPointReport{
			Index:               k,
			PixelA:              model.PixelRef{BlockID: a.ID, I: ia, J: ja},
			PixelB:              model.PixelRef{BlockID: b.ID, I: ib, J: jb},
			RawAngleDeg:         round6(rawAngle),
			GeometryAngleDeg:    round6(signals.GeometryAngleDeg),
			AlternativeAngleDeg: round6(signals.AlternativeAngleDeg),
			WeldedAngleDeg:      round6(rawAngle),
			ThresholdDeg:        threshold,
			Violation:           rawAngle > threshold,
			Attribution:         attribution,
			Basis: map[string]float64{
				"corrected_geometry_angle_deg": round6(signals.CorrectedAngleDeg),
				"edge_height_jump":             round6(signals.EdgeJump),
				"cross_slope_deg":              round6(signals.CrossSlopeDeg),
			},
		}
		if point.Violation {
			report.ViolatingCount++
			report.ViolatingPoints = append(report.ViolatingPoints, point)
			if classErr != nil {
				point.Code = model.CodeSeam
				point.Message = classErr.Message
				classErr.BlockID = a.ID + "/" + b.ID
				if classErr.Details == nil {
					classErr.Details = map[string]any{}
				}
				classErr.Details["seam_id"] = seam.ID
				classErr.Details["pixel_a"] = point.PixelA
				classErr.Details["pixel_b"] = point.PixelB
				classErr.Details["raw_angle_deg"] = point.RawAngleDeg
				classErr.Details["normal_a"] = pa.Normal
				classErr.Details["normal_b"] = pb.Normal
				errs = append(errs, classErr)
			}
		}
		report.Points = append(report.Points, point)
		report.AttributionCounts[attribution]++
		report.MaxRawAngleDeg = math.Max(report.MaxRawAngleDeg, rawAngle)
	}
	report.MaxRawAngleDeg = round6(report.MaxRawAngleDeg)
	return report, errs
}

func Classify(s Signals) (model.Attribution, *model.Error) {
	if s.RawAngleDeg <= s.ThresholdDeg {
		return model.AttributionNone, nil
	}
	if s.CorrectedAngleDeg >= s.ThresholdDeg {
		return model.AttributionGeometric, nil
	}
	materialGap := s.RawAngleDeg - s.GeometryAngleDeg
	if materialGap >= 0.75*s.ThresholdDeg && s.GeometryAngleDeg <= 1.25*s.ThresholdDeg {
		return model.AttributionMaterial, nil
	}
	if s.CorrectedAngleDeg <= 0.9*s.ThresholdDeg || s.AlternativeAngleDeg <= s.ThresholdDeg {
		return model.AttributionDifferential, nil
	}
	err := model.NewError(model.CodeSeam, "", "seam.attribution",
		"无法判定接缝突变: raw=%.3f° geometry=%.3f° corrected=%.3f° alternative=%.3f° threshold=%.3f°",
		s.RawAngleDeg, s.GeometryAngleDeg, s.CorrectedAngleDeg, s.AlternativeAngleDeg, s.ThresholdDeg)
	err.Details = map[string]any{
		"raw_angle_deg":         round6(s.RawAngleDeg),
		"geometry_angle_deg":    round6(s.GeometryAngleDeg),
		"corrected_angle_deg":   round6(s.CorrectedAngleDeg),
		"alternative_angle_deg": round6(s.AlternativeAngleDeg),
		"threshold_deg":         round6(s.ThresholdDeg),
	}
	return model.AttributionAmbiguous, err
}

func measureSignals(project *model.Project, seam *model.SeamConstraint, a, b *model.Block, k int, rawAngle float64) Signals {
	ia, ja := EdgeCoordinate(a, seam.EdgeA, k)
	ib, jb := EdgeCoordinate(b, seam.EdgeB, k)
	op := project.Params.Differential
	dhxA, dhzA := bake.HeightDerivatives(op, a, ia, ja)
	dhxB, dhzB := bake.HeightDerivatives(op, b, ib, jb)
	scale := project.Params.HeightScale
	ga := bake.NormalFromDerivatives(dhxA, dhzA, scale)
	gb := bake.NormalFromDerivatives(dhxB, dhzB, scale)
	geometryAngle := bake.AngleDeg(ga, gb)

	cdhxA, cdhzA := correctedDerivatives(a, ia, ja)
	cdhxB, cdhzB := correctedDerivatives(b, ib, jb)
	correctedAngle := bake.AngleDeg(
		bake.NormalFromDerivatives(cdhxA, cdhzA, scale),
		bake.NormalFromDerivatives(cdhxB, cdhzB, scale),
	)

	altA, altB, crossSlope, jump := crossSeamNormals(project, seam, a, b, k, dhxA, dhzA, dhxB, dhzB)
	alternativeAngle := bake.AngleDeg(altA, altB)

	return Signals{
		ThresholdDeg:        seam.ThresholdDeg,
		RawAngleDeg:         rawAngle,
		GeometryAngleDeg:    geometryAngle,
		CorrectedAngleDeg:   correctedAngle,
		AlternativeAngleDeg: alternativeAngle,
		EdgeJump:            math.Abs(jump),
		CrossSlopeDeg:       crossSlope,
	}
}

func correctedDerivatives(b *model.Block, i, j int) (float64, float64) {
	dhx, dhz := bake.HeightDerivatives("central", b, i, j)
	if i == 0 && b.NX >= 3 {
		fpp := (b.Height(2, j) - 2*b.Height(1, j) + b.Height(0, j)) / (b.DX * b.DX)
		dhx -= 0.5 * fpp * b.DX
	} else if i == b.NX-1 && b.NX >= 3 {
		fpp := (b.Height(i, j) - 2*b.Height(i-1, j) + b.Height(i-2, j)) / (b.DX * b.DX)
		dhx += 0.5 * fpp * b.DX
	}
	if j == 0 && b.NZ >= 3 {
		fpp := (b.Height(i, 2) - 2*b.Height(i, 1) + b.Height(i, 0)) / (b.DZ * b.DZ)
		dhz -= 0.5 * fpp * b.DZ
	} else if j == b.NZ-1 && b.NZ >= 3 {
		fpp := (b.Height(i, j) - 2*b.Height(i, j-1) + b.Height(i, j-2)) / (b.DZ * b.DZ)
		dhz += 0.5 * fpp * b.DZ
	}
	return dhx, dhz
}

func crossSeamNormals(project *model.Project, seam *model.SeamConstraint, a, b *model.Block, k int, dhxA, dhzA, dhxB, dhzB float64) (model.Vec3, model.Vec3, float64, float64) {
	ia, ja := EdgeCoordinate(a, seam.EdgeA, k)
	ib, jb := EdgeCoordinate(b, seam.EdgeB, k)
	edgeA := a.Height(ia, ja)
	edgeB := b.Height(ib, jb)
	jump := edgeA - edgeB
	scale := project.Params.HeightScale

	vertical := seam.EdgeA == model.EdgeLeft || seam.EdgeA == model.EdgeRight
	ai, aj := inwardCoordinate(a, seam.EdgeA, k, 1)
	bi, bj := inwardCoordinate(b, seam.EdgeB, k, 1)

	var cross float64
	if vertical {
		coordA := a.OriginX + float64(ai)*a.DX
		coordB := b.OriginX + float64(bi)*b.DX
		hA := a.Height(ai, aj)
		hB := b.Height(bi, bj)
		if coordB < coordA {
			coordA, coordB = coordB, coordA
			hA, hB = hB, hA
		}
		if coordB-coordA != 0 {
			cross = (hB - hA) / (coordB - coordA)
		}
		alongA := dhzA
		alongB := dhzB
		altA := bake.NormalFromDerivatives(cross, alongA, scale)
		altB := bake.NormalFromDerivatives(cross, alongB, scale)
		common := bake.NormalFromDerivatives(cross, (alongA+alongB)/2, scale)
		return altA, altB, math.Acos(clamp(common[2], -1, 1)) * 180 / math.Pi, jump
	}

	coordA := a.OriginZ + float64(aj)*a.DZ
	coordB := b.OriginZ + float64(bj)*b.DZ
	hA := a.Height(ai, aj)
	hB := b.Height(bi, bj)
	if coordB < coordA {
		coordA, coordB = coordB, coordA
		hA, hB = hB, hA
	}
	if coordB-coordA != 0 {
		cross = (hB - hA) / (coordB - coordA)
	}
	alongA := dhxA
	alongB := dhxB
	altA := bake.NormalFromDerivatives(alongA, cross, scale)
	altB := bake.NormalFromDerivatives(alongB, cross, scale)
	common := bake.NormalFromDerivatives((alongA+alongB)/2, cross, scale)
	return altA, altB, math.Acos(clamp(common[2], -1, 1)) * 180 / math.Pi, jump
}

func applyWeld(project *model.Project, seam *model.SeamConstraint, point *model.SeamPointReport, na, nb model.Vec3, ra, rb *model.BlockResult, ia, ja, ib, jb int) {
	if point.Attribution == model.AttributionGeometric {
		maxCorrection := project.Params.WeldMaxCorrection
		theta := bake.AngleDeg(na, nb)
		if theta <= 2*maxCorrection {
			common := bake.Normalize(model.Vec3{(na[0] + nb[0]) / 2, (na[1] + nb[1]) / 2, (na[2] + nb[2]) / 2})
			setNormal(ra, ia, ja, common)
			setNormal(rb, ib, jb, common)
			return
		}
		ta := maxCorrection / theta
		movedA := slerp(na, nb, clamp(ta, 0, 1))
		movedB := slerp(nb, na, clamp(ta, 0, 1))
		setNormal(ra, ia, ja, movedA)
		setNormal(rb, ib, jb, movedB)
		return
	}
	common := bake.Normalize(model.Vec3{(na[0] + nb[0]) / 2, (na[1] + nb[1]) / 2, (na[2] + nb[2]) / 2})
	setNormal(ra, ia, ja, common)
	setNormal(rb, ib, jb, common)
}

func setNormal(r *model.BlockResult, i, j int, n model.Vec3) {
	idx := j*r.NX + i
	r.Pixels[idx].Normal = n
	r.Pixels[idx].RGB = bake.NormalRGB(n)
}

func recomputeWeldedAngles(seam model.SeamConstraint, ra, rb *model.BlockResult, report *model.SeamReport) {
	var maxWelded float64
	unresolved := 0
	for idx := range report.Points {
		p := &report.Points[idx]
		pa := ra.Pixels[p.PixelA.I+p.PixelA.J*ra.NX]
		pb := rb.Pixels[p.PixelB.I+p.PixelB.J*rb.NX]
		welded := bake.AngleDeg(pa.Normal, pb.Normal)
		p.WeldedAngleDeg = round6(welded)
		p.PostViolation = welded > p.ThresholdDeg
		if p.PostViolation {
			unresolved++
		}
		maxWelded = math.Max(maxWelded, welded)
	}
	for idx := range report.ViolatingPoints {
		vp := &report.ViolatingPoints[idx]
		for _, p := range report.Points {
			if p.Index == vp.Index && p.PixelA == vp.PixelA {
				vp.WeldedAngleDeg = p.WeldedAngleDeg
				vp.PostViolation = p.PostViolation
				break
			}
		}
	}
	report.MaxWeldedAngleDeg = round6(maxWelded)
	report.UnresolvedCount = unresolved
}

func slerp(a, b model.Vec3, t float64) model.Vec3 {
	cosTheta := clamp(a[0]*b[0]+a[1]*b[1]+a[2]*b[2], -1, 1)
	theta := math.Acos(cosTheta)
	if theta < 1e-12 {
		return a
	}
	sinTheta := math.Sin(theta)
	wa := math.Sin((1-t)*theta) / sinTheta
	wb := math.Sin(t*theta) / sinTheta
	return bake.Normalize(model.Vec3{
		wa*a[0] + wb*b[0],
		wa*a[1] + wb*b[1],
		wa*a[2] + wb*b[2],
	})
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

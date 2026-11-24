package seam

import (
	"fmt"
	"math"
	"sort"

	"terrainnormalverify/internal/bake"
	"terrainnormalverify/internal/model"
)

type Violation struct {
	AX          int        `json:"ax"`
	AY          int        `json:"ay"`
	BX          int        `json:"bx"`
	BY          int        `json:"by"`
	AngleDeg    float64    `json:"angleDeg"`
	Attribution string     `json:"attribution"`
	Evidence    string     `json:"evidence"`
	NormalA     model.Vec3 `json:"normalA"`
	NormalB     model.Vec3 `json:"normalB"`
}

type SeamReport struct {
	SeamID           string      `json:"seamId"`
	BlockA           int         `json:"blockA"`
	BlockB           int         `json:"blockB"`
	ThresholdDeg     float64     `json:"thresholdDeg"`
	MaxAngleDeg      float64     `json:"maxAngleDeg"`
	FinalMaxAngleDeg float64     `json:"finalMaxAngleDeg"`
	IterationsUsed   int         `json:"iterationsUsed"`
	CorrectedPixels  int         `json:"correctedPixels"`
	Violations       []Violation `json:"violations"`
	FinalViolations  []Violation `json:"finalViolations,omitempty"`
}

type edgePair struct {
	ax int
	ay int
	bx int
	by int
}

func Apply(project *model.Project, results map[int]*bake.BlockResult) ([]SeamReport, error) {
	for _, result := range results {
		copy(result.Normals, result.RawNormals)
	}
	seams := project.SortedSeams()
	reports := make([]SeamReport, 0, len(seams))
	for _, seam := range seams {
		pairs, err := edgePairs(project, seam, results)
		if err != nil {
			return nil, err
		}
		report := SeamReport{
			SeamID:       seam.ID,
			BlockA:       seam.BlockA,
			BlockB:       seam.BlockB,
			ThresholdDeg: seam.ThresholdDeg,
		}
		for _, pair := range pairs {
			a := results[seam.BlockA]
			b := results[seam.BlockB]
			na := a.RawNormals[a.Index(pair.ax, pair.ay)]
			nb := b.RawNormals[b.Index(pair.bx, pair.by)]
			angle := model.AngleDeg(na, nb)
			if angle > report.MaxAngleDeg {
				report.MaxAngleDeg = angle
			}
			if angle > seam.ThresholdDeg+1e-9 {
				report.Violations = append(report.Violations, violation(project, seam, pair, na, nb, angle))
			}
		}
		reports = append(reports, report)
	}

	maxIterations := 0
	iterationsUsed := make(map[string]int)
	for _, seam := range seams {
		if seam.MaxIterations > maxIterations {
			maxIterations = seam.MaxIterations
		}
	}
	for iteration := 1; iteration <= maxIterations; iteration++ {
		for _, seam := range seams {
			if iteration > seam.MaxIterations {
				continue
			}
			pairs, err := edgePairs(project, seam, results)
			if err != nil {
				return nil, err
			}
			a := results[seam.BlockA]
			b := results[seam.BlockB]
			corrected := false
			for _, pair := range pairs {
				ai := a.Index(pair.ax, pair.ay)
				bi := b.Index(pair.bx, pair.by)
				na := a.Normals[ai]
				nb := b.Normals[bi]
				angle := model.AngleDeg(na, nb)
				if angle <= seam.ThresholdDeg+1e-9 {
					continue
				}
				alpha := project.Parameters.SeamRelaxation
				a.Normals[ai] = model.Vec3{
					X: na.X*(1-alpha) + nb.X*alpha,
					Y: na.Y*(1-alpha) + nb.Y*alpha,
					Z: na.Z*(1-alpha) + nb.Z*alpha,
				}.Normalize()
				b.Normals[bi] = model.Vec3{
					X: nb.X*(1-alpha) + na.X*alpha,
					Y: nb.Y*(1-alpha) + na.Y*alpha,
					Z: nb.Z*(1-alpha) + na.Z*alpha,
				}.Normalize()
				corrected = true
			}
			if corrected {
				iterationsUsed[seam.ID] = iteration
			}
		}
		converged := true
		for _, seam := range seams {
			pairs, err := edgePairs(project, seam, results)
			if err != nil {
				return nil, err
			}
			a := results[seam.BlockA]
			b := results[seam.BlockB]
			for _, pair := range pairs {
				if model.AngleDeg(a.Normals[a.Index(pair.ax, pair.ay)], b.Normals[b.Index(pair.bx, pair.by)]) > seam.ThresholdDeg+1e-9 {
					converged = false
					break
				}
			}
			if !converged {
				break
			}
		}
		if converged {
			break
		}
	}

	for i := range reports {
		seam := findSeam(seams, reports[i].SeamID)
		pairs, err := edgePairs(project, seam, results)
		if err != nil {
			return nil, err
		}
		a := results[seam.BlockA]
		b := results[seam.BlockB]
		for _, pair := range pairs {
			ai := a.Index(pair.ax, pair.ay)
			bi := b.Index(pair.bx, pair.by)
			angle := model.AngleDeg(a.Normals[ai], b.Normals[bi])
			if angle > reports[i].FinalMaxAngleDeg {
				reports[i].FinalMaxAngleDeg = angle
			}
			if angle > seam.ThresholdDeg+1e-9 {
				reports[i].FinalViolations = append(reports[i].FinalViolations, violation(
					project, seam, pair, a.Normals[ai], b.Normals[bi], angle,
				))
			}
		}
		reports[i].IterationsUsed = iterationsUsed[seam.ID]
		reports[i].CorrectedPixels = len(reports[i].Violations)
		if len(reports[i].FinalViolations) > 0 {
			worst := reports[i].FinalViolations[0]
			for _, v := range reports[i].FinalViolations[1:] {
				if v.AngleDeg > worst.AngleDeg {
					worst = v
				}
			}
			e := model.NewError(model.ErrSeam, seam.BlockA, "finalMaxAngleDeg",
				fmt.Sprintf("seam %s blocks %d/%d still violates continuity after %d iterations", seam.ID, seam.BlockA, seam.BlockB, seam.MaxIterations))
			e.BlockID = seam.BlockA
			e.Value = map[string]any{
				"seamId":       seam.ID,
				"blockA":       seam.BlockA,
				"blockB":       seam.BlockB,
				"angleDeg":     worst.AngleDeg,
				"thresholdDeg": seam.ThresholdDeg,
				"ax":           worst.AX,
				"ay":           worst.AY,
				"bx":           worst.BX,
				"by":           worst.BY,
			}
			return nil, e
		}
	}

	for _, result := range results {
		bake.RecomputeDerived(result)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].SeamID < reports[j].SeamID })
	return reports, nil
}

func violation(project *model.Project, seam model.Seam, pair edgePair, na, nb model.Vec3, angle float64) Violation {
	a, _ := project.Block(seam.BlockA)
	b, _ := project.Block(seam.BlockB)
	hJump := math.Abs(a.HeightAt(pair.ax, pair.ay) - b.HeightAt(pair.bx, pair.by))
	weightsA := a.WeightsAt(pair.ax, pair.ay)
	weightsB := b.WeightsAt(pair.bx, pair.by)
	totalA, totalB := 0, 0
	for i := range weightsA {
		totalA += weightsA[i]
		totalB += weightsB[i]
	}
	weightDiff := 0.0
	if totalA > 0 && totalB > 0 {
		for i := range weightsA {
			weightDiff += math.Abs(float64(weightsA[i])/float64(totalA) - float64(weightsB[i])/float64(totalB))
		}
	}
	weightDiff *= 0.5
	rawSlopeA := model.SlopeDeg(na)
	rawSlopeB := model.SlopeDeg(nb)
	rawSlope := math.Max(rawSlopeA, rawSlopeB)
	category := "difference_operator"
	evidence := fmt.Sprintf("heightJump=%.9f slope=%.6f weightDiff=%.6f", hJump, rawSlope, weightDiff)
	if hJump > 1e-6 || rawSlope >= seam.GeometrySlopeThresholdDeg {
		category = "geometric_fold"
		evidence = fmt.Sprintf("heightJump=%.9f slope=%.6f >= geometryThreshold=%.6f", hJump, rawSlope, seam.GeometrySlopeThresholdDeg)
	} else if weightDiff >= 0.06 {
		category = "material_mix"
		evidence = fmt.Sprintf("heightJump=%.9f slope=%.6f weightDiff=%.6f", hJump, rawSlope, weightDiff)
	}
	return Violation{
		AX:          pair.ax,
		AY:          pair.ay,
		BX:          pair.bx,
		BY:          pair.by,
		AngleDeg:    angle,
		Attribution: category,
		Evidence:    evidence,
		NormalA:     na,
		NormalB:     nb,
	}
}

func edgePairs(project *model.Project, seam model.Seam, results map[int]*bake.BlockResult) ([]edgePair, error) {
	aBlock, okA := project.Block(seam.BlockA)
	bBlock, okB := project.Block(seam.BlockB)
	if !okA || !okB {
		return nil, model.NewError(model.ErrSchema, seam.BlockA, "seam.blocks", "seam references missing block")
	}
	if _, ok := results[seam.BlockA]; !ok {
		return nil, model.NewError(model.ErrBlock, seam.BlockA, "seam.result", "missing bake result")
	}
	if _, ok := results[seam.BlockB]; !ok {
		return nil, model.NewError(model.ErrBlock, seam.BlockB, "seam.result", "missing bake result")
	}
	pairs := make([]edgePair, 0)
	switch seam.Axis {
	case "vertical":
		if aBlock.Height != bBlock.Height {
			e := model.NewError(model.ErrSize, seam.BlockA, "seam.edgeSamples", "shared edge lengths differ")
			e.Value = aBlock.Height
			e.Want = bBlock.Height
			return nil, e
		}
		for y := 0; y < aBlock.Height; y++ {
			pairs = append(pairs, edgePair{ax: aBlock.Width - 1, ay: y, bx: 0, by: y})
		}
	case "horizontal":
		if aBlock.Width != bBlock.Width {
			e := model.NewError(model.ErrSize, seam.BlockA, "seam.edgeSamples", "shared edge lengths differ")
			e.Value = aBlock.Width
			e.Want = bBlock.Width
			return nil, e
		}
		for x := 0; x < aBlock.Width; x++ {
			pairs = append(pairs, edgePair{ax: x, ay: aBlock.Height - 1, bx: x, by: 0})
		}
	default:
		return nil, model.NewError(model.ErrSchema, seam.BlockA, "seam.axis", "unsupported seam axis")
	}
	return pairs, nil
}

func findSeam(seams []model.Seam, id string) model.Seam {
	for _, seam := range seams {
		if seam.ID == id {
			return seam
		}
	}
	return model.Seam{}
}

func AggregateByBlock(reports []SeamReport) map[int][]SeamReport {
	out := make(map[int][]SeamReport)
	for _, report := range reports {
		out[report.BlockA] = append(out[report.BlockA], report)
		if report.BlockB != report.BlockA {
			out[report.BlockB] = append(out[report.BlockB], report)
		}
	}
	for id := range out {
		sort.Slice(out[id], func(i, j int) bool { return out[id][i].SeamID < out[id][j].SeamID })
	}
	return out
}

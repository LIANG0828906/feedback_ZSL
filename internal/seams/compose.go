package seams

import (
	"sort"

	"terrainpipeline/internal/bake"
	"terrainpipeline/internal/model"
)

func ComposeBlock(project *model.Project, blockID string, dst *model.BlockResult, raw map[string]*model.BlockResult, reports []*model.SeamReport) {
	block := project.BlockByID(blockID)
	if block == nil || dst == nil || raw[blockID] == nil {
		return
	}
	incident := append([]model.SeamConstraint(nil), project.Seams...)
	sort.SliceStable(incident, func(i, j int) bool { return incident[i].ID < incident[j].ID })
	for _, seam := range incident {
		if seam.BlockA != blockID && seam.BlockB != blockID {
			continue
		}
		report := FindReport(reports, seam.ID)
		if report == nil {
			continue
		}
		for _, point := range report.Points {
			if !point.Violation || point.Attribution == model.AttributionAmbiguous {
				continue
			}
			var ownRef, otherRef model.PixelRef
			if point.PixelA.BlockID == blockID {
				ownRef, otherRef = point.PixelA, point.PixelB
			} else if point.PixelB.BlockID == blockID {
				ownRef, otherRef = point.PixelB, point.PixelA
			} else {
				continue
			}
			ownResult := raw[ownRef.BlockID]
			otherResult := raw[otherRef.BlockID]
			if ownResult == nil || otherResult == nil {
				continue
			}
			own := ownResult.Pixels[ownRef.J*ownResult.NX+ownRef.I].Normal
			other := otherResult.Pixels[otherRef.J*otherResult.NX+otherRef.I].Normal
			corrected := CorrectSideNormal(project, point.Attribution, own, other)
			setNormal(dst, ownRef.I, ownRef.J, corrected)
		}
	}
}

func CorrectSideNormal(project *model.Project, attribution model.Attribution, own, other model.Vec3) model.Vec3 {
	theta := bake.AngleDeg(own, other)
	if theta == 0 {
		return own
	}
	if attribution == model.AttributionGeometric {
		maxCorrection := project.Params.WeldMaxCorrection
		if theta <= 2*maxCorrection {
			return bake.Normalize(model.Vec3{(own[0] + other[0]) / 2, (own[1] + other[1]) / 2, (own[2] + other[2]) / 2})
		}
		return slerp(own, other, clamp(maxCorrection/theta, 0, 1))
	}
	return bake.Normalize(model.Vec3{(own[0] + other[0]) / 2, (own[1] + other[1]) / 2, (own[2] + other[2]) / 2})
}

func RecomputeWeldedAngles(project *model.Project, final map[string]*model.BlockResult, reports []*model.SeamReport) {
	for _, seam := range project.Seams {
		report := FindReport(reports, seam.ID)
		ra := final[seam.BlockA]
		rb := final[seam.BlockB]
		if report == nil || ra == nil || rb == nil {
			continue
		}
		recomputeWeldedAngles(seam, ra, rb, report)
	}
}

func FindReport(reports []*model.SeamReport, id string) *model.SeamReport {
	for _, report := range reports {
		if report != nil && report.SeamID == id {
			return report
		}
	}
	return nil
}

func cloneResult(in *model.BlockResult) *model.BlockResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Pixels = make([]model.PixelBake, len(in.Pixels))
	for i, pixel := range in.Pixels {
		out.Pixels[i] = pixel
		if pixel.Weights != nil {
			out.Pixels[i].Weights = append([]uint8(nil), pixel.Weights...)
		}
	}
	return &out
}

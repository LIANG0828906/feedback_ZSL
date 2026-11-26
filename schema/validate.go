package schema

import (
	"fmt"

	"maskqc/model"
)

func validateProject(project *model.Project) model.Errors {
	var problems model.Errors
	if project.ID == "" {
		problems = append(problems, missing("$.project", project.ID))
	}
	if project.Width <= 0 || project.Height <= 0 {
		problems = append(problems, model.NewError(model.CodeSchema, "$.canvas", project.ID, "width and height must be positive"))
		return problems
	}
	if project.Width > 1<<20 || project.Height > 1<<20 || project.Width*project.Height > 1<<28 {
		problems = append(problems, model.NewError(model.CodeSchema, "$.canvas", project.ID, "canvas exceeds supported dimensions"))
		return problems
	}
	expectedMask := project.Width * project.Height
	if len(project.CandidateMask) != expectedMask {
		problems = append(problems, model.NewError(model.CodeSchema, "$.candidateMask", project.ID,
			fmt.Sprintf("mask length %d does not match %dx%d", len(project.CandidateMask), project.Width, project.Height)))
	}
	strokeIDs := map[string]bool{}
	seqs := map[int]bool{}
	for index, stroke := range project.Strokes {
		path := fmt.Sprintf("$.strokes[%d]", index)
		if stroke.ID == "" {
			problems = append(problems, missing(path+".id", project.ID))
		} else if strokeIDs[stroke.ID] {
			problems = append(problems, model.NewError(model.CodeSchema, path+".id", stroke.ID, "duplicate stroke id"))
		}
		strokeIDs[stroke.ID] = true
		if stroke.Seq <= 0 {
			problems = append(problems, model.NewError(model.CodeSchema, path+".seq", stroke.ID, "stroke sequence must be positive"))
		} else if seqs[stroke.Seq] {
			problems = append(problems, model.NewError(model.CodeSchema, path+".seq", stroke.ID, "duplicate stroke sequence"))
		}
		seqs[stroke.Seq] = true
		if stroke.Radius <= 0 {
			problems = append(problems, &model.Error{
				Code: model.CodeParam, Path: path + ".radius", ObjectID: stroke.ID,
				Field: "radius", Message: "brush radius must be positive", Allowed: "1..1048576",
			})
		}
		if stroke.Hardness < 0 || stroke.Hardness > model.MaxHardness {
			problems = append(problems, &model.Error{
				Code: model.CodeParam, Path: path + ".hardness", ObjectID: stroke.ID,
				Field: "hardness", Message: "brush hardness is outside the allowed range", Allowed: "0..100",
			})
		}
		if stroke.Weight < 0 || stroke.Weight > model.MaxCoverage {
			problems = append(problems, &model.Error{
				Code: model.CodeParam, Path: path + ".weight", ObjectID: stroke.ID,
				Field: "weight", Message: "coverage weight is outside the allowed range", Allowed: "0..10000",
			})
		}
		if stroke.Direction == "" {
			problems = append(problems, model.NewError(model.CodeSchema, path+".direction", stroke.ID, "direction category is empty"))
		}
	}
	groupIDs := map[string]bool{}
	for index, group := range project.Groups {
		path := fmt.Sprintf("$.strokeGroups[%d]", index)
		if group.ID == "" {
			problems = append(problems, missing(path+".id", project.ID))
		} else if groupIDs[group.ID] {
			problems = append(problems, model.NewError(model.CodeSchema, path+".id", group.ID, "duplicate stroke group id"))
		}
		groupIDs[group.ID] = true
		for _, strokeID := range group.StrokeIDs {
			if !strokeIDs[strokeID] {
				problems = append(problems, model.NewError(model.CodeSchema, path+".strokeIds", group.ID, "unknown stroke "+strokeID))
			}
		}
	}
	for index, truth := range project.Truth {
		path := fmt.Sprintf("$.truth[%d]", index)
		if truth.ID == "" {
			problems = append(problems, missing(path+".id", project.ID))
		}
		if truth.Area < 0 || truth.Holes < 0 || truth.RingWidth < 0 || truth.Perimeter < 0 {
			problems = append(problems, model.NewError(model.CodeSchema, path, truth.ID, "truth metrics must be non-negative"))
		}
		if truth.BBox.Empty() {
			problems = append(problems, model.NewError(model.CodeSchema, path+".bbox", truth.ID, "truth bbox is empty"))
		}
		if len(truth.ReferenceMask) != expectedMask {
			problems = append(problems, model.NewError(model.CodeSchema, path+".refMask", truth.ID,
				fmt.Sprintf("reference mask length %d does not match canvas", len(truth.ReferenceMask))))
		}
		if truth.Direction == "" {
			problems = append(problems, model.NewError(model.CodeSchema, path+".direction", truth.ID, "direction category is empty"))
		}
		for segmentIndex, segment := range truth.BoundarySegments {
			segmentPath := fmt.Sprintf("%s.segments[%d]", path, segmentIndex)
			if segment.NX < -1 || segment.NX > 1 || segment.NY < -1 || segment.NY > 1 || segment.NX*segment.NX+segment.NY*segment.NY != 1 {
				problems = append(problems, model.NewError(model.CodeSchema, segmentPath+".normal", truth.ID, "normal must be a 4-neighbour unit vector"))
			}
			if segment.Direction == "" {
				problems = append(problems, model.NewError(model.CodeSchema, segmentPath+".direction", truth.ID, "direction category is empty"))
			}
		}
	}
	return problems
}

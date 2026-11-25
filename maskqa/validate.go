package maskqa

import "fmt"

// ValidateProject checks canonical parameter ranges after schema mapping.
func ValidateProject(p *Project) error {
	if p == nil {
		return schemaError("", "$", "nil project")
	}
	if p.SourceVersion == "" {
		p.SourceVersion = SchemaV3
	}
	if p.Width <= 0 || p.Height <= 0 {
		e := paramError("size", "width_px>0 and height_px>0", "invalid dimensions %dx%d", p.Width, p.Height)
		e.SourceVersion = p.SourceVersion
		return e
	}
	if p.Candidate == nil || p.Candidate.Width != p.Width || p.Candidate.Height != p.Height {
		e := paramError("candidate_mask.size", fmt.Sprintf("%dx%d", p.Width, p.Height), "candidate mask dimensions do not match project")
		e.SourceVersion = p.SourceVersion
		return e
	}
	if p.BrushWidthPx < 1 || p.BrushWidthPx > maxInt(p.Width, p.Height)*2 {
		e := paramError("brush_width_px", fmt.Sprintf("1..%d", maxInt(p.Width, p.Height)*2), "brush width %d out of range", p.BrushWidthPx)
		e.SourceVersion = p.SourceVersion
		return e
	}
	if p.CutAlphaBPS < 1 || p.CutAlphaBPS > WeightBasisScale {
		e := paramError("cut_alpha_bps", "1..10000", "cut alpha %d out of range", p.CutAlphaBPS)
		e.SourceVersion = p.SourceVersion
		return e
	}
	switch p.WeightScale {
	case "permille", "basis_points":
	default:
		e := paramError("weight_scale", "permille|basis_points", "unsupported weight scale %q", p.WeightScale)
		e.SourceVersion = p.SourceVersion
		return e
	}
	if len(p.Strokes) == 0 {
		e := schemaError(p.SourceVersion, "strokes", "at least one stroke is required")
		return e
	}
	seenIndex := map[int]bool{}
	for i, s := range p.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		if s.Index <= 0 {
			e := paramError(path+".index", ">=1", "stroke index must be positive")
			e.SourceVersion = p.SourceVersion
			return e
		}
		if seenIndex[s.Index] {
			// Duplicate indexes are surfaced as ordering conflicts during adjudication.
			continue
		}
		seenIndex[s.Index] = true
		if s.WeightBPS < 1 || s.WeightBPS > WeightBasisScale {
			e := paramError(path+".weight", "1..10000 basis points (1..1000 permille)", "weight %d bps is outside allowed scale", s.WeightBPS)
			e.SourceVersion = p.SourceVersion
			return e
		}
		if s.HardnessPermille < 0 || s.HardnessPermille > WeightPermilleScale {
			e := paramError(path+".hardness_permille", "0..1000", "hardness %d is outside allowed interval", s.HardnessPermille)
			e.SourceVersion = p.SourceVersion
			return e
		}
		if s.Start.X < -p.Width || s.Start.X > p.Width*2 || s.Start.Y < -p.Height || s.Start.Y > p.Height*2 {
			e := paramError(path+".start", fmt.Sprintf("x in [0,%d], y in [0,%d]", p.Width, p.Height), "start %v outside declared image", s.Start)
			e.SourceVersion = p.SourceVersion
			return e
		}
		if s.End.X < -p.Width || s.End.X > p.Width*2 || s.End.Y < -p.Height || s.End.Y > p.Height*2 {
			e := paramError(path+".end", fmt.Sprintf("x in [0,%d], y in [0,%d]", p.Width, p.Height), "end %v outside declared image", s.End)
			e.SourceVersion = p.SourceVersion
			return e
		}
		if s.Start == s.End {
			e := paramError(path+".end", "different from start", "zero-length stroke %d", s.Index)
			e.SourceVersion = p.SourceVersion
			return e
		}
	}
	return nil
}

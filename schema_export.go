package maskqa

import (
	"encoding/json"
	"fmt"
)

// ExportProject converts the unified model to a specific schema version.
func ExportProject(project *Project, version string) (*ExportResult, error) {
	if project == nil {
		return nil, decodeError("project", "nil project")
	}
	switch version {
	case SchemaV1:
		return exportV1(project)
	case SchemaV2:
		return exportV2(project)
	case SchemaV3:
		return exportV3(project)
	default:
		return nil, &Error{Code: CodeDecode, Field: "schema_version", Message: "unknown export version", Actual: version}
	}
}

func exportV3(p *Project) (*ExportResult, error) {
	w, h, candidateB64 := encodeGray(p.Candidate)
	_, _, referenceB64 := encodeGray(p.Reference)
	iconB64, err := encodePNG(p.Icon)
	if err != nil {
		return nil, err
	}
	strokes := make([]strokeV3, 0, len(p.Strokes))
	for _, s := range p.Strokes {
		seq := s.Seq
		priority := s.Priority
		width := s.WidthPx
		hardness := s.HardnessPermyriad
		weight := s.WeightPermyriad
		code := s.DirectionCode
		if code == 0 {
			code = DirectionToCode(s.Direction)
		}
		strokes = append(strokes, strokeV3{
			ID: s.ID, Seq: &seq, Group: s.Group, Priority: &priority,
			Start: &Point{X: s.Start.X, Y: s.Start.Y}, End: &Point{X: s.End.X, Y: s.End.Y},
			WidthPx: &width, HardnessPermyriad: &hardness, WeightPermyriad: &weight,
			Direction: s.Direction, DirectionCode: &code,
		})
	}
	id, seed, width, height := p.ID, p.Seed, p.Width, p.Height
	data, err := json.MarshalIndent(dtoV3{
		SchemaVersion: SchemaV3, ID: &id, Seed: &seed, Width: &width, Height: &height,
		TileSize: p.TileSize, IconPNGB64: iconB64,
		Candidate: &maskV3{WidthPx: &w, HeightPx: &h, Gray8Base64: candidateB64},
		Reference: &maskV3{WidthPx: &w, HeightPx: &h, Gray8Base64: referenceB64},
		Strokes:   strokes, Truth: p.Truth, Tolerances: &p.Tolerances,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Schema: SchemaV3, Data: data}, nil
}

func exportV2(p *Project) (*ExportResult, error) {
	w, h, candidateB64 := encodeGray(p.Candidate)
	_, _, referenceB64 := encodeGray(p.Reference)
	iconB64, err := encodePNG(p.Icon)
	if err != nil {
		return nil, err
	}
	strokes := make([]strokeV2, 0, len(p.Strokes))
	conversions := []TruncationRecord{}
	for i, s := range p.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		originX, err := SequenceOrigin(s.Seq, 0, p.Width)
		if err != nil {
			return nil, err
		}
		originY, err := SequenceOrigin(s.Seq, 1, p.Height)
		if err != nil {
			return nil, err
		}
		sx, err := OffsetFromAbsolute(int64(s.Start.X), int64(originX), path+".startOffsetX")
		if err != nil {
			return nil, err
		}
		sy, err := OffsetFromAbsolute(int64(s.Start.Y), int64(originY), path+".startOffsetY")
		if err != nil {
			return nil, err
		}
		ex, err := OffsetFromAbsolute(int64(s.End.X), int64(originX), path+".endOffsetX")
		if err != nil {
			return nil, err
		}
		ey, err := OffsetFromAbsolute(int64(s.End.Y), int64(originY), path+".endOffsetY")
		if err != nil {
			return nil, err
		}
		seq := s.Seq
		priority := s.Priority
		widthPx := s.WidthPx
		weight := int64(s.WeightPermyriad)
		hardness := s.HardnessPermyriad
		code := s.DirectionCode
		if code == 0 {
			code = DirectionToCode(s.Direction)
		}
		conversions = append(conversions, TruncationRecord{
			Field: path + ".coordinates", Operation: "absolute_to_sequence_offset",
			Position: path, OperandA: int64(originX), OperandB: int64(originY),
			Result: sx, Rounding: "integer_subtraction_exact",
		})
		strokes = append(strokes, strokeV2{
			StrokeID: s.ID, Sequence: &seq, CompositionPriority: &priority, GroupName: s.Group,
			StartOffsetX: &sx, StartOffsetY: &sy, EndOffsetX: &ex, EndOffsetY: &ey,
			WidthPx: &widthPx, WeightPermyriad: &weight, HardnessPermyriad: &hardness,
			DirectionCode: &code,
		})
	}
	truth := make([]truthV2, 0, len(p.Truth))
	for _, t := range p.Truth {
		no, cx, cy, area := t.DomainNo, t.CentroidXMilli, t.CentroidYMilli, t.Area
		minX, minY, maxX, maxY := t.MinX, t.MinY, t.MaxX, t.MaxY
		holes, band := t.Holes, t.BandWidthMilli
		code := t.DirectionCode
		if code == 0 {
			code = DirectionToCode(t.Direction)
		}
		truth = append(truth, truthV2{
			DomainNo: &no, CentroidXMilli: &cx, CentroidYMilli: &cy, Area: &area,
			MinX: &minX, MinY: &minY, MaxX: &maxX, MaxY: &maxY, Holes: &holes,
			BandWidthMilli: &band, DirectionCode: &code, StrokeIDs: t.StrokeIDs,
		})
	}
	id, seed, width, height := p.ID, p.Seed, p.Width, p.Height
	data, err := json.MarshalIndent(dtoV2{
		SchemaVersion: SchemaV2, ProjectID: &id, Seed: &seed,
		CanvasWidth: &width, CanvasHeight: &height, TileSize: p.TileSize,
		IconPNGBase64: iconB64,
		Candidate:     &maskV2{Width: &w, Height: &h, PixelsBase64: candidateB64},
		Reference:     &maskV2{Width: &w, Height: &h, PixelsBase64: referenceB64},
		Strokes:       strokes, TruthDomains: truth, Tolerances: &p.Tolerances,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Schema: SchemaV2, Data: data, Conversions: conversions}, nil
}

func exportV1(p *Project) (*ExportResult, error) {
	_, _, candidateB64 := encodeGray(p.Candidate)
	_, _, referenceB64 := encodeGray(p.Reference)
	iconB64, err := encodePNG(p.Icon)
	if err != nil {
		return nil, err
	}
	strokes := make([]strokeV1, 0, len(p.Strokes))
	conversions := []TruncationRecord{}
	lossy := []LossyIssue{}
	groups := map[string]bool{}
	recovery := &recoveryV1{}
	for i, s := range p.Strokes {
		path := fmt.Sprintf("stroke_records[%d]", i)
		weight, remainder, err := ScalePermyriadToPermille(
			int64(s.WeightPermyriad), path+".cover_weight_permille", path, &conversions)
		if err != nil {
			return nil, err
		}
		no, priority := s.Seq, s.Priority
		x0, y0, x1, y1 := s.Start.X, s.Start.Y, s.End.X, s.End.Y
		widthPx := s.WidthPx
		var remPtr *int64
		if remainder != 0 {
			remCopy := remainder
			remPtr = &remCopy
		}
		strokes = append(strokes, strokeV1{
			No: &no, StrokeID: s.ID, OrderPriority: &priority,
			X0: &x0, Y0: &y0, X1: &x1, Y1: &y1, BrushWidthPx: &widthPx,
			WeightPermille: &weight, WeightRemainder: remPtr,
		})
		if s.Group != "" && !groups[s.Group] {
			groups[s.Group] = true
			lossy = append(lossy, LossyIssue{
				Code: CodeLossy, Field: path + ".pen_group",
				Message:     fmt.Sprintf("grouped structure %q is not expressible in %s", s.Group, SchemaV1),
				Disposition: "standard_v1_field_omitted; original moved to x_maskqa_recovery for equivalent round-trip", Value: s.Group,
			})
		}
		if s.Direction != "" && s.Direction != DirUnknown {
			lossy = append(lossy, LossyIssue{
				Code: CodeLossy, Field: path + ".direction_code",
				Message:     "direction category encoding is absent in v1",
				Disposition: "standard_v1_field_omitted; default trace recorded; original moved to x_maskqa_recovery", Value: s.Direction,
			})
		}
		if s.HardnessPermyriad != DefaultHardnessPermyriad {
			lossy = append(lossy, LossyIssue{
				Code: CodeLossy, Field: path + ".hardness",
				Message:     "brush hardness field is absent in v1",
				Disposition: "standard_v1_field_omitted; default trace recorded; original moved to x_maskqa_recovery", Value: s.HardnessPermyriad,
			})
		}
		recovery.Strokes = append(recovery.Strokes, recoveryStrokeV1{
			StrokeID: s.ID, Group: s.Group, HardnessPermyriad: s.HardnessPermyriad,
			Direction: s.Direction, DirectionCode: s.DirectionCode,
			HasHardness: s.HasHardness, HasDirection: s.HasDirection,
		})
	}
	truth := make([]truthV1, 0, len(p.Truth))
	for i, t := range p.Truth {
		no, cx, cy, area := t.DomainNo, t.CentroidXMilli, t.CentroidYMilli, t.Area
		x0, y0, x1, y1 := t.MinX, t.MinY, t.MaxX, t.MaxY
		holes, band := t.Holes, t.BandWidthMilli
		truth = append(truth, truthV1{
			DomainIdx: &no, CXMilli: &cx, CYMilli: &cy, PixelArea: &area,
			BoxX0: &x0, BoxY0: &y0, BoxX1: &x1, BoxY1: &y1,
			HoleCnt: &holes, RingWidthMilli: &band, StrokeRefs: t.StrokeIDs,
		})
		if t.Direction != "" && t.Direction != DirUnknown {
			lossy = append(lossy, LossyIssue{
				Code: CodeLossy, Field: fmt.Sprintf("truth_items[%d].direction_code", i),
				Message:     "truth direction category encoding is absent in v1",
				Disposition: "fill_declared_default_unknown_on_load", Value: t.Direction,
			})
		}
		recovery.Truth = append(recovery.Truth, recoveryTruthV1{
			DomainNo: t.DomainNo, Direction: t.Direction, DirectionCode: t.DirectionCode,
		})
	}
	id, seed, width, height := p.ID, p.Seed, p.Width, p.Height
	data, err := json.MarshalIndent(dtoV1{
		Schema: SchemaV1, ProjID: &id, SeedNo: &seed, CanvasW: &width, CanvasH: &height,
		TileSz: p.TileSize, IconPNGB64: iconB64,
		CandidateMask: &maskV1{W: &width, H: &height, PixB64: candidateB64},
		ReferenceMask: &maskV1{W: &width, H: &height, PixB64: referenceB64},
		StrokeRecords: strokes, TruthItems: truth, Tolerances: &p.Tolerances, Recovery: recovery,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Schema: SchemaV1, Data: data, Lossy: lossy, Conversions: conversions}, nil
}

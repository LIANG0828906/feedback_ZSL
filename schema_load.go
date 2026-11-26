package maskqa

import (
	"encoding/json"
	"errors"
	"fmt"
)

func unmarshalDTO(data []byte, dto any) error {
	if err := json.Unmarshal(data, dto); err != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) && typeError.Field != "" {
			return decodeError(typeError.Field, "invalid field type: %v", err)
		}
		return decodeError("$", "invalid JSON: %v", err)
	}
	return nil
}

func loadV1(data []byte) (*LoadResult, error) {
	var dto dtoV1
	if err := unmarshalDTO(data, &dto); err != nil {
		return nil, err
	}
	traces := []FieldTrace{}
	conversions := []TruncationRecord{}

	id, err := requiredValue(dto.ProjID, "proj_id")
	if err != nil {
		return nil, err
	}
	seed, err := requiredValue(dto.SeedNo, "seed_no")
	if err != nil {
		return nil, err
	}
	width, err := requiredValue(dto.CanvasW, "canvas_w")
	if err != nil {
		return nil, err
	}
	height, err := requiredValue(dto.CanvasH, "canvas_h")
	if err != nil {
		return nil, err
	}
	candidateDTO, err := requiredValue(dto.CandidateMask, "candidate_mask")
	if err != nil {
		return nil, err
	}
	referenceDTO, err := requiredValue(dto.ReferenceMask, "reference_mask")
	if err != nil {
		return nil, err
	}
	candidate, err := decodeGray("candidate_mask", candidateDTO.W, candidateDTO.H, candidateDTO.PixB64)
	if err != nil {
		return nil, err
	}
	reference, err := decodeGray("reference_mask", referenceDTO.W, referenceDTO.H, referenceDTO.PixB64)
	if err != nil {
		return nil, err
	}
	icon, err := decodePNG("icon_png_b64", dto.IconPNGB64)
	if err != nil {
		return nil, err
	}

	strokes := make([]Stroke, 0, len(dto.StrokeRecords))
	for i, raw := range dto.StrokeRecords {
		path := fmt.Sprintf("stroke_records[%d]", i)
		seq, err := requiredValue(raw.No, path+".stroke_no")
		if err != nil {
			return nil, err
		}
		priority, err := requiredValue(raw.OrderPriority, path+".order_priority")
		if err != nil {
			return nil, err
		}
		x0, err := requiredValue(raw.X0, path+".x0")
		if err != nil {
			return nil, err
		}
		y0, err := requiredValue(raw.Y0, path+".y0")
		if err != nil {
			return nil, err
		}
		x1, err := requiredValue(raw.X1, path+".x1")
		if err != nil {
			return nil, err
		}
		y1, err := requiredValue(raw.Y1, path+".y1")
		if err != nil {
			return nil, err
		}
		widthPx, err := requiredValue(raw.BrushWidthPx, path+".brush_width_px")
		if err != nil {
			return nil, err
		}
		weightPermille, err := requiredValue(raw.WeightPermille, path+".cover_weight_permille")
		if err != nil {
			return nil, err
		}
		if weightPermille < 0 || weightPermille > WeightScalePermille {
			return nil, paraError(path+".cover_weight_permille", 0, WeightScalePermille,
				"v1 weight %d outside declared permille interval", weightPermille)
		}
		weight, err := ScalePermilleToPermyriad(weightPermille, path+".cover_weight_permille", &conversions)
		if err != nil {
			return nil, err
		}
		if raw.WeightRemainder != nil {
			rem := *raw.WeightRemainder
			if rem < 0 || rem > 9 {
				return nil, paraError(path+".weight_remainder_bp", 0, 9, "remainder must be one base point digit")
			}
			weight += rem
		}
		strokeID := raw.StrokeID
		if strokeID == "" {
			strokeID = fmt.Sprintf("stroke-no-%d", seq)
		}
		hardness := DefaultHardnessPermyriad
		addDefaultTrace(&traces, SchemaV1, path+".hardness", "hardness", "HardnessPermyriad",
			DefaultHardnessPermyriad, "v1 has no brush hardness field")
		direction := DefaultDirection
		addDefaultTrace(&traces, SchemaV1, path+".direction_code", "direction_code", "Direction",
			string(DefaultDirection), "v1 has no direction category encoding")
		addFieldTrace(&traces, SchemaV1, path+".stroke_no", "stroke_no", "Seq", "rename", raw.No, seq)
		addFieldTrace(&traces, SchemaV1, path+".order_priority", "order_priority", "Priority", "rename", raw.OrderPriority, priority)
		addFieldTrace(&traces, SchemaV1, path+".x0", "x0", "Start.X", "absolute_pixel_coordinate", raw.X0, x0)
		addFieldTrace(&traces, SchemaV1, path+".y0", "y0", "Start.Y", "absolute_pixel_coordinate", raw.Y0, y0)
		addFieldTrace(&traces, SchemaV1, path+".x1", "x1", "End.X", "absolute_pixel_coordinate", raw.X1, x1)
		addFieldTrace(&traces, SchemaV1, path+".y1", "y1", "End.Y", "absolute_pixel_coordinate", raw.Y1, y1)
		addFieldTrace(&traces, SchemaV1, path+".brush_width_px", "brush_width_px", "WidthPx", "rename", raw.BrushWidthPx, widthPx)
		addFieldTrace(&traces, SchemaV1, path+".cover_weight_permille", "cover_weight_permille", "WeightPermyriad", "mul10_integer_scale", weightPermille, weight)
		strokes = append(strokes, Stroke{
			ID: strokeID, Seq: seq, Group: "ungrouped-v1", Priority: priority,
			Start: Point{X: x0, Y: y0}, End: Point{X: x1, Y: y1},
			WidthPx: widthPx, HardnessPermyriad: hardness, WeightPermyriad: int(weight),
			Direction: direction, DirectionCode: 0, HasHardness: false, HasDirection: false,
		})
	}

	truth, err := loadTruthV1(dto.TruthItems)
	if err != nil {
		return nil, err
	}
	if dto.Recovery != nil {
		strokeRecovery := map[string]recoveryStrokeV1{}
		for _, rec := range dto.Recovery.Strokes {
			strokeRecovery[rec.StrokeID] = rec
		}
		for i := range strokes {
			rec, ok := strokeRecovery[strokes[i].ID]
			if !ok {
				continue
			}
			strokes[i].Group = rec.Group
			strokes[i].HardnessPermyriad = rec.HardnessPermyriad
			strokes[i].Direction = rec.Direction
			strokes[i].DirectionCode = rec.DirectionCode
			strokes[i].HasHardness = rec.HasHardness
			strokes[i].HasDirection = rec.HasDirection
			traces = append(traces, FieldTrace{
				SourceVersion: SchemaV1, Path: fmt.Sprintf("stroke_records[%d].x_maskqa_recovery", i),
				SourceField: "x_maskqa_recovery", TargetField: "Group/Hardness/Direction",
				Rule: "vendor_extension_equivalent_restore", NewValue: rec,
			})
		}
		truthRecovery := map[int]recoveryTruthV1{}
		for _, rec := range dto.Recovery.Truth {
			truthRecovery[rec.DomainNo] = rec
		}
		for i := range truth {
			if rec, ok := truthRecovery[truth[i].DomainNo]; ok {
				truth[i].Direction = rec.Direction
				truth[i].DirectionCode = rec.DirectionCode
			}
		}
	}
	tileSize := dto.TileSz
	if tileSize == 0 {
		tileSize = DefaultTileSize
	}
	project := &Project{
		ID: id, Seed: seed, Width: width, Height: height, TileSize: tileSize,
		Icon: icon, Candidate: candidate, Reference: reference, Strokes: strokes,
		Truth: truth, Tolerances: defaultTolerancesIfNil(dto.Tolerances),
		SourceVersion: SchemaV1, Traces: traces, Conversions: conversions,
	}
	return &LoadResult{Project: project, Traces: traces, Conversions: conversions}, nil
}

func loadTruthV1(items []truthV1) ([]DomainTruth, error) {
	out := make([]DomainTruth, 0, len(items))
	for i, raw := range items {
		path := fmt.Sprintf("truth_items[%d]", i)
		no, err := requiredValue(raw.DomainIdx, path+".domain_idx")
		if err != nil {
			return nil, err
		}
		cx, err := requiredValue(raw.CXMilli, path+".cx_milli")
		if err != nil {
			return nil, err
		}
		cy, err := requiredValue(raw.CYMilli, path+".cy_milli")
		if err != nil {
			return nil, err
		}
		area, err := requiredValue(raw.PixelArea, path+".pixel_area")
		if err != nil {
			return nil, err
		}
		x0, err := requiredValue(raw.BoxX0, path+".box_x0")
		if err != nil {
			return nil, err
		}
		y0, err := requiredValue(raw.BoxY0, path+".box_y0")
		if err != nil {
			return nil, err
		}
		x1, err := requiredValue(raw.BoxX1, path+".box_x1")
		if err != nil {
			return nil, err
		}
		y1, err := requiredValue(raw.BoxY1, path+".box_y1")
		if err != nil {
			return nil, err
		}
		holes, err := requiredValue(raw.HoleCnt, path+".hole_cnt")
		if err != nil {
			return nil, err
		}
		band, err := requiredValue(raw.RingWidthMilli, path+".ring_width_milli")
		if err != nil {
			return nil, err
		}
		out = append(out, DomainTruth{
			DomainNo: no, CentroidXMilli: cx, CentroidYMilli: cy, Area: area,
			MinX: x0, MinY: y0, MaxX: x1, MaxY: y1, Holes: holes,
			BandWidthMilli: band, Direction: DirUnknown, DirectionCode: 0,
			StrokeIDs: append([]string(nil), raw.StrokeRefs...),
		})
	}
	return out, nil
}

func loadV2(data []byte) (*LoadResult, error) {
	var dto dtoV2
	if err := unmarshalDTO(data, &dto); err != nil {
		return nil, err
	}
	traces := []FieldTrace{}
	conversions := []TruncationRecord{}
	id, err := requiredValue(dto.ProjectID, "projectId")
	if err != nil {
		return nil, err
	}
	seed, err := requiredValue(dto.Seed, "seed")
	if err != nil {
		return nil, err
	}
	width, err := requiredValue(dto.CanvasWidth, "canvasWidth")
	if err != nil {
		return nil, err
	}
	height, err := requiredValue(dto.CanvasHeight, "canvasHeight")
	if err != nil {
		return nil, err
	}
	candidateDTO, err := requiredValue(dto.Candidate, "candidateMask")
	if err != nil {
		return nil, err
	}
	referenceDTO, err := requiredValue(dto.Reference, "referenceMask")
	if err != nil {
		return nil, err
	}
	candidate, err := decodeGray("candidateMask", candidateDTO.Width, candidateDTO.Height, candidateDTO.PixelsBase64)
	if err != nil {
		return nil, err
	}
	reference, err := decodeGray("referenceMask", referenceDTO.Width, referenceDTO.Height, referenceDTO.PixelsBase64)
	if err != nil {
		return nil, err
	}
	icon, err := decodePNG("iconPngBase64", dto.IconPNGBase64)
	if err != nil {
		return nil, err
	}

	strokes := make([]Stroke, 0, len(dto.Strokes))
	for i, raw := range dto.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		seq, err := requiredValue(raw.Sequence, path+".sequence")
		if err != nil {
			return nil, err
		}
		priority, err := requiredValue(raw.CompositionPriority, path+".compositionPriority")
		if err != nil {
			return nil, err
		}
		sox, err := requiredValue(raw.StartOffsetX, path+".startOffsetX")
		if err != nil {
			return nil, err
		}
		soy, err := requiredValue(raw.StartOffsetY, path+".startOffsetY")
		if err != nil {
			return nil, err
		}
		eox, err := requiredValue(raw.EndOffsetX, path+".endOffsetX")
		if err != nil {
			return nil, err
		}
		eoy, err := requiredValue(raw.EndOffsetY, path+".endOffsetY")
		if err != nil {
			return nil, err
		}
		widthPx, err := requiredValue(raw.WidthPx, path+".widthPx")
		if err != nil {
			return nil, err
		}
		weight64, err := requiredValue(raw.WeightPermyriad, path+".weightPermyriad")
		if err != nil {
			return nil, err
		}
		if weight64 < 0 || weight64 > WeightScalePermyriad {
			return nil, paraError(path+".weightPermyriad", 0, WeightScalePermyriad,
				"v2 weight %d outside declared permyriad interval", weight64)
		}
		originX, err := SequenceOrigin(seq, 0, width)
		if err != nil {
			return nil, err
		}
		originY, err := SequenceOrigin(seq, 1, height)
		if err != nil {
			return nil, err
		}
		sx64, err := AbsoluteFromOffset(sox, int64(originX), path+".startOffsetX")
		if err != nil {
			return nil, err
		}
		sy64, err := AbsoluteFromOffset(soy, int64(originY), path+".startOffsetY")
		if err != nil {
			return nil, err
		}
		ex64, err := AbsoluteFromOffset(eox, int64(originX), path+".endOffsetX")
		if err != nil {
			return nil, err
		}
		ey64, err := AbsoluteFromOffset(eoy, int64(originY), path+".endOffsetY")
		if err != nil {
			return nil, err
		}
		hardness := DefaultHardnessPermyriad
		hasHardness := false
		if raw.HardnessPermyriad != nil {
			hardness = *raw.HardnessPermyriad
			hasHardness = true
		} else {
			addDefaultTrace(&traces, SchemaV2, path+".hardnessPermyriad", "hardnessPermyriad",
				"HardnessPermyriad", DefaultHardnessPermyriad, "missing hardness uses declared brush default")
		}
		direction := DefaultDirection
		hasDirection := false
		code := 0
		if raw.DirectionCode != nil {
			code = *raw.DirectionCode
			direction = CodeToDirection(code)
			hasDirection = direction != DirUnknown
		} else {
			addDefaultTrace(&traces, SchemaV2, path+".directionCode", "directionCode", "Direction",
				string(DefaultDirection), "missing direction code uses declared default")
		}
		group := raw.GroupName
		if group == "" {
			group = "ungrouped-v2"
		}
		v2Path := path
		addFieldTrace(&traces, SchemaV2, v2Path+".sequence", "sequence", "Seq", "rename", raw.Sequence, seq)
		addFieldTrace(&traces, SchemaV2, v2Path+".startOffsetX", "startOffsetX", "Start.X", "sequence_origin_offset_to_absolute", sox, int(sx64))
		addFieldTrace(&traces, SchemaV2, v2Path+".startOffsetY", "startOffsetY", "Start.Y", "sequence_origin_offset_to_absolute", soy, int(sy64))
		addFieldTrace(&traces, SchemaV2, v2Path+".endOffsetX", "endOffsetX", "End.X", "sequence_origin_offset_to_absolute", eox, int(ex64))
		addFieldTrace(&traces, SchemaV2, v2Path+".endOffsetY", "endOffsetY", "End.Y", "sequence_origin_offset_to_absolute", eoy, int(ey64))
		addFieldTrace(&traces, SchemaV2, v2Path+".weightPermyriad", "weightPermyriad", "WeightPermyriad", "same_integer_scale", raw.WeightPermyriad, int(weight64))
		strokes = append(strokes, Stroke{
			ID: raw.StrokeID, Seq: seq, Group: group, Priority: priority,
			Start: Point{X: int(sx64), Y: int(sy64)}, End: Point{X: int(ex64), Y: int(ey64)},
			WidthPx: widthPx, HardnessPermyriad: hardness, WeightPermyriad: int(weight64),
			Direction: direction, DirectionCode: code, HasHardness: hasHardness, HasDirection: hasDirection,
		})
	}

	truth := make([]DomainTruth, 0, len(dto.TruthDomains))
	for i, raw := range dto.TruthDomains {
		path := fmt.Sprintf("truthDomains[%d]", i)
		no, err := requiredValue(raw.DomainNo, path+".domainNo")
		if err != nil {
			return nil, err
		}
		cx, err := requiredValue(raw.CentroidXMilli, path+".centroidXMilli")
		if err != nil {
			return nil, err
		}
		cy, err := requiredValue(raw.CentroidYMilli, path+".centroidYMilli")
		if err != nil {
			return nil, err
		}
		area, err := requiredValue(raw.Area, path+".area")
		if err != nil {
			return nil, err
		}
		minX, err := requiredValue(raw.MinX, path+".minX")
		if err != nil {
			return nil, err
		}
		minY, err := requiredValue(raw.MinY, path+".minY")
		if err != nil {
			return nil, err
		}
		maxX, err := requiredValue(raw.MaxX, path+".maxX")
		if err != nil {
			return nil, err
		}
		maxY, err := requiredValue(raw.MaxY, path+".maxY")
		if err != nil {
			return nil, err
		}
		holes, err := requiredValue(raw.Holes, path+".holes")
		if err != nil {
			return nil, err
		}
		band, err := requiredValue(raw.BandWidthMilli, path+".bandWidthMilli")
		if err != nil {
			return nil, err
		}
		code := 0
		direction := DirUnknown
		if raw.DirectionCode != nil {
			code = *raw.DirectionCode
			direction = CodeToDirection(code)
		} else {
			addDefaultTrace(&traces, SchemaV2, path+".directionCode", "directionCode", "Truth.Direction",
				string(DefaultDirection), "missing truth direction code uses declared default")
		}
		truth = append(truth, DomainTruth{
			DomainNo: no, CentroidXMilli: cx, CentroidYMilli: cy, Area: area,
			MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY, Holes: holes,
			BandWidthMilli: band, Direction: direction, DirectionCode: code,
			StrokeIDs: append([]string(nil), raw.StrokeIDs...),
		})
	}

	tileSize := dto.TileSize
	if tileSize == 0 {
		tileSize = DefaultTileSize
	}
	project := &Project{
		ID: id, Seed: seed, Width: width, Height: height, TileSize: tileSize,
		Icon: icon, Candidate: candidate, Reference: reference, Strokes: strokes,
		Truth: truth, Tolerances: defaultTolerancesIfNil(dto.Tolerances),
		SourceVersion: SchemaV2, Traces: traces, Conversions: conversions,
	}
	return &LoadResult{Project: project, Traces: traces, Conversions: conversions}, nil
}

func loadV3(data []byte) (*LoadResult, error) {
	var dto dtoV3
	if err := unmarshalDTO(data, &dto); err != nil {
		return nil, err
	}
	traces := []FieldTrace{}
	id, err := requiredValue(dto.ID, "id")
	if err != nil {
		return nil, err
	}
	seed, err := requiredValue(dto.Seed, "seed")
	if err != nil {
		return nil, err
	}
	width, err := requiredValue(dto.Width, "width")
	if err != nil {
		return nil, err
	}
	height, err := requiredValue(dto.Height, "height")
	if err != nil {
		return nil, err
	}
	candidateDTO, err := requiredValue(dto.Candidate, "candidate_mask")
	if err != nil {
		return nil, err
	}
	referenceDTO, err := requiredValue(dto.Reference, "reference_mask")
	if err != nil {
		return nil, err
	}
	candidate, err := decodeGray("candidate_mask", candidateDTO.WidthPx, candidateDTO.HeightPx, candidateDTO.Gray8Base64)
	if err != nil {
		return nil, err
	}
	reference, err := decodeGray("reference_mask", referenceDTO.WidthPx, referenceDTO.HeightPx, referenceDTO.Gray8Base64)
	if err != nil {
		return nil, err
	}
	icon, err := decodePNG("icon_png_base64", dto.IconPNGB64)
	if err != nil {
		return nil, err
	}
	strokes := make([]Stroke, 0, len(dto.Strokes))
	for i, raw := range dto.Strokes {
		path := fmt.Sprintf("strokes[%d]", i)
		seq, err := requiredValue(raw.Seq, path+".seq")
		if err != nil {
			return nil, err
		}
		priority, err := requiredValue(raw.Priority, path+".priority")
		if err != nil {
			return nil, err
		}
		start, err := requiredValue(raw.Start, path+".start")
		if err != nil {
			return nil, err
		}
		end, err := requiredValue(raw.End, path+".end")
		if err != nil {
			return nil, err
		}
		widthPx, err := requiredValue(raw.WidthPx, path+".width_px")
		if err != nil {
			return nil, err
		}
		hardness := DefaultHardnessPermyriad
		hasHardness := false
		if raw.HardnessPermyriad != nil {
			hardness = *raw.HardnessPermyriad
			hasHardness = true
		} else {
			addDefaultTrace(&traces, SchemaV3, path+".hardness_permyriad", "hardness_permyriad",
				"HardnessPermyriad", DefaultHardnessPermyriad, "missing hardness uses declared default")
		}
		weight, err := requiredValue(raw.WeightPermyriad, path+".weight_permyriad")
		if err != nil {
			return nil, err
		}
		direction := raw.Direction
		hasDirection := direction != ""
		code := 0
		if raw.DirectionCode != nil {
			code = *raw.DirectionCode
		}
		if direction == "" {
			if code != 0 {
				direction = CodeToDirection(code)
				hasDirection = true
			} else {
				direction = DefaultDirection
				addDefaultTrace(&traces, SchemaV3, path+".direction", "direction", "Direction",
					string(DefaultDirection), "missing direction uses declared default")
			}
		}
		if code == 0 {
			code = DirectionToCode(direction)
		}
		strokes = append(strokes, Stroke{
			ID: raw.ID, Seq: seq, Group: raw.Group, Priority: priority,
			Start: start, End: end, WidthPx: widthPx,
			HardnessPermyriad: hardness, WeightPermyriad: weight,
			Direction: direction, DirectionCode: code,
			HasHardness: hasHardness, HasDirection: hasDirection,
		})
	}
	tileSize := dto.TileSize
	if tileSize == 0 {
		tileSize = DefaultTileSize
	}
	project := &Project{
		ID: id, Seed: seed, Width: width, Height: height, TileSize: tileSize,
		Icon: icon, Candidate: candidate, Reference: reference,
		Strokes: strokes, Truth: dto.Truth,
		Tolerances:    defaultTolerancesIfNil(dto.Tolerances),
		SourceVersion: SchemaV3, Traces: traces,
	}
	return &LoadResult{Project: project, Traces: traces}, nil
}

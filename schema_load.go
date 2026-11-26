package maskqc

import "fmt"

func loadV1(doc map[string]any, raw []byte) (*Project, error) {
	p := baseProject(doc, SchemaV1, raw)
	images, err := reqArray(doc, "$", "images")
	if err != nil {
		return nil, err
	}
	for ii, item := range images {
		path := fmt.Sprintf("images[%d]", ii)
		im, err := mustMap(item, path)
		if err != nil {
			return nil, err
		}
		icon, err := loadIconCommon(im, path, "image_id", "tile_w", "tile_h", "candidate_mask_hex", "truth_mask_hex", "truth_blocks")
		if err != nil {
			return nil, err
		}
		rows, err := reqArray(im, path, "strokes")
		if err != nil {
			return nil, err
		}
		for si, row := range rows {
			sp := fmt.Sprintf("%s.strokes[%d]", path, si)
			sm, err := mustMap(row, sp)
			if err != nil {
				return nil, err
			}
			id, err := reqString(sm, sp, "stroke_id")
			if err != nil {
				return nil, err
			}
			seq, err := reqInt(sm, sp, "pen_no")
			if err != nil {
				return nil, err
			}
			priority, err := reqInt(sm, sp, "proc_priority")
			if err != nil {
				return nil, err
			}
			x1, err := reqInt(sm, sp, "x1")
			if err != nil {
				return nil, err
			}
			y1, err := reqInt(sm, sp, "y1")
			if err != nil {
				return nil, err
			}
			x2, err := reqInt(sm, sp, "x2")
			if err != nil {
				return nil, err
			}
			y2, err := reqInt(sm, sp, "y2")
			if err != nil {
				return nil, err
			}
			radius, err := reqInt(sm, sp, "brush_radius")
			if err != nil {
				return nil, err
			}
			permille, err := reqInt(sm, sp, "opacity_permille")
			if err != nil {
				return nil, err
			}
			weight, wtr, err := ScalePermilleToPermyriad(permille, sp+".opacity_permille")
			if err != nil {
				return nil, err
			}
			if rem, ok := optInt(sm, "weight_residual_bp", 0); ok && rem != 0 {
				weight += rem
				wtr.NewValue = fmt.Sprint(weight)
				wtr.Residual = int64(rem)
				wtr.Operation += "+stored_residual"
			}
			dir := DefaultDirection
			if code, ok := optInt(sm, "dir_code", 0); ok {
				mapped, exists := v1CodeToDir[code]
				if !exists {
					return nil, newError(ESchema, sp+".dir_code", "unknown direction code %d", code)
				}
				dir = mapped
			} else {
				p.ConvertTrace = append(p.ConvertTrace, defaultTrace(icon.ID, id, sp+".dir_code", "", string(DefaultDirection), "declared-default:v1-direction"))
			}
			p.ConvertTrace = append(p.ConvertTrace, wtr,
				defaultTrace(icon.ID, id, sp+".hardness", nil, DefaultHardnessPermille, "declared-default:v1-no-hardness"),
				defaultTrace(icon.ID, id, sp+".composition_order", nil, DefaultCompositionOrder, "declared-default:v1-no-composition-order"),
			)
			stroke := &Stroke{
				ID: id, Seq: seq, Priority: priority, Start: Point{x1, y1}, End: Point{x2, y2},
				Radius: radius, HardnessPermille: DefaultHardnessPermille, WeightPermyriad: weight,
				DeclaredDirection: dir, CompositionOrder: DefaultCompositionOrder, SourceSchema: SchemaV1,
			}
			icon.Strokes = append(icon.Strokes, stroke)
			p.traceStrokeMapping(icon.ID, sp, stroke, map[string]string{
				"id": "stroke_id", "seq": "pen_no", "priority": "proc_priority",
				"start.x": "x1", "start.y": "y1", "end.x": "x2", "end.y": "y2",
				"radius": "brush_radius", "hardness_permille": "<absent:declared-default>",
				"weight_permyriad": "opacity_permille*10(+residual)", "declared_direction": "dir_code",
				"composition_order": "<absent:declared-default>", "group": "<absent:lossy-on-export>",
			})
		}
		p.Icons = append(p.Icons, icon)
	}
	return p, nil
}

func loadV2(doc map[string]any, raw []byte) (*Project, error) {
	p := baseProject(doc, SchemaV2, raw)
	icons, err := reqArray(doc, "$", "icons")
	if err != nil {
		return nil, err
	}
	for ii, item := range icons {
		path := fmt.Sprintf("icons[%d]", ii)
		im, err := mustMap(item, path)
		if err != nil {
			return nil, err
		}
		icon, err := loadIconCommon(im, path, "iconId", "tileWidth", "tileHeight", "candidateMaskHex", "truthMaskHex", "truthTiles")
		if err != nil {
			return nil, err
		}
		rows, err := reqArray(im, path, "strokes")
		if err != nil {
			return nil, err
		}
		for si, row := range rows {
			sp := fmt.Sprintf("%s.strokes[%d]", path, si)
			sm, err := mustMap(row, sp)
			if err != nil {
				return nil, err
			}
			id, err := reqString(sm, sp, "strokeId")
			if err != nil {
				return nil, err
			}
			seq, err := reqInt(sm, sp, "penSequence")
			if err != nil {
				return nil, err
			}
			priority, err := reqInt(sm, sp, "phasePriority")
			if err != nil {
				return nil, err
			}
			stride, _ := optInt(sm, "coordStride", SequenceStridePixels)
			sdx, err := reqInt(sm, sp, "startDX")
			if err != nil {
				return nil, err
			}
			sdy, err := reqInt(sm, sp, "startDY")
			if err != nil {
				return nil, err
			}
			edx, err := reqInt(sm, sp, "endDX")
			if err != nil {
				return nil, err
			}
			edy, err := reqInt(sm, sp, "endDY")
			if err != nil {
				return nil, err
			}
			sx, tr1, err := SequenceOffsetToAbsolute(sdx, seq, stride, sp+".startDX")
			if err != nil {
				return nil, err
			}
			sy, tr2, err := SequenceOffsetToAbsolute(sdy, seq, stride, sp+".startDY")
			if err != nil {
				return nil, err
			}
			ex, tr3, err := SequenceOffsetToAbsolute(edx, seq, stride, sp+".endDX")
			if err != nil {
				return nil, err
			}
			ey, tr4, err := SequenceOffsetToAbsolute(edy, seq, stride, sp+".endDY")
			if err != nil {
				return nil, err
			}
			radius, err := reqInt(sm, sp, "brushRadius")
			if err != nil {
				return nil, err
			}
			hardnessPct, hardnessPresent := optInt(sm, "hardnessPct", DefaultHardnessPermille/10)
			hardness := hardnessPct * 10
			if !hardnessPresent {
				hardness = DefaultHardnessPermille
				p.ConvertTrace = append(p.ConvertTrace, defaultTrace(icon.ID, id, sp+".hardnessPct", nil, hardness, "declared-default:v2-missing-hardness"))
			}
			cov, err := reqMap(sm, sp, "coverage")
			if err != nil {
				return nil, err
			}
			permille, err := reqInt(cov, sp+".coverage", "permille")
			if err != nil {
				return nil, err
			}
			weight, wtr, err := ScalePermilleToPermyriad(permille, sp+".coverage.permille")
			if err != nil {
				return nil, err
			}
			if residual, _ := optInt(cov, "weightResidualBP", 0); residual != 0 {
				weight += residual
				wtr.Residual = int64(residual)
				wtr.Operation += "+stored_residual"
			}
			dir := DefaultDirection
			if dirName, ok := sm["direction"].(string); ok && dirName != "" {
				mapped, exists := v2ToDir[dirName]
				if !exists {
					return nil, newError(ESchema, sp+".direction", "unknown direction name %q", dirName)
				}
				dir = mapped
			} else {
				p.ConvertTrace = append(p.ConvertTrace, defaultTrace(icon.ID, id, sp+".direction", nil, string(DefaultDirection), "declared-default:missing-direction"))
			}
			order, orderPresent := optInt(sm, "orderHint", DefaultCompositionOrder)
			if !orderPresent {
				p.ConvertTrace = append(p.ConvertTrace, defaultTrace(icon.ID, id, sp+".orderHint", nil, DefaultCompositionOrder, "declared-default:missing-composition-order"))
			}
			group, _ := sm["groupName"].(string)
			p.ConvertTrace = append(p.ConvertTrace, tr1, tr2, tr3, tr4, wtr)
			stroke := &Stroke{
				ID: id, Seq: seq, Priority: priority, Group: group,
				Start: Point{sx, sy}, End: Point{ex, ey}, Radius: radius,
				HardnessPermille: hardness, WeightPermyriad: weight,
				DeclaredDirection: dir, CompositionOrder: order, SourceSchema: SchemaV2,
			}
			icon.Strokes = append(icon.Strokes, stroke)
			p.traceStrokeMapping(icon.ID, sp, stroke, map[string]string{
				"id": "strokeId", "seq": "penSequence", "priority": "phasePriority", "group": "groupName",
				"start.x": "startDX+penSequence*coordStride", "start.y": "startDY+penSequence*coordStride",
				"end.x": "endDX+penSequence*coordStride", "end.y": "endDY+penSequence*coordStride",
				"radius": "brushRadius", "hardness_permille": "hardnessPct*10",
				"weight_permyriad":   "coverage.permille*10(+weightResidualBP)",
				"declared_direction": "direction", "composition_order": "orderHint",
			})
		}
		p.Icons = append(p.Icons, icon)
	}
	return p, nil
}

func loadV3(doc map[string]any, raw []byte) (*Project, error) {
	p := baseProject(doc, SchemaV3, raw)
	icons, err := reqArray(doc, "$", "icons")
	if err != nil {
		return nil, err
	}
	for ii, item := range icons {
		path := fmt.Sprintf("icons[%d]", ii)
		im, err := mustMap(item, path)
		if err != nil {
			return nil, err
		}
		icon, err := loadIconCommon(im, path, "id", "tile_width", "tile_height", "candidate_mask_hex", "truth_mask_hex", "truth_tiles")
		if err != nil {
			return nil, err
		}
		rows, err := reqArray(im, path, "strokes")
		if err != nil {
			return nil, err
		}
		for si, row := range rows {
			sp := fmt.Sprintf("%s.strokes[%d]", path, si)
			sm, err := mustMap(row, sp)
			if err != nil {
				return nil, err
			}
			id, err := reqString(sm, sp, "id")
			if err != nil {
				return nil, err
			}
			seq, err := reqInt(sm, sp, "seq")
			if err != nil {
				return nil, err
			}
			priority, err := reqInt(sm, sp, "process_priority")
			if err != nil {
				return nil, err
			}
			startM, err := reqMap(sm, sp, "start")
			if err != nil {
				return nil, err
			}
			endM, err := reqMap(sm, sp, "end")
			if err != nil {
				return nil, err
			}
			sx, err := reqInt(startM, sp+".start", "x")
			if err != nil {
				return nil, err
			}
			sy, err := reqInt(startM, sp+".start", "y")
			if err != nil {
				return nil, err
			}
			ex, err := reqInt(endM, sp+".end", "x")
			if err != nil {
				return nil, err
			}
			ey, err := reqInt(endM, sp+".end", "y")
			if err != nil {
				return nil, err
			}
			brush, err := reqMap(sm, sp, "brush")
			if err != nil {
				return nil, err
			}
			radius, err := reqInt(brush, sp+".brush", "radius")
			if err != nil {
				return nil, err
			}
			hardness, err := reqInt(brush, sp+".brush", "hardness_permille")
			if err != nil {
				return nil, err
			}
			cov, err := reqMap(sm, sp, "coverage")
			if err != nil {
				return nil, err
			}
			weight, err := reqInt(cov, sp+".coverage", "weight_permyriad")
			if err != nil {
				return nil, err
			}
			dir := DefaultDirection
			if dirM, ok := sm["direction"].(map[string]any); ok {
				if className, _ := dirM["class"].(string); className != "" {
					mapped := DirectionClass(className)
					if mapped != DirAlong && mapped != DirCross && mapped != DirMixed && mapped != DirUnknown {
						return nil, newError(ESchema, sp+".direction.class", "unknown direction class %q", className)
					}
					dir = mapped
				}
			}
			order, _ := optInt(sm, "composition_order", DefaultCompositionOrder)
			group, _ := sm["group_id"].(string)
			stroke := &Stroke{
				ID: id, Seq: seq, Priority: priority, Group: group,
				Start: Point{sx, sy}, End: Point{ex, ey}, Radius: radius,
				HardnessPermille: hardness, WeightPermyriad: weight,
				DeclaredDirection: dir, CompositionOrder: order, SourceSchema: SchemaV3,
			}
			icon.Strokes = append(icon.Strokes, stroke)
			p.traceStrokeMapping(icon.ID, sp, stroke, map[string]string{
				"id": "id", "seq": "seq", "priority": "process_priority", "group": "group_id",
				"start.x": "start.x", "start.y": "start.y", "end.x": "end.x", "end.y": "end.y",
				"radius": "brush.radius", "hardness_permille": "brush.hardness_permille",
				"weight_permyriad": "coverage.weight_permyriad", "declared_direction": "direction.class",
				"composition_order": "composition_order",
			})
		}
		p.Icons = append(p.Icons, icon)
	}
	return p, nil
}

func reqMap(m map[string]any, path, key string) (map[string]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, newError(ESchema, path+"."+key, "missing required field")
	}
	return mustMap(v, path+"."+key)
}

func loadIconCommon(m map[string]any, path, idKey, twKey, thKey, candKey, truthKey, tilesKey string) (*Icon, error) {
	id, err := reqString(m, path, idKey)
	if err != nil {
		return nil, err
	}
	w, err := reqInt(m, path, "width")
	if err != nil {
		return nil, err
	}
	h, err := reqInt(m, path, "height")
	if err != nil {
		return nil, err
	}
	tw, err := reqInt(m, path, twKey)
	if err != nil {
		return nil, err
	}
	th, err := reqInt(m, path, thKey)
	if err != nil {
		return nil, err
	}
	candHex, err := reqString(m, path, candKey)
	if err != nil {
		return nil, err
	}
	truthHex, err := reqString(m, path, truthKey)
	if err != nil {
		return nil, err
	}
	cand, err := decodeMaskHex(candHex, w, h, path+"."+candKey)
	if err != nil {
		return nil, err
	}
	truth, err := decodeMaskHex(truthHex, w, h, path+"."+truthKey)
	if err != nil {
		return nil, err
	}
	icon := &Icon{ID: id, Width: w, Height: h, TileWidth: tw, TileHeight: th, Candidate: cand, Truth: truth}
	if rawTiles, ok := m[tilesKey]; ok {
		if err := loadTruthTiles(icon, rawTiles, path+"."+tilesKey); err != nil {
			return nil, err
		}
	} else {
		return nil, newError(ESchema, path+"."+tilesKey, "missing required truth tile manifest")
	}
	for i, tt := range icon.TruthTiles {
		tile, err := CropMask(truth, tt.Rect)
		if err != nil {
			return nil, newError(ESchema, fmt.Sprintf("%s.%s[%d].rect", path, tilesKey, i), "%v", err)
		}
		if got := tile.Hash(); got != tt.Hash {
			return nil, newError(ESchema, fmt.Sprintf("%s.%s[%d].sha256", path, tilesKey, i), "truth tile hash mismatch: expected %s, computed %s", tt.Hash, got)
		}
	}
	return icon, nil
}

func defaultTrace(iconID, strokeID, path string, old any, new any, source string) FieldTrace {
	return FieldTrace{
		IconID: iconID, StrokeID: strokeID, FieldPath: path,
		Operation: "fill_declared_default", OldValue: fmt.Sprint(old), NewValue: fmt.Sprint(new), Source: source,
	}
}

func (p *Project) traceStrokeMapping(iconID, path string, s *Stroke, sourceKeys map[string]string) {
	fields := []struct {
		name  string
		value any
	}{
		{"id", s.ID}, {"seq", s.Seq}, {"priority", s.Priority}, {"group", s.Group},
		{"start.x", s.Start.X}, {"start.y", s.Start.Y}, {"end.x", s.End.X}, {"end.y", s.End.Y},
		{"radius", s.Radius}, {"hardness_permille", s.HardnessPermille},
		{"weight_permyriad", s.WeightPermyriad}, {"declared_direction", s.DeclaredDirection},
		{"composition_order", s.CompositionOrder},
	}
	for _, field := range fields {
		sourcePath := sourceKeys[field.name]
		if sourcePath == "" {
			sourcePath = "<same-or-derived>"
		}
		p.ConvertTrace = append(p.ConvertTrace, FieldTrace{
			IconID: iconID, StrokeID: s.ID, FieldPath: path + "." + field.name,
			Operation: "schema_field_mapping", OldValue: sourcePath,
			NewValue: fmt.Sprint(field.value), Source: s.SourceSchema,
		})
	}
}

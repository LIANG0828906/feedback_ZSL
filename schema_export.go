package vpf

import "encoding/json"

// ExportProject writes a canonical project in the requested schema version.
// A non-nil LossReport still contains a readable exported document; callers
// that require strict losslessness can treat LossReport.asError as E_LOSSY.
func ExportProject(p *Project, version int) ([]byte, *LossReport, error) {
	report := &LossReport{Code: CodeLossy}
	conversions := &TraceBook{}

	var doc map[string]any
	var err error
	switch version {
	case SchemaV1:
		doc, err = exportV1(p, report, conversions)
	case SchemaV2:
		doc, err = exportV2(p, report, conversions)
	case SchemaV3:
		doc, err = exportV3(p, report, conversions)
	default:
		return nil, nil, schemaError(version, "$.version", "unknown export schema version")
	}
	if err != nil {
		return nil, nil, err
	}
	for _, truncation := range conversions.Truncations {
		report.Items = append(report.Items, LossItem{
			Code: CodeLossy, Kind: "integer_rounding", FieldPath: truncation.Field,
			Disposition:   truncation.Operation + ":" + truncation.Direction,
			PrecisionLoss: truncation.Remainder != 0,
			Detail:        "non-zero remainder was discarded",
		})
	}
	if len(report.Items) == 0 {
		report = nil
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, report, err
	}
	return data, report, nil
}

func ExportProjectStrict(p *Project, version int) ([]byte, error) {
	data, report, err := ExportProject(p, version)
	if err != nil {
		return nil, err
	}
	if e := report.asError(); e != nil {
		return data, e
	}
	return data, nil
}

func lookupLayer(p *Project, id string) Layer {
	layer, _ := p.LayerByID(id)
	return layer
}

func exportTimeList(p *Project, version int, conversions *TraceBook) ([]any, []any, error) {
	frames := make([]any, 0, len(p.Frames))
	commands := make([]any, 0, len(p.Instructions))
	for _, frame := range p.Frames {
		value, err := TimeToSchemaUnit("$.frames.time", frame.AtNS, version, conversions)
		if err != nil {
			return nil, nil, err
		}
		switch version {
		case SchemaV1:
			frames = append(frames, map[string]any{"frame_no": frame.Index, "at_tick": value, "cue": frame.Label})
		case SchemaV2:
			frames = append(frames, map[string]any{"frame_index": frame.Index, "time_ms": value, "marker": frame.Label})
		default:
			frames = append(frames, map[string]any{"frameIndex": frame.Index, "timeUs": value, "label": frame.Label})
		}
	}
	for _, in := range p.Instructions {
		value, err := TimeToSchemaUnit("$.instructions.time", in.AtNS, version, conversions)
		if err != nil {
			return nil, nil, err
		}
		switch version {
		case SchemaV1:
			commands = append(commands, map[string]any{
				"seq": in.Order, "at_tick": value, "op": in.Op, "layer": in.LayerID,
				"source": in.SourceID, "radius_px": in.Radius, "strength_bp": in.AmplitudeBP,
			})
		case SchemaV2:
			commands = append(commands, map[string]any{
				"order": in.Order, "at_ms": value, "verb": in.Op, "target_layer": in.LayerID,
				"source": in.SourceID, "radius": in.Radius, "strength_bp": in.AmplitudeBP,
			})
		default:
			commands = append(commands, map[string]any{
				"order": in.Order, "atUs": value, "operation": in.Op, "layerId": in.LayerID,
				"sourceId": in.SourceID, "radius": in.Radius, "strengthBasisPoints": in.AmplitudeBP,
			})
		}
	}
	return frames, commands, nil
}

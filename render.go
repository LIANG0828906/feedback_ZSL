package downsample

import "encoding/json"

func RenderDocument(project *Project, version SchemaVersion) ([]byte, *Error) {
	var doc any
	switch version {
	case SchemaV1:
		doc = renderV1(project)
	case SchemaV2:
		doc = renderV2(project)
	case SchemaV3:
		doc = renderV3(project)
	default:
		return nil, NewError(CodeSchemaUnsupportedVersion, "cannot render unknown schema").
			WithVersion(string(version))
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, WrapError(CodeSchemaInvalidJSON, "render failed", err)
	}
	return data, nil
}

func renderV1(p *Project) map[string]any {
	metrics := make([]any, 0, len(p.Metrics))
	for _, metric := range p.SortedMetrics() {
		funcs := make([]string, 0, len(metric.Functions))
		for _, fn := range metric.Functions {
			funcs = append(funcs, string(fn))
		}
		m := map[string]any{
			"metric_id":           metric.ID,
			"metric_order":        metric.Ordinal,
			"kind":                metric.Kind,
			"window_width_ms":     metric.WindowMS,
			"alignment_basis":     "integer_minute_epoch",
			"anchor_ms":           metric.AnchorMS,
			"weight_scale":        "permille_1000",
			"fixed_bit_width":     metric.WeightBits,
			"aggregate_functions": funcs,
		}
		if metric.Derivation != nil {
			m["derivation"] = renderV1Derivation(*metric.Derivation)
		} else {
			points := make([]any, 0)
			for _, point := range p.SortedPoints(metric.ID) {
				weight := point.Weight / 10
				if point.SourceWeightScale == int64(ScalePermille) {
					weight = point.SourceWeightRaw
				}
				points = append(points, map[string]any{
					"seq":             point.Seq,
					"timestamp_ms":    point.TimestampMS,
					"value":           point.Value,
					"weight_permille": weight,
				})
			}
			m["samples"] = points
		}
		metrics = append(metrics, m)
	}
	return map[string]any{
		"schema_version":     string(SchemaV1),
		"project_id":         p.ProjectID,
		"config_version":     p.ConfigVersion,
		"base_time_ms":       p.BaseTimeMS,
		"range_start_ms":     p.RangeStartMS,
		"range_end_ms":       p.RangeEndMS,
		"declared_tolerance": p.Tolerance,
		"metrics":            metrics,
		"function_table":     renderV1Table(),
		"degradation": map[string]any{
			"missing_fill_field":            p.Degradation.MissingFillField,
			"default_fill_lookback_windows": p.Degradation.FillLookback,
		},
	}
}

func renderV1Alignment(a SourceAlignment) string {
	if a == SourceAlignFirstPoint {
		return "first_data_point"
	}
	return "integer_minute_epoch"
}

func renderV1Derivation(d Derivation) map[string]any {
	inputs := make([]any, 0, len(d.Inputs))
	for _, in := range d.Inputs {
		inputs = append(inputs, map[string]any{"metric": in.MetricID, "function": string(in.Function), "lag": in.Lag})
	}
	return map[string]any{"op": d.Op, "factor": d.Factor, "inputs": inputs}
}

func renderV1Table() map[string]any {
	defs := []string{"count", "sum", "avg", "min", "max", "first", "last", "weighted_avg", "derived_value"}
	constants := make([]any, 0, len(defs))
	for _, def := range defs {
		constants = append(constants, map[string]any{"name": def, "op": def})
	}
	return map[string]any{"organization": "flat_constants", "constants": constants}
}

func renderV2(p *Project) map[string]any {
	metrics := make([]any, 0, len(p.Metrics))
	for _, metric := range p.SortedMetrics() {
		funcs := make([]string, 0, len(metric.Functions))
		for _, fn := range metric.Functions {
			funcs = append(funcs, string(fn))
		}
		m := map[string]any{
			"id":                   metric.ID,
			"order":                metric.Ordinal,
			"kind":                 metric.Kind,
			"windowDurationMs":     metric.WindowMS,
			"alignmentBasis":       "first_data_point",
			"anchorTimestampMs":    metric.AnchorMS,
			"weightUnit":           "permyriad_10000",
			"fixedBitWidth":        metric.WeightBits,
			"aggregationFunctions": funcs,
		}
		if metric.Fill.Enabled || metric.Fill.FieldPresent {
			m["lastValueFill"] = map[string]any{
				"enabled":            metric.Fill.Enabled,
				"strategy":           metric.Fill.Strategy,
				"maxLookbackWindows": metric.Fill.MaxLookbackWindows,
			}
		}
		if metric.Derivation != nil {
			inputs := make([]any, 0, len(metric.Derivation.Inputs))
			for _, in := range metric.Derivation.Inputs {
				inputs = append(inputs, map[string]any{"metricId": in.MetricID, "function": string(in.Function), "lag": in.Lag})
			}
			m["derivation"] = map[string]any{"op": metric.Derivation.Op, "factor": metric.Derivation.Factor, "inputs": inputs}
		} else {
			points := make([]any, 0)
			for _, point := range p.SortedPoints(metric.ID) {
				start := WindowStart(point.TimestampMS, metric.WindowMS, metric.AnchorMS)
				index := (start - metric.AnchorMS) / metric.WindowMS
				offset := point.TimestampMS - start
				points = append(points, map[string]any{
					"sequence":             point.Seq,
					"windowIndex":          index,
					"offsetWithinWindowMs": offset,
					"sampleValue":          point.Value,
					"weightRaw":            point.Weight,
				})
			}
			m["points"] = points
		}
		metrics = append(metrics, m)
	}
	return map[string]any{
		"schemaVersion":     string(SchemaV2),
		"projectId":         p.ProjectID,
		"configVersion":     p.ConfigVersion,
		"baseTimestampMs":   p.BaseTimeMS,
		"rangeStartMs":      p.RangeStartMS,
		"rangeEndMs":        p.RangeEndMS,
		"declaredTolerance": p.Tolerance,
		"metrics":           metrics,
		"functionTree":      renderV2Tree(),
		"degradation": map[string]any{
			"missingFillField":       p.Degradation.MissingFillField,
			"defaultLookbackWindows": p.Degradation.FillLookback,
		},
	}
}

func renderV2Tree() map[string]any {
	raw := []any{
		map[string]any{"functionName": "avg", "builtin": "avg"},
		map[string]any{"functionName": "sum", "builtin": "sum"},
		map[string]any{"functionName": "min", "builtin": "min"},
		map[string]any{"functionName": "max", "builtin": "max"},
		map[string]any{"functionName": "weighted_avg", "builtin": "weighted_avg"},
	}
	derived := []any{
		map[string]any{"functionName": "derived_value", "builtin": "derived_value"},
		map[string]any{"functionName": "mean_alias", "aliasRef": "avg"},
	}
	return map[string]any{
		"organization": "nested_by_metric_group",
		"groups": map[string]any{
			"raw_metric_functions":     raw,
			"derived_metric_functions": derived,
		},
	}
}

func renderV3(p *Project) map[string]any {
	metrics := make([]any, 0, len(p.Metrics))
	for _, metric := range p.SortedMetrics() {
		funcs := make([]string, 0, len(metric.Functions))
		for _, fn := range metric.Functions {
			funcs = append(funcs, string(fn))
		}
		m := map[string]any{
			"MetricID":    metric.ID,
			"Ordinal":     metric.Ordinal,
			"Kind":        metric.Kind,
			"WindowMS":    metric.WindowMS,
			"Alignment":   "epoch_grid",
			"AnchorMS":    metric.AnchorMS,
			"WeightScale": "permyriad_10000",
			"WeightBits":  metric.WeightBits,
			"Functions":   funcs,
			"FillPolicy": map[string]any{
				"Enabled":         metric.Fill.Enabled,
				"Strategy":        metric.Fill.Strategy,
				"LookbackWindows": metric.Fill.MaxLookbackWindows,
			},
		}
		if metric.Derivation != nil {
			inputs := make([]any, 0, len(metric.Derivation.Inputs))
			for _, in := range metric.Derivation.Inputs {
				inputs = append(inputs, map[string]any{"InputMetric": in.MetricID, "Function": string(in.Function), "Lag": in.Lag})
			}
			m["Derivation"] = map[string]any{"Op": metric.Derivation.Op, "Factor": metric.Derivation.Factor, "Inputs": inputs}
		} else {
			points := make([]any, 0)
			for _, point := range p.SortedPoints(metric.ID) {
				points = append(points, map[string]any{
					"Sequence":           point.Seq,
					"TimestampUnixMilli": point.TimestampMS,
					"Value":              point.Value,
					"Weight":             point.Weight,
				})
			}
			m["Points"] = points
		}
		metrics = append(metrics, m)
	}
	return map[string]any{
		"Schema":        string(SchemaV3),
		"ProjectID":     p.ProjectID,
		"ConfigVersion": p.ConfigVersion,
		"BaseTimeMS":    p.BaseTimeMS,
		"RangeStartMS":  p.RangeStartMS,
		"RangeEndMS":    p.RangeEndMS,
		"Tolerance":     p.Tolerance,
		"Metrics":       metrics,
		"FunctionCatalog": map[string]any{
			"Organization": "flat_catalog",
			"Functions": []any{
				map[string]any{"Name": "avg", "Op": "avg"},
				map[string]any{"Name": "sum", "Op": "sum"},
				map[string]any{"Name": "min", "Op": "min"},
				map[string]any{"Name": "max", "Op": "max"},
				map[string]any{"Name": "weighted_avg", "Op": "weighted_avg"},
				map[string]any{"Name": "derived_value", "Op": "derived_value"},
			},
		},
		"Degradation": map[string]any{"MissingFillField": p.Degradation.MissingFillField, "FillLookback": p.Degradation.FillLookback},
	}
}

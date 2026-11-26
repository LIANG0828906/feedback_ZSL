package downsample

import "encoding/json"

func loadV1(data []byte) (*Project, *Error) {
	root, err := parseStrictObject(data, SchemaV1, "$", allowedSet(
		"schema_version", "project_id", "config_version", "base_time_ms",
		"range_start_ms", "range_end_ms", "declared_tolerance", "metrics",
		"function_table", "degradation",
	))
	if err != nil {
		return nil, err
	}

	version, err := getString(root, "schema_version", SchemaV1, "$", true)
	if err != nil {
		return nil, err
	}
	if version != string(SchemaV1) {
		return nil, NewError(CodeSchemaUnsupportedVersion, "unsupported schema_version").
			WithVersion(version).WithFieldPath("$.schema_version")
	}

	projectID, err := getString(root, "project_id", SchemaV1, "$", true)
	if err != nil {
		return nil, err
	}
	configVersion, err := getString(root, "config_version", SchemaV1, "$", true)
	if err != nil {
		return nil, err
	}
	baseMS, err := getInt64(root, "base_time_ms", SchemaV1, "$", true)
	if err != nil {
		return nil, err
	}
	rangeStart, err := getInt64(root, "range_start_ms", SchemaV1, "$", true)
	if err != nil {
		return nil, err
	}
	rangeEnd, err := getInt64(root, "range_end_ms", SchemaV1, "$", true)
	if err != nil {
		return nil, err
	}
	tolerance, err := getFloat64(root, "declared_tolerance", SchemaV1, "$", false)
	if err != nil {
		return nil, err
	}
	if tolerance == 0 {
		tolerance = 1e-6
	}

	degradation := DegradationPolicy{MissingFillField: "disabled", FillLookback: 2}
	if raw, ok := root.raw("degradation"); ok {
		obj, perr := parseStrictObject(raw, SchemaV1, "$.degradation", allowedSet("missing_fill_field", "default_fill_lookback_windows"))
		if perr != nil {
			return nil, perr
		}
		degradation.MissingFillField, err = getString(obj, "missing_fill_field", SchemaV1, "$.degradation", false)
		if err != nil {
			return nil, err
		}
		degradation.FillLookback, err = getInt(obj, "default_fill_lookback_windows", SchemaV1, "$.degradation", false)
		if err != nil {
			return nil, err
		}
	}

	funcTable := DefaultFunctionTable()
	if raw, ok := root.raw("function_table"); ok {
		funcTable, err = parseV1FunctionTable(raw)
		if err != nil {
			return nil, err
		}
	}
	if verr := ValidateFunctionTable(funcTable); verr != nil {
		return nil, verr.WithVersion(string(SchemaV1))
	}

	metricsRaw, ok := root.raw("metrics")
	if !ok {
		return nil, NewError(CodeSchemaFieldCombination, "metrics is required").
			WithVersion(string(SchemaV1)).WithFieldPath("$.metrics")
	}
	var rawMetrics []json.RawMessage
	if jerr := json.Unmarshal(metricsRaw, &rawMetrics); jerr != nil {
		return nil, NewError(CodeSchemaInvalidJSON, "metrics must be an array").
			WithVersion(string(SchemaV1)).WithFieldPath("$.metrics")
	}

	project := &Project{
		SchemaVersion:  SchemaV1,
		ProjectID:      projectID,
		ConfigVersion:  configVersion,
		BaseTimeMS:     baseMS,
		RangeStartMS:   rangeStart,
		RangeEndMS:     rangeEnd,
		Tolerance:      tolerance,
		Series:         map[string][]Point{},
		Functions:      funcTable,
		Degradation:    degradation,
		SourceDocument: append([]byte(nil), data...),
	}

	for i, metricRaw := range rawMetrics {
		path := indexPath("$.metrics", i)
		metric, points, merr := parseV1Metric(metricRaw, path, degradation)
		if merr != nil {
			return nil, merr
		}
		project.Metrics = append(project.Metrics, metric)
		project.Series[metric.ID] = points
	}
	return project, nil
}

func parseV1FunctionTable(raw json.RawMessage) (FunctionTable, *Error) {
	obj, err := parseStrictObject(raw, SchemaV1, "$.function_table", allowedSet("organization", "constants"))
	if err != nil {
		return FunctionTable{}, err
	}
	org, _ := getString(obj, "organization", SchemaV1, "$.function_table", false)
	table := FunctionTable{Organization: org, Defs: map[string]FunctionDef{}}
	constantsRaw, ok := obj.raw("constants")
	if !ok {
		return table, nil
	}
	var defRaws []json.RawMessage
	if jerr := json.Unmarshal(constantsRaw, &defRaws); jerr != nil {
		return table, NewError(CodeSchemaInvalidJSON, "constants must be an array").
			WithVersion(string(SchemaV1)).WithFieldPath("$.function_table.constants")
	}
	for i, defRaw := range defRaws {
		path := indexPath("$.function_table.constants", i)
		def, derr := parseFunctionDef(defRaw, SchemaV1, path, allowedSet("name", "op", "ref"))
		if derr != nil {
			return table, derr
		}
		table.Defs[def.Name] = def
	}
	return table, nil
}

func parseFunctionDef(raw json.RawMessage, version SchemaVersion, path string, allowed map[string]bool) (FunctionDef, *Error) {
	obj, err := parseStrictObject(raw, version, path, allowed)
	if err != nil {
		return FunctionDef{}, err
	}
	name, err := getString(obj, firstPresent(obj, "name", "functionName", "Name"), version, path, true)
	if err != nil {
		return FunctionDef{}, err
	}
	opValue, _ := getString(obj, firstPresent(obj, "op", "builtin", "Op"), version, path, false)
	ref, _ := getString(obj, firstPresent(obj, "ref", "aliasRef", "Ref"), version, path, false)
	def := FunctionDef{Name: name, Ref: ref}
	if opValue != "" {
		def.Op = parseFuncName(opValue)
	}
	return def, nil
}

func firstPresent(obj rawObject, keys ...string) string {
	for _, key := range keys {
		if _, ok := obj.raw(key); ok {
			return key
		}
	}
	return keys[0]
}

func parseV1Metric(raw json.RawMessage, path string, degradation DegradationPolicy) (Metric, []Point, *Error) {
	obj, err := parseStrictObject(raw, SchemaV1, path, allowedSet(
		"metric_id", "metric_order", "kind", "window_width_ms", "alignment_basis",
		"anchor_ms", "weight_scale", "fixed_bit_width", "aggregate_functions",
		"samples", "derivation",
	))
	if err != nil {
		return Metric{}, nil, err
	}
	id, err := getString(obj, "metric_id", SchemaV1, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	ordinal, err := getInt(obj, "metric_order", SchemaV1, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	kind, _ := getString(obj, "kind", SchemaV1, path, false)
	if kind == "" {
		kind = "raw"
	}
	width, err := getInt64(obj, "window_width_ms", SchemaV1, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	if width <= 0 {
		return Metric{}, nil, NewError(CodeWindowWidthZero, "window width must be positive").
			WithVersion(string(SchemaV1)).WithFieldPath(path+".window_width_ms").
			WithContext("metric_id", id).WithContext("width", width)
	}
	alignment, _ := getString(obj, "alignment_basis", SchemaV1, path, true)
	anchor, err := getInt64(obj, "anchor_ms", SchemaV1, path, false)
	if err != nil {
		return Metric{}, nil, err
	}
	sourceAlignment := SourceAlignEpoch
	switch alignment {
	case "integer_minute_epoch", "epoch_grid", "whole_hour_epoch":
		sourceAlignment = SourceAlignEpoch
		if anchor == 0 {
			anchor = 0
		}
	case "first_data_point":
		sourceAlignment = SourceAlignFirstPoint
	default:
		return Metric{}, nil, NewError(CodeSchemaFieldCombination, "unsupported alignment_basis").
			WithVersion(string(SchemaV1)).WithFieldPath(path+".alignment_basis").
			WithContext("value", alignment)
	}

	scaleValue, err := getString(obj, "weight_scale", SchemaV1, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	scale, serr := parseScale(scaleValue, string(SchemaV1), path+".weight_scale")
	if serr != nil {
		return Metric{}, nil, serr.WithContext("metric_id", id)
	}
	bits, err := getInt(obj, "fixed_bit_width", SchemaV1, path, false)
	if err != nil {
		return Metric{}, nil, err
	}
	if bits == 0 {
		bits = DefaultWeightBits
	}
	if berr := validateWeightBits(bits, path+".fixed_bit_width"); berr != nil {
		return Metric{}, nil, berr.WithVersion(string(SchemaV1)).WithContext("metric_id", id)
	}

	funcs, err := parseStringList(obj, "aggregate_functions", SchemaV1, path)
	if err != nil {
		return Metric{}, nil, err
	}
	functions := make([]Func, 0, len(funcs))
	for _, f := range funcs {
		functions = append(functions, parseFuncName(f))
	}

	fill := FillPolicy{
		Enabled:            degradation.MissingFillField == "use_declared_default",
		Strategy:           "last_valid_window",
		MaxLookbackWindows: degradation.FillLookback,
		FieldPresent:       false,
		Degraded:           true,
		DegradationRule:    degradation.MissingFillField,
	}

	metric := Metric{
		ID: id, Ordinal: ordinal, Kind: kind, WindowMS: width, AnchorMS: anchor,
		SourceAlignment: sourceAlignment, WeightScale: scale, WeightBits: bits,
		Functions: functions, Fill: fill,
	}

	if raw, ok := obj.raw("derivation"); ok {
		derivation, derr := parseDerivation(raw, SchemaV1, path+".derivation", "metric", "function")
		if derr != nil {
			return Metric{}, nil, derr
		}
		metric.Derivation = &derivation
	}

	var points []Point
	if raw, ok := obj.raw("samples"); ok && kind != "derived" {
		var pointRaws []json.RawMessage
		if jerr := json.Unmarshal(raw, &pointRaws); jerr != nil {
			return Metric{}, nil, NewError(CodeSchemaInvalidJSON, "samples must be an array").
				WithVersion(string(SchemaV1)).WithFieldPath(path + ".samples")
		}
		for i, pr := range pointRaws {
			ppath := indexPath(path+".samples", i)
			point, perr := parseV1Point(pr, ppath, id, scale, bits)
			if perr != nil {
				return Metric{}, nil, perr
			}
			points = append(points, point)
		}
	}
	return metric, points, nil
}

func parseStringList(obj rawObject, key string, version SchemaVersion, path string) ([]string, *Error) {
	raw, ok := obj.raw(key)
	if !ok {
		return nil, NewError(CodeSchemaFieldCombination, key+" is required").
			WithVersion(string(version)).WithFieldPath(joinPath(path, key))
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, NewError(CodeSchemaInvalidJSON, key+" must be a string array").
			WithVersion(string(version)).WithFieldPath(joinPath(path, key))
	}
	return out, nil
}

func parseDerivation(raw json.RawMessage, version SchemaVersion, path, metricKey, funcKey string) (Derivation, *Error) {
	obj, err := parseStrictObject(raw, version, path, allowedSet(
		"inputs", "Inputs", "op", "Op", "factor", "Factor",
	))
	if err != nil {
		return Derivation{}, err
	}
	opKey := firstPresent(obj, "op", "Op")
	op, err := getString(obj, opKey, version, path, true)
	if err != nil {
		return Derivation{}, err
	}
	factorKey := firstPresent(obj, "factor", "Factor")
	factor, _ := getFloat64(obj, factorKey, version, path, false)
	out := Derivation{Op: op, Factor: factor}
	inputsKey := firstPresent(obj, "inputs", "Inputs")
	inputsRaw, ok := obj.raw(inputsKey)
	if !ok {
		return out, NewError(CodeSchemaFieldCombination, "derivation inputs required").
			WithVersion(string(version)).WithFieldPath(path + "." + inputsKey)
	}
	var raws []json.RawMessage
	if jerr := json.Unmarshal(inputsRaw, &raws); jerr != nil {
		return out, NewError(CodeSchemaInvalidJSON, "inputs must be array").
			WithVersion(string(version)).WithFieldPath(path + ".inputs")
	}
	for i, inputRaw := range raws {
		ipath := indexPath(path+"."+inputsKey, i)
		lagKey := "lag"
		if metricKey == "InputMetric" {
			lagKey = "Lag"
		}
		inputObj, ierr := parseStrictObject(inputRaw, version, ipath, allowedSet(metricKey, funcKey, "lag", "Lag"))
		if ierr != nil {
			return out, ierr
		}
		metricID, ierr := getString(inputObj, metricKey, version, ipath, true)
		if ierr != nil {
			return out, ierr
		}
		fnName, ierr := getString(inputObj, funcKey, version, ipath, true)
		if ierr != nil {
			return out, ierr
		}
		selectedLagKey := firstPresent(inputObj, "lag", "Lag")
		_ = lagKey
		lag, _ := getInt(inputObj, selectedLagKey, version, ipath, false)
		out.Inputs = append(out.Inputs, DerivedInput{MetricID: metricID, Function: parseFuncName(fnName), Lag: lag})
	}
	return out, nil
}

func parseV1Point(raw json.RawMessage, path, metricID string, scale WeightScale, bits int) (Point, *Error) {
	obj, err := parseStrictObject(raw, SchemaV1, path, allowedSet("seq", "timestamp_ms", "value", "weight_permille"))
	if err != nil {
		return Point{}, err
	}
	seq, err := getInt64(obj, "seq", SchemaV1, path, true)
	if err != nil {
		return Point{}, err
	}
	ts, err := getInt64(obj, "timestamp_ms", SchemaV1, path, true)
	if err != nil {
		return Point{}, err
	}
	value, err := getFloat64(obj, "value", SchemaV1, path, true)
	if err != nil {
		return Point{}, err
	}
	weightRaw, err := getInt64(obj, "weight_permille", SchemaV1, path, true)
	if err != nil {
		return Point{}, err
	}
	canonical, rep, cerr := CanonicalWeight(weightRaw, scale, bits)
	point := Point{
		Seq: seq, MetricID: metricID, TimestampMS: ts, Value: value, Weight: canonical,
		SourceWeightRaw: weightRaw, SourceWeightScale: int64(scale),
	}
	if cerr != nil {
		return point, cerr.WithVersion(string(SchemaV1)).WithFieldPath(path+".weight_permille").
			WithContext("conversion", rep)
	}
	return point, nil
}

package downsample

import "encoding/json"

func loadV2(data []byte) (*Project, *Error) {
	root, err := parseStrictObject(data, SchemaV2, "$", allowedSet(
		"schemaVersion", "projectId", "configVersion", "baseTimestampMs",
		"rangeStartMs", "rangeEndMs", "declaredTolerance", "metrics",
		"functionTree", "degradation",
	))
	if err != nil {
		return nil, err
	}
	version, err := getString(root, "schemaVersion", SchemaV2, "$", true)
	if err != nil {
		return nil, err
	}
	if version != string(SchemaV2) {
		return nil, NewError(CodeSchemaUnsupportedVersion, "unsupported schemaVersion").
			WithVersion(version).WithFieldPath("$.schemaVersion")
	}
	projectID, err := getString(root, "projectId", SchemaV2, "$", true)
	if err != nil {
		return nil, err
	}
	configVersion, err := getString(root, "configVersion", SchemaV2, "$", true)
	if err != nil {
		return nil, err
	}
	baseMS, err := getInt64(root, "baseTimestampMs", SchemaV2, "$", true)
	if err != nil {
		return nil, err
	}
	rangeStart, err := getInt64(root, "rangeStartMs", SchemaV2, "$", true)
	if err != nil {
		return nil, err
	}
	rangeEnd, err := getInt64(root, "rangeEndMs", SchemaV2, "$", true)
	if err != nil {
		return nil, err
	}
	tolerance, _ := getFloat64(root, "declaredTolerance", SchemaV2, "$", false)
	if tolerance == 0 {
		tolerance = 1e-6
	}

	degradation := DegradationPolicy{MissingFillField: "disabled", FillLookback: 2}
	if raw, ok := root.raw("degradation"); ok {
		obj, perr := parseStrictObject(raw, SchemaV2, "$.degradation", allowedSet("missingFillField", "defaultLookbackWindows"))
		if perr != nil {
			return nil, perr
		}
		degradation.MissingFillField, _ = getString(obj, "missingFillField", SchemaV2, "$.degradation", false)
		degradation.FillLookback, _ = getInt(obj, "defaultLookbackWindows", SchemaV2, "$.degradation", false)
	}

	funcTable := DefaultFunctionTable()
	if raw, ok := root.raw("functionTree"); ok {
		funcTable, err = parseV2FunctionTree(raw)
		if err != nil {
			return nil, err
		}
	}
	if verr := ValidateFunctionTable(funcTable); verr != nil {
		return nil, verr.WithVersion(string(SchemaV2))
	}

	metricsRaw, ok := root.raw("metrics")
	if !ok {
		return nil, NewError(CodeSchemaFieldCombination, "metrics is required").
			WithVersion(string(SchemaV2)).WithFieldPath("$.metrics")
	}
	var metricRaws []json.RawMessage
	if jerr := json.Unmarshal(metricsRaw, &metricRaws); jerr != nil {
		return nil, NewError(CodeSchemaInvalidJSON, "metrics must be an array").
			WithVersion(string(SchemaV2)).WithFieldPath("$.metrics")
	}

	project := &Project{
		SchemaVersion: SchemaV2, ProjectID: projectID, ConfigVersion: configVersion,
		BaseTimeMS: baseMS, RangeStartMS: rangeStart, RangeEndMS: rangeEnd,
		Tolerance: tolerance, Series: map[string][]Point{}, Functions: funcTable,
		Degradation: degradation, SourceDocument: append([]byte(nil), data...),
	}
	for i, raw := range metricRaws {
		path := indexPath("$.metrics", i)
		metric, points, merr := parseV2Metric(raw, path)
		if merr != nil {
			return nil, merr
		}
		project.Metrics = append(project.Metrics, metric)
		project.Series[metric.ID] = points
	}
	return project, nil
}

func parseV2FunctionTree(raw json.RawMessage) (FunctionTable, *Error) {
	obj, err := parseStrictObject(raw, SchemaV2, "$.functionTree", allowedSet("organization", "groups"))
	if err != nil {
		return FunctionTable{}, err
	}
	org, _ := getString(obj, "organization", SchemaV2, "$.functionTree", false)
	table := FunctionTable{Organization: "nested_tree", Defs: map[string]FunctionDef{}, Groups: map[string][]FunctionDef{}}
	groupsRaw, ok := obj.raw("groups")
	if !ok {
		return table, nil
	}
	var groups map[string][]json.RawMessage
	if jerr := json.Unmarshal(groupsRaw, &groups); jerr != nil {
		return table, NewError(CodeSchemaInvalidJSON, "groups must be an object of arrays").
			WithVersion(string(SchemaV2)).WithFieldPath("$.functionTree.groups")
	}
	for groupName, defRaws := range groups {
		gpath := "$.functionTree.groups." + groupName
		for i, defRaw := range defRaws {
			path := indexPath(gpath, i)
			def, derr := parseFunctionDef(defRaw, SchemaV2, path, allowedSet("functionName", "builtin", "aliasRef"))
			if derr != nil {
				return table, derr
			}
			table.Defs[def.Name] = def
			table.Groups[groupName] = append(table.Groups[groupName], def)
		}
	}
	_ = org
	return table, nil
}

func parseV2Metric(raw json.RawMessage, path string) (Metric, []Point, *Error) {
	obj, err := parseStrictObject(raw, SchemaV2, path, allowedSet(
		"id", "order", "kind", "windowDurationMs", "alignmentBasis",
		"anchorTimestampMs", "weightUnit", "fixedBitWidth",
		"aggregationFunctions", "points", "lastValueFill", "derivation",
	))
	if err != nil {
		return Metric{}, nil, err
	}
	id, err := getString(obj, "id", SchemaV2, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	ordinal, err := getInt(obj, "order", SchemaV2, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	kind, _ := getString(obj, "kind", SchemaV2, path, false)
	if kind == "" {
		kind = "raw"
	}
	width, err := getInt64(obj, "windowDurationMs", SchemaV2, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	if width <= 0 {
		return Metric{}, nil, NewError(CodeWindowWidthZero, "windowDurationMs must be positive").
			WithVersion(string(SchemaV2)).WithFieldPath(path+".windowDurationMs").
			WithContext("metric_id", id).WithContext("width", width)
	}
	basis, err := getString(obj, "alignmentBasis", SchemaV2, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	anchor, err := getInt64(obj, "anchorTimestampMs", SchemaV2, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	sourceAlignment := SourceAlignFirstPoint
	switch basis {
	case "first_data_point", "firstSample":
		sourceAlignment = SourceAlignFirstPoint
	case "epoch_grid", "integer_minute_epoch":
		sourceAlignment = SourceAlignEpoch
	default:
		return Metric{}, nil, NewError(CodeSchemaFieldCombination, "unsupported alignmentBasis").
			WithVersion(string(SchemaV2)).WithFieldPath(path+".alignmentBasis").
			WithContext("value", basis)
	}

	unit, err := getString(obj, "weightUnit", SchemaV2, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	scale, serr := parseScale(unit, string(SchemaV2), path+".weightUnit")
	if serr != nil {
		return Metric{}, nil, serr.WithContext("metric_id", id)
	}
	bits, _ := getInt(obj, "fixedBitWidth", SchemaV2, path, false)
	if bits == 0 {
		bits = DefaultWeightBits
	}
	if berr := validateWeightBits(bits, path+".fixedBitWidth"); berr != nil {
		return Metric{}, nil, berr.WithVersion(string(SchemaV2)).WithContext("metric_id", id)
	}
	funcNames, err := parseStringList(obj, "aggregationFunctions", SchemaV2, path)
	if err != nil {
		return Metric{}, nil, err
	}
	functions := make([]Func, 0, len(funcNames))
	for _, name := range funcNames {
		functions = append(functions, parseFuncName(name))
	}

	fill := FillPolicy{Enabled: false, Strategy: "last_valid_window", MaxLookbackWindows: 2, FieldPresent: false}
	if fillRaw, ok := obj.raw("lastValueFill"); ok {
		fill, err = parseV2Fill(fillRaw, path+".lastValueFill")
		if err != nil {
			return Metric{}, nil, err
		}
	}

	metric := Metric{
		ID: id, Ordinal: ordinal, Kind: kind, WindowMS: width, AnchorMS: anchor,
		SourceAlignment: sourceAlignment, WeightScale: scale, WeightBits: bits,
		Functions: functions, Fill: fill,
	}
	if raw, ok := obj.raw("derivation"); ok {
		derivation, derr := parseDerivation(raw, SchemaV2, path+".derivation", "metricId", "function")
		if derr != nil {
			return Metric{}, nil, derr
		}
		metric.Derivation = &derivation
	}

	var points []Point
	if raw, ok := obj.raw("points"); ok && kind != "derived" {
		var raws []json.RawMessage
		if jerr := json.Unmarshal(raw, &raws); jerr != nil {
			return Metric{}, nil, NewError(CodeSchemaInvalidJSON, "points must be an array").
				WithVersion(string(SchemaV2)).WithFieldPath(path + ".points")
		}
		for i, pr := range raws {
			ppath := indexPath(path+".points", i)
			point, perr := parseV2Point(pr, ppath, id, width, anchor, scale, bits)
			if perr != nil {
				return Metric{}, nil, perr
			}
			points = append(points, point)
		}
	}
	return metric, points, nil
}

func parseV2Fill(raw json.RawMessage, path string) (FillPolicy, *Error) {
	obj, err := parseStrictObject(raw, SchemaV2, path, allowedSet("enabled", "strategy", "maxLookbackWindows"))
	if err != nil {
		return FillPolicy{}, err
	}
	enabled, err := getBool(obj, "enabled", SchemaV2, path, true)
	if err != nil {
		return FillPolicy{}, err
	}
	strategy, _ := getString(obj, "strategy", SchemaV2, path, false)
	if strategy == "" {
		strategy = "last_valid_window"
	}
	lookback, err := getInt(obj, "maxLookbackWindows", SchemaV2, path, true)
	if err != nil {
		return FillPolicy{}, err
	}
	if lookback < 0 {
		return FillPolicy{}, NewError(CodeConfigEditInvalid, "lookback must be non-negative").
			WithVersion(string(SchemaV2)).WithFieldPath(path + ".maxLookbackWindows")
	}
	return FillPolicy{Enabled: enabled, Strategy: strategy, MaxLookbackWindows: lookback, FieldPresent: true}, nil
}

func parseV2Point(raw json.RawMessage, path, metricID string, width, anchor int64, scale WeightScale, bits int) (Point, *Error) {
	obj, err := parseStrictObject(raw, SchemaV2, path, allowedSet("sequence", "windowIndex", "offsetWithinWindowMs", "sampleValue", "weightRaw"))
	if err != nil {
		return Point{}, err
	}
	seq, err := getInt64(obj, "sequence", SchemaV2, path, true)
	if err != nil {
		return Point{}, err
	}
	windowIndex, err := getInt64(obj, "windowIndex", SchemaV2, path, true)
	if err != nil {
		return Point{}, err
	}
	offset, err := getInt64(obj, "offsetWithinWindowMs", SchemaV2, path, true)
	if err != nil {
		return Point{}, err
	}
	if offset < 0 || offset >= width {
		return Point{}, NewError(CodeRelativeOffsetRange, "relative offset must satisfy 0 <= offset < window width; use next window index for the right boundary").
			WithVersion(string(SchemaV2)).
			WithFieldPath(path+".offsetWithinWindowMs").
			WithContext("metric_id", metricID).
			WithContext("window_index", windowIndex).
			WithContext("offset", offset).
			WithContext("window_width_ms", width)
	}
	value, err := getFloat64(obj, "sampleValue", SchemaV2, path, true)
	if err != nil {
		return Point{}, err
	}
	weightRaw, err := getInt64(obj, "weightRaw", SchemaV2, path, true)
	if err != nil {
		return Point{}, err
	}
	ts := anchor + windowIndex*width + offset
	canonical, rep, cerr := CanonicalWeight(weightRaw, scale, bits)
	point := Point{
		Seq: seq, MetricID: metricID, TimestampMS: ts, Value: value, Weight: canonical,
		SourceWeightRaw: weightRaw, SourceWeightScale: int64(scale),
		RelativeWindowIndex: &windowIndex, RelativeOffsetMS: &offset,
	}
	if cerr != nil {
		return point, cerr.WithVersion(string(SchemaV2)).WithFieldPath(path+".weightRaw").
			WithContext("conversion", rep)
	}
	return point, nil
}

package downsample

import "encoding/json"

func LoadProject(data []byte) (*Project, *Error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, NewError(CodeSchemaInvalidJSON, "document is not valid JSON").
			WithContext("parse_error", err.Error())
	}
	if _, ok := head["schema_version"]; ok {
		return loadV1(data)
	}
	if _, ok := head["schemaVersion"]; ok {
		return loadV2(data)
	}
	if _, ok := head["Schema"]; ok {
		return loadV3(data)
	}
	return nil, NewError(CodeSchemaUnsupportedVersion, "unrecognized schema version envelope").
		WithFieldPath("$.schema_version").
		WithContext("allowed", []string{"schema_version", "schemaVersion", "Schema"})
}

func loadV3(data []byte) (*Project, *Error) {
	root, err := parseStrictObject(data, SchemaV3, "$", allowedSet(
		"Schema", "ProjectID", "ConfigVersion", "BaseTimeMS", "RangeStartMS",
		"RangeEndMS", "Tolerance", "Metrics", "FunctionCatalog", "Degradation",
	))
	if err != nil {
		return nil, err
	}
	version, err := getString(root, "Schema", SchemaV3, "$", true)
	if err != nil {
		return nil, err
	}
	if version != string(SchemaV3) {
		return nil, NewError(CodeSchemaUnsupportedVersion, "unsupported Schema").
			WithVersion(version).WithFieldPath("$.Schema")
	}
	projectID, _ := getString(root, "ProjectID", SchemaV3, "$", true)
	configVersion, _ := getString(root, "ConfigVersion", SchemaV3, "$", true)
	baseMS, err := getInt64(root, "BaseTimeMS", SchemaV3, "$", true)
	if err != nil {
		return nil, err
	}
	rangeStart, err := getInt64(root, "RangeStartMS", SchemaV3, "$", true)
	if err != nil {
		return nil, err
	}
	rangeEnd, err := getInt64(root, "RangeEndMS", SchemaV3, "$", true)
	if err != nil {
		return nil, err
	}
	tolerance, _ := getFloat64(root, "Tolerance", SchemaV3, "$", false)
	if tolerance == 0 {
		tolerance = 1e-6
	}
	degradation := DegradationPolicy{MissingFillField: "use_declared_default", FillLookback: 2}
	if raw, ok := root.raw("Degradation"); ok {
		obj, perr := parseStrictObject(raw, SchemaV3, "$.Degradation", allowedSet("MissingFillField", "FillLookback"))
		if perr != nil {
			return nil, perr
		}
		degradation.MissingFillField, _ = getString(obj, "MissingFillField", SchemaV3, "$.Degradation", false)
		degradation.FillLookback, _ = getInt(obj, "FillLookback", SchemaV3, "$.Degradation", false)
	}

	funcTable := DefaultFunctionTable()
	if raw, ok := root.raw("FunctionCatalog"); ok {
		funcTable, err = parseV3Catalog(raw)
		if err != nil {
			return nil, err
		}
	}
	if verr := ValidateFunctionTable(funcTable); verr != nil {
		return nil, verr.WithVersion(string(SchemaV3))
	}

	var rawMetrics []json.RawMessage
	if raw, ok := root.raw("Metrics"); ok {
		if jerr := json.Unmarshal(raw, &rawMetrics); jerr != nil {
			return nil, NewError(CodeSchemaInvalidJSON, "Metrics must be array").
				WithVersion(string(SchemaV3)).WithFieldPath("$.Metrics")
		}
	}
	project := &Project{
		SchemaVersion: SchemaV3, ProjectID: projectID, ConfigVersion: configVersion,
		BaseTimeMS: baseMS, RangeStartMS: rangeStart, RangeEndMS: rangeEnd,
		Tolerance: tolerance, Series: map[string][]Point{}, Functions: funcTable,
		Degradation: degradation, SourceDocument: append([]byte(nil), data...),
	}
	for i, raw := range rawMetrics {
		path := indexPath("$.Metrics", i)
		metric, points, merr := parseV3Metric(raw, path)
		if merr != nil {
			return nil, merr
		}
		project.Metrics = append(project.Metrics, metric)
		project.Series[metric.ID] = points
	}
	return project, nil
}

func parseV3Catalog(raw json.RawMessage) (FunctionTable, *Error) {
	obj, err := parseStrictObject(raw, SchemaV3, "$.FunctionCatalog", allowedSet("Organization", "Functions"))
	if err != nil {
		return FunctionTable{}, err
	}
	org, _ := getString(obj, "Organization", SchemaV3, "$.FunctionCatalog", false)
	table := FunctionTable{Organization: org, Defs: map[string]FunctionDef{}}
	functionsRaw, ok := obj.raw("Functions")
	if !ok {
		return table, nil
	}
	var defRaws []json.RawMessage
	if jerr := json.Unmarshal(functionsRaw, &defRaws); jerr != nil {
		return table, NewError(CodeSchemaInvalidJSON, "Functions must be array").
			WithVersion(string(SchemaV3)).WithFieldPath("$.FunctionCatalog.Functions")
	}
	for i, defRaw := range defRaws {
		path := indexPath("$.FunctionCatalog.Functions", i)
		def, derr := parseFunctionDef(defRaw, SchemaV3, path, allowedSet("Name", "Op", "Ref"))
		if derr != nil {
			return table, derr
		}
		table.Defs[def.Name] = def
	}
	return table, nil
}

func parseV3Metric(raw json.RawMessage, path string) (Metric, []Point, *Error) {
	obj, err := parseStrictObject(raw, SchemaV3, path, allowedSet(
		"MetricID", "Ordinal", "Kind", "WindowMS", "Alignment", "AnchorMS",
		"WeightScale", "WeightBits", "Functions", "Points", "FillPolicy", "Derivation",
	))
	if err != nil {
		return Metric{}, nil, err
	}
	id, _ := getString(obj, "MetricID", SchemaV3, path, true)
	ordinal, _ := getInt(obj, "Ordinal", SchemaV3, path, true)
	kind, _ := getString(obj, "Kind", SchemaV3, path, false)
	if kind == "" {
		kind = "raw"
	}
	width, err := getInt64(obj, "WindowMS", SchemaV3, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	if width <= 0 {
		return Metric{}, nil, NewError(CodeWindowWidthZero, "WindowMS must be positive").
			WithVersion(string(SchemaV3)).WithFieldPath(path+".WindowMS").
			WithContext("metric_id", id)
	}
	alignment, _ := getString(obj, "Alignment", SchemaV3, path, false)
	anchor, err := getInt64(obj, "AnchorMS", SchemaV3, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	sourceAlignment := SourceAlignEpoch
	if alignment == "first_data_point" {
		sourceAlignment = SourceAlignFirstPoint
	}
	scaleValue, err := getString(obj, "WeightScale", SchemaV3, path, true)
	if err != nil {
		return Metric{}, nil, err
	}
	scale, serr := parseScale(scaleValue, string(SchemaV3), path+".WeightScale")
	if serr != nil {
		return Metric{}, nil, serr.WithContext("metric_id", id)
	}
	bits, _ := getInt(obj, "WeightBits", SchemaV3, path, false)
	if bits == 0 {
		bits = DefaultWeightBits
	}
	names, err := parseStringList(obj, "Functions", SchemaV3, path)
	if err != nil {
		return Metric{}, nil, err
	}
	fns := make([]Func, 0, len(names))
	for _, name := range names {
		fns = append(fns, parseFuncName(name))
	}
	fill := FillPolicy{Enabled: true, Strategy: "last_valid_window", MaxLookbackWindows: 2, FieldPresent: true}
	if fillRaw, ok := obj.raw("FillPolicy"); ok {
		fill, err = parseV3Fill(fillRaw, path+".FillPolicy")
		if err != nil {
			return Metric{}, nil, err
		}
	}
	metric := Metric{
		ID: id, Ordinal: ordinal, Kind: kind, WindowMS: width, AnchorMS: anchor,
		SourceAlignment: sourceAlignment, WeightScale: scale, WeightBits: bits,
		Functions: fns, Fill: fill,
	}
	if rawDerivation, ok := obj.raw("Derivation"); ok {
		derivation, derr := parseDerivation(rawDerivation, SchemaV3, path+".Derivation", "InputMetric", "Function")
		if derr != nil {
			return Metric{}, nil, derr
		}
		metric.Derivation = &derivation
	}
	var points []Point
	if pointsRaw, ok := obj.raw("Points"); ok && kind != "derived" {
		var raws []json.RawMessage
		if jerr := json.Unmarshal(pointsRaw, &raws); jerr != nil {
			return Metric{}, nil, NewError(CodeSchemaInvalidJSON, "Points must be array").
				WithVersion(string(SchemaV3)).WithFieldPath(path + ".Points")
		}
		for i, pr := range raws {
			ppath := indexPath(path+".Points", i)
			point, perr := parseV3Point(pr, ppath, id, scale, bits)
			if perr != nil {
				return Metric{}, nil, perr
			}
			points = append(points, point)
		}
	}
	return metric, points, nil
}

func parseV3Fill(raw json.RawMessage, path string) (FillPolicy, *Error) {
	obj, err := parseStrictObject(raw, SchemaV3, path, allowedSet("Enabled", "Strategy", "LookbackWindows"))
	if err != nil {
		return FillPolicy{}, err
	}
	enabled, _ := getBool(obj, "Enabled", SchemaV3, path, false)
	strategy, _ := getString(obj, "Strategy", SchemaV3, path, false)
	if strategy == "" {
		strategy = "last_valid_window"
	}
	lookback, err := getInt(obj, "LookbackWindows", SchemaV3, path, true)
	if err != nil {
		return FillPolicy{}, err
	}
	return FillPolicy{Enabled: enabled, Strategy: strategy, MaxLookbackWindows: lookback, FieldPresent: true}, nil
}

func parseV3Point(raw json.RawMessage, path, metricID string, scale WeightScale, bits int) (Point, *Error) {
	obj, err := parseStrictObject(raw, SchemaV3, path, allowedSet("Sequence", "TimestampUnixMilli", "Value", "Weight"))
	if err != nil {
		return Point{}, err
	}
	seq, _ := getInt64(obj, "Sequence", SchemaV3, path, true)
	ts, err := getInt64(obj, "TimestampUnixMilli", SchemaV3, path, true)
	if err != nil {
		return Point{}, err
	}
	value, err := getFloat64(obj, "Value", SchemaV3, path, true)
	if err != nil {
		return Point{}, err
	}
	weightRaw, err := getInt64(obj, "Weight", SchemaV3, path, true)
	if err != nil {
		return Point{}, err
	}
	canonical, rep, cerr := CanonicalWeight(weightRaw, scale, bits)
	point := Point{
		Seq: seq, MetricID: metricID, TimestampMS: ts, Value: value, Weight: canonical,
		SourceWeightRaw: weightRaw, SourceWeightScale: int64(scale),
	}
	if cerr != nil {
		return point, cerr.WithVersion(string(SchemaV3)).WithFieldPath(path+".Weight").
			WithContext("conversion", rep)
	}
	return point, nil
}

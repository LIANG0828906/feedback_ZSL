package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

// LoadedDocument preserves schema diagnostics alongside the normalized model.
type LoadedDocument struct {
	Version  string   `json:"version"`
	Project  *Project `json:"project"`
	Warnings []string `json:"warnings,omitempty"`
}

// LoadJSON detects the schema version and converts it to the normalized model.
func LoadJSON(data []byte) (*LoadedDocument, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, NewBusinessError(CodeSchemaUnsupported, "invalid JSON document: %v", err).
			WithContext("stage", "json_decode")
	}
	if dec.More() {
		return nil, NewBusinessError(CodeSchemaUnsupported, "multiple JSON values are not supported").
			WithContext("stage", "json_decode")
	}
	obj, err := objectAt(raw, "$")
	if err != nil {
		return nil, err
	}
	versionRaw, versionKey, err := pickAt(obj, "$", "schema_version", "schemaVersion")
	if err != nil {
		return nil, err
	}
	if versionKey == "" {
		return nil, NewBusinessError(CodeSchemaUnsupported, "schema version is missing").
			WithContext("path", "$.schema_version")
	}
	version, err := scalarString(versionRaw, "$."+versionKey)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "v1", "1", "legacy":
		return loadV1(obj)
	case "v2", "2", "edge":
		return loadV2(obj)
	case "v3", "3", "compact":
		return loadV3(obj)
	default:
		return nil, NewBusinessError(CodeSchemaUnsupported, "unsupported schema version %q", version).
			WithContext("path", "$."+versionKey).
			WithContext("version", version)
	}
}

func loadV1(root map[string]any) (*LoadedDocument, error) {
	const version = "v1"
	origin, err := requiredTimeInt(root, "$", "time_origin_ms")
	if err != nil {
		return nil, err
	}
	unit, err := requiredTimeInt(root, "$", "time_unit_ms")
	if err != nil {
		return nil, err
	}
	alignment, err := requiredString(root, "$", "alignment")
	if err != nil {
		return nil, err
	}
	weightScale, err := requiredString(root, "$", "weight_scale")
	if err != nil {
		return nil, err
	}
	watermarkEnabled, err := optionalBool(root, "$", "watermark_enabled")
	if err != nil {
		return nil, err
	}
	if alignment != string(AlignEpoch) {
		return nil, ambiguousCombination(version, "epoch timestamp", alignment, "$.alignment")
	}
	return loadCommon(root, commonLoad{
		version: version, origin: origin, unit: unit,
		alignment: WindowAlignment(alignment), weightScale: WeightScale(weightScale),
		watermarkEnabled: watermarkEnabled, timeMode: timeEpoch,
		featureKeys: featureKeysV1, aggKeys: aggKeysV1, eventKeys: eventKeysV1,
	})
}

func loadV2(root map[string]any) (*LoadedDocument, error) {
	const version = "v2"
	origin, err := requiredTimeInt(root, "$", "timeOriginMs")
	if err != nil {
		return nil, err
	}
	unit, err := requiredTimeInt(root, "$", "timeUnitMs")
	if err != nil {
		return nil, err
	}
	alignment, err := requiredString(root, "$", "alignment")
	if err != nil {
		return nil, err
	}
	weightScale, err := requiredString(root, "$", "weightScale")
	if err != nil {
		return nil, err
	}
	watermarkEnabled, err := optionalBool(root, "$", "watermarkEnabled")
	if err != nil {
		return nil, err
	}
	if alignment != string(AlignFirstData) {
		return nil, ambiguousCombination(version, "relative event sequence", alignment, "$.alignment")
	}
	return loadCommon(root, commonLoad{
		version: version, origin: origin, unit: unit,
		alignment: WindowAlignment(alignment), weightScale: WeightScale(weightScale),
		watermarkEnabled: watermarkEnabled, timeMode: timeRelativeTick,
		featureKeys: featureKeysV2, aggKeys: aggKeysV2, eventKeys: eventKeysV2,
	})
}

func loadV3(root map[string]any) (*LoadedDocument, error) {
	const version = "v3"
	origin, err := requiredTimeInt(root, "$", "epoch_origin_ms")
	if err != nil {
		return nil, err
	}
	unit, err := requiredTimeInt(root, "$", "event_tick_ms")
	if err != nil {
		return nil, err
	}
	alignment, err := requiredString(root, "$", "window_alignment")
	if err != nil {
		return nil, err
	}
	weightScale, err := requiredString(root, "$", "weight_scale")
	if err != nil {
		return nil, err
	}
	watermarkEnabled, err := optionalBool(root, "$", "watermark_enabled")
	if err != nil {
		return nil, err
	}
	if alignment != string(AlignFirstData) {
		return nil, ambiguousCombination(version, "compact epoch timestamp", alignment, "$.window_alignment")
	}
	return loadCommon(root, commonLoad{
		version: version, origin: origin, unit: unit,
		alignment: WindowAlignment(alignment), weightScale: WeightScale(weightScale),
		watermarkEnabled: watermarkEnabled, timeMode: timeEpoch,
		featureKeys: featureKeysV3, aggKeys: aggKeysV3, eventKeys: eventKeysV3,
	})
}

type timeMode int

const (
	timeEpoch timeMode = iota
	timeRelativeTick
)

type commonLoad struct {
	version          string
	origin           int64
	unit             int64
	alignment        WindowAlignment
	weightScale      WeightScale
	watermarkEnabled bool
	timeMode         timeMode
	featureKeys      fKeys
	aggKeys          aKeys
	eventKeys        eKeys
}

type fKeys struct {
	id, width, step, lateness, aggs, derived string
}

type aKeys struct {
	kind, weight string
}

type eKeys struct {
	seq, feature, timestamp, channel, value, watermark string
}

var (
	featureKeysV1 = fKeys{"feature_id", "window_width_ms", "window_step_ms", "allowed_lateness_ms", "aggregations", "derived_from"}
	featureKeysV2 = fKeys{"featureId", "windowWidth", "windowStep", "allowedLateness", "aggregations", "derivedFrom"}
	featureKeysV3 = fKeys{"id", "win_ms", "stride_ms", "late_allow_ms", "aggs", "parent_features"}

	aggKeysV1 = aKeys{"function", "weight_permille"}
	aggKeysV2 = aKeys{"fn", "weightPerMyriad"}
	aggKeysV3 = aKeys{"kind", "weight"}

	eventKeysV1 = eKeys{"arrival_seq", "feature_id", "observed_at_ms", "channel", "value", "watermark_ms"}
	eventKeysV2 = eKeys{"arrivalSeq", "featureId", "observedTick", "channelId", "sample", "watermarkTick"}
	eventKeysV3 = eKeys{"seq", "feature", "epoch_ms", "channel", "v", "wm_ms"}
)

func loadCommon(root map[string]any, cfg commonLoad) (*LoadedDocument, error) {
	if err := checkAliases(root, "$",
		[]string{"project_id", "projectId"},
		[]string{"time_origin_ms", "timeOriginMs", "epoch_origin_ms"},
		[]string{"time_unit_ms", "timeUnitMs", "event_tick_ms"},
		[]string{"alignment", "window_alignment"},
		[]string{"weight_scale", "weightScale"},
		[]string{"watermark_enabled", "watermarkEnabled"},
	); err != nil {
		return nil, err
	}
	if cfg.unit <= 0 {
		return nil, NewBusinessError(CodeTimeBasisMissing, "time unit must be positive").
			WithContext("version", cfg.version).WithContext("value", fmt.Sprint(cfg.unit))
	}
	if !cfg.alignment.Valid() {
		return nil, NewBusinessError(CodeSchemaAmbiguous, "unknown alignment %q", cfg.alignment).
			WithContext("version", cfg.version)
	}
	if !cfg.weightScale.Valid() {
		return nil, NewBusinessError(CodeSchemaAmbiguous, "unknown weight scale %q", cfg.weightScale).
			WithContext("version", cfg.version)
	}
	pid, err := requiredString(root, "$", projectIDKey(cfg.version))
	if err != nil {
		return nil, err
	}
	featuresRaw, _, err := pickAt(root, "$", "features")
	if err != nil || featuresRaw == nil {
		if err == nil {
			err = NewBusinessError(CodeSchemaUnsupported, "features array is missing").WithContext("path", "$.features")
		}
		return nil, err
	}
	featureArray, err := arrayAt(featuresRaw, "$.features")
	if err != nil {
		return nil, err
	}

	project := &Project{
		SchemaVersion: cfg.version, ProjectID: pid,
		TimeOriginMs: cfg.origin, TimeTickMs: cfg.unit,
		Alignment: cfg.alignment, WeightScale: cfg.weightScale,
		WatermarkEnabled: cfg.watermarkEnabled,
	}
	seen := map[string]bool{}
	for i, raw := range featureArray {
		path := fmt.Sprintf("$.features[%d]", i)
		obj, err := objectAt(raw, path)
		if err != nil {
			return nil, err
		}
		feature, err := parseFeature(obj, path, cfg)
		if err != nil {
			return nil, err
		}
		if seen[feature.ID] {
			return nil, NewBusinessError(CodeFeatureDuplicate, "duplicate feature %q", feature.ID).
				WithContext("path", path+".id").WithContext("feature_id", feature.ID)
		}
		seen[feature.ID] = true
		project.Features = append(project.Features, feature)
	}
	if len(project.Features) == 0 {
		return nil, NewBusinessError(CodeSchemaUnsupported, "at least one feature is required").
			WithContext("path", "$.features")
	}
	for _, f := range project.Features {
		for _, parent := range f.DerivedFrom {
			if !seen[parent] {
				return nil, NewBusinessError(CodeFeatureUnknown, "derived feature %q references unknown parent %q", f.ID, parent).
					WithContext("feature_id", f.ID).WithContext("parent_feature", parent)
			}
		}
	}

	eventsRaw, _, err := pickAt(root, "$", "events")
	if err != nil || eventsRaw == nil {
		if err == nil {
			err = NewBusinessError(CodeSchemaUnsupported, "events array is missing").WithContext("path", "$.events")
		}
		return nil, err
	}
	eventArray, err := arrayAt(eventsRaw, "$.events")
	if err != nil {
		return nil, err
	}
	warnings := make([]string, 0)
	if !cfg.watermarkEnabled {
		warnings = append(warnings, "watermark processing disabled by declaration; late-event gating is explicitly disabled")
	}
	for i, raw := range eventArray {
		path := fmt.Sprintf("$.events[%d]", i)
		obj, err := objectAt(raw, path)
		if err != nil {
			return nil, err
		}
		event, hasWatermark, err := parseEvent(obj, path, cfg, project)
		if err != nil {
			return nil, err
		}
		if !cfg.watermarkEnabled && hasWatermark {
			return nil, NewBusinessError(CodeSchemaAmbiguous, "watermark value present while watermark_enabled is false").
				WithContext("path", path+"."+cfg.eventKeys.watermark).WithContext("event_seq", fmt.Sprint(event.Seq))
		}
		if cfg.watermarkEnabled && !hasWatermark {
			return nil, NewBusinessError(CodeWatermarkMissing, "watermark is required when declaration enables it").
				WithContext("path", path+"."+cfg.eventKeys.watermark).
				WithContext("event_seq", fmt.Sprint(event.Seq)).WithContext("feature_id", event.FeatureID)
		}
		project.Events = append(project.Events, event)
	}
	project.SortEvents()
	if err := validateEventSequence(project); err != nil {
		return nil, err
	}
	if !cfg.watermarkEnabled {
		for i := range project.Events {
			project.Events[i].WatermarkMs = nil
		}
	}
	return &LoadedDocument{Version: cfg.version, Project: project, Warnings: warnings}, nil
}

func parseFeature(obj map[string]any, path string, cfg commonLoad) (FeatureDefinition, error) {
	if err := checkAliases(obj, path,
		[]string{"feature_id", "featureId", "id"},
		[]string{"window_width_ms", "windowWidth", "win_ms"},
		[]string{"window_step_ms", "windowStep", "stride_ms"},
		[]string{"allowed_lateness_ms", "allowedLateness", "late_allow_ms"},
		[]string{"aggregations", "aggs"},
		[]string{"derived_from", "derivedFrom", "parent_features"},
	); err != nil {
		return FeatureDefinition{}, err
	}
	id, err := requiredString(obj, path, cfg.featureKeys.id)
	if err != nil {
		return FeatureDefinition{}, err
	}
	name, _ := optionalString(obj, path, "name")
	width, err := requiredInt(obj, path, cfg.featureKeys.width)
	if err != nil {
		return FeatureDefinition{}, err
	}
	if width <= 0 {
		return FeatureDefinition{}, NewBusinessError(CodeFeatureWindowWidth, "window width must be positive").
			WithContext("feature_id", id).WithContext("path", path+"."+cfg.featureKeys.width).
			WithContext("value", fmt.Sprint(width))
	}
	step, err := requiredInt(obj, path, cfg.featureKeys.step)
	if err != nil {
		return FeatureDefinition{}, err
	}
	if step <= 0 {
		return FeatureDefinition{}, NewBusinessError(CodeFeatureStepZero, "window step must be positive").
			WithContext("feature_id", id).WithContext("path", path+"."+cfg.featureKeys.step).
			WithContext("value", fmt.Sprint(step))
	}
	if width < step {
		return FeatureDefinition{}, NewBusinessError(CodeFeatureWidthBelowStep, "window width must be at least the step").
			WithContext("feature_id", id).WithContext("width", fmt.Sprint(width)).
			WithContext("step", fmt.Sprint(step))
	}
	lateness, err := requiredInt(obj, path, cfg.featureKeys.lateness)
	if err != nil {
		return FeatureDefinition{}, err
	}
	if lateness < 0 {
		return FeatureDefinition{}, NewBusinessError(CodeFeatureStepZero, "allowed lateness must not be negative").
			WithContext("feature_id", id).WithContext("path", path+"."+cfg.featureKeys.lateness).
			WithContext("value", fmt.Sprint(lateness))
	}
	aggsRaw, _, err := pickAt(obj, path, cfg.featureKeys.aggs)
	if err != nil || aggsRaw == nil {
		if err == nil {
			err = NewBusinessError(CodeSchemaUnsupported, "aggregations are missing").
				WithContext("feature_id", id).WithContext("path", path+"."+cfg.featureKeys.aggs)
		}
		return FeatureDefinition{}, err
	}
	aggArray, err := arrayAt(aggsRaw, path+"."+cfg.featureKeys.aggs)
	if err != nil {
		return FeatureDefinition{}, err
	}
	feature := FeatureDefinition{
		ID: id, Name: name, WindowWidthMs: width, WindowStepMs: step,
		AllowedLatenessMs: lateness,
	}
	for i, raw := range aggArray {
		aggPath := fmt.Sprintf("%s.%s[%d]", path, cfg.featureKeys.aggs, i)
		aggObj, err := objectAt(raw, aggPath)
		if err != nil {
			return FeatureDefinition{}, err
		}
		if err := checkAliases(aggObj, aggPath,
			[]string{"function", "fn", "kind"},
			[]string{"weight_permille", "weightPerMyriad", "weight"},
		); err != nil {
			return FeatureDefinition{}, err
		}
		kindRaw, kindKey, err := pickAt(aggObj, aggPath, cfg.aggKeys.kind)
		if err != nil {
			return FeatureDefinition{}, err
		}
		if kindKey == "" {
			return FeatureDefinition{}, NewBusinessError(CodeAggFunctionUnknown, "aggregate kind is missing").
				WithContext("feature_id", id).WithContext("path", aggPath)
		}
		kindString, err := scalarString(kindRaw, aggPath+"."+kindKey)
		if err != nil {
			return FeatureDefinition{}, err
		}
		kind := AggKind(strings.ToLower(kindString))
		if !kind.Valid() {
			return FeatureDefinition{}, NewBusinessError(CodeAggFunctionUnknown, "unknown aggregate function %q", kindString).
				WithContext("feature_id", id).WithContext("path", aggPath)
		}
		weightRaw, weightKey, err := pickAt(aggObj, aggPath, cfg.aggKeys.weight)
		if err != nil {
			return FeatureDefinition{}, err
		}
		if weightKey == "" {
			return FeatureDefinition{}, NewBusinessError(CodeAggWeightOutOfRange, "aggregate weight is missing").
				WithContext("feature_id", id).WithContext("path", aggPath)
		}
		rawWeight, err := scalarInt(weightRaw, aggPath+"."+weightKey)
		if err != nil {
			return FeatureDefinition{}, err
		}
		normalized, err := normalizeWeight(rawWeight, cfg.weightScale)
		if err != nil {
			return FeatureDefinition{}, err.(*BusinessError).
				WithContext("feature_id", id).WithContext("path", aggPath+"."+weightKey).
				WithContext("weight_scale", string(cfg.weightScale))
		}
		feature.Aggregations = append(feature.Aggregations, Aggregation{Kind: kind, Weight: normalized})
	}
	if len(feature.Aggregations) == 0 {
		return FeatureDefinition{}, NewBusinessError(CodeSchemaUnsupported, "feature needs at least one aggregate").
			WithContext("feature_id", id).WithContext("path", path)
	}
	if derived, present, err := optionalStringArray(obj, path, cfg.featureKeys.derived); err != nil {
		return FeatureDefinition{}, err
	} else if present {
		feature.DerivedFrom = derived
	}
	return feature, nil
}

func parseEvent(obj map[string]any, path string, cfg commonLoad, project *Project) (ObservationEvent, bool, error) {
	if err := checkAliases(obj, path,
		[]string{"arrival_seq", "arrivalSeq", "seq"},
		[]string{"feature_id", "featureId", "feature"},
		[]string{"observed_at_ms", "observedTick", "epoch_ms"},
		[]string{"channel", "channelId"},
		[]string{"value", "sample", "v"},
		[]string{"watermark_ms", "watermarkTick", "wm_ms"},
	); err != nil {
		return ObservationEvent{}, false, err
	}
	seq, err := requiredInt(obj, path, cfg.eventKeys.seq)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	if seq <= 0 {
		return ObservationEvent{}, false, NewBusinessError(CodeEventSequenceInvalid, "event sequence must be one-based").
			WithContext("path", path+"."+cfg.eventKeys.seq).WithContext("value", fmt.Sprint(seq))
	}
	featureID, err := requiredString(obj, path, cfg.eventKeys.feature)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	if _, ok := project.FeatureByID(featureID); !ok {
		return ObservationEvent{}, false, NewBusinessError(CodeEventUnknownFeature, "event references unknown feature %q", featureID).
			WithContext("path", path+"."+cfg.eventKeys.feature).WithContext("event_seq", fmt.Sprint(seq))
	}
	tsRaw, tsKey, err := pickAt(obj, path, cfg.eventKeys.timestamp)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	if tsKey == "" {
		return ObservationEvent{}, false, NewBusinessError(CodeTimeBasisMissing, "event timestamp field is missing").
			WithContext("path", path+"."+cfg.eventKeys.timestamp).WithContext("version", cfg.version)
	}
	rawTS, err := scalarInt(tsRaw, path+"."+tsKey)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	var observed int64
	switch cfg.timeMode {
	case timeEpoch:
		observed = rawTS
	case timeRelativeTick:
		if rawTS > math.MaxInt64/abs64(project.TimeTickMs) || rawTS < math.MinInt64/abs64(project.TimeTickMs) {
			return ObservationEvent{}, false, NewBusinessError(CodeTimeBasisMissing, "relative event tick overflows").
				WithContext("path", path+"."+tsKey)
		}
		observed = project.TimeOriginMs + rawTS*project.TimeTickMs
	default:
		return ObservationEvent{}, false, NewBusinessError(CodeTimeBasisMissing, "unknown internal time mode").
			WithContext("path", path)
	}
	channel, err := requiredString(obj, path, cfg.eventKeys.channel)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	value, err := requiredInt(obj, path, cfg.eventKeys.value)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	event := ObservationEvent{Seq: seq, FeatureID: featureID, ObservedAtMs: observed, Channel: channel, Value: value}
	wmRaw, wmKey, err := pickAt(obj, path, cfg.eventKeys.watermark)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	if wmKey == "" {
		return event, false, nil
	}
	rawWM, err := scalarInt(wmRaw, path+"."+wmKey)
	if err != nil {
		return ObservationEvent{}, false, err
	}
	var wm int64
	switch cfg.timeMode {
	case timeEpoch:
		wm = rawWM
	case timeRelativeTick:
		wm = project.TimeOriginMs + rawWM*project.TimeTickMs
	}
	event.WatermarkMs = &wm
	return event, true, nil
}

func validateEventSequence(p *Project) error {
	if len(p.Events) == 0 {
		return NewBusinessError(CodeEventSequenceInvalid, "event stream is empty").WithContext("project_id", p.ProjectID)
	}
	for i, event := range p.Events {
		want := int64(i + 1)
		if event.Seq != want {
			return NewBusinessError(CodeEventSequenceInvalid, "event sequence must be contiguous and start at 1").
				WithContext("index", fmt.Sprint(i)).WithContext("got", fmt.Sprint(event.Seq)).
				WithContext("want", fmt.Sprint(want))
		}
	}
	return nil
}

func normalizeWeight(raw int64, scale WeightScale) (int64, error) {
	var divisor int64
	switch scale {
	case WeightPerMille:
		divisor = 1_000
	case WeightPerMyriad:
		divisor = 10_000
	case WeightPPM:
		divisor = 1_000_000
	default:
		return 0, NewBusinessError(CodeAggWeightOutOfRange, "unknown weight scale %q", scale)
	}
	if raw < 0 || raw > divisor {
		return 0, NewBusinessError(CodeAggWeightOutOfRange, "aggregate weight %d is outside [0,%d]", raw, divisor).
			WithContext("value", fmt.Sprint(raw))
	}
	if raw == 0 {
		return 0, NewBusinessError(CodeAggWeightOutOfRange, "aggregate weight must be greater than zero").
			WithContext("value", "0")
	}
	return raw * (NormalizedWeightScale / divisor), nil
}

func ambiguousCombination(version, timeExpression, alignment, path string) error {
	return NewBusinessError(CodeSchemaAmbiguous, "cannot disambiguate %s schema with %s time and %q alignment", version, timeExpression, alignment).
		WithContext("version", version).WithContext("path", path).
		WithContext("time_expression", timeExpression).WithContext("alignment", alignment)
}

func projectIDKey(version string) string {
	if version == "v2" {
		return "projectId"
	}
	return "project_id"
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func objectAt(raw any, path string) (map[string]any, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, NewBusinessError(CodeSchemaUnsupported, "expected JSON object").
			WithContext("path", path).WithContext("got_type", fmt.Sprintf("%T", raw))
	}
	return obj, nil
}

func arrayAt(raw any, path string) ([]any, error) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, NewBusinessError(CodeSchemaUnsupported, "expected JSON array").
			WithContext("path", path).WithContext("got_type", fmt.Sprintf("%T", raw))
	}
	return arr, nil
}

// pickAt reads exactly one alias. Multiple aliases with different values are
// reported as ambiguous instead of silently preferring one.
func pickAt(obj map[string]any, path string, keys ...string) (any, string, error) {
	var selectedKey string
	var selected any
	for _, key := range keys {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if selectedKey == "" {
			selectedKey, selected = key, value
			continue
		}
		if !reflect.DeepEqual(selected, value) {
			return nil, "", NewBusinessError(CodeSchemaAmbiguous, "fields %q and %q disagree", selectedKey, key).
				WithContext("path", path).WithContext("field_a", selectedKey).WithContext("field_b", key)
		}
	}
	return selected, selectedKey, nil
}

func checkAliases(obj map[string]any, path string, groups ...[]string) error {
	for _, keys := range groups {
		_, _, err := pickAt(obj, path, keys...)
		if err != nil {
			return err
		}
	}
	return nil
}

func requiredString(obj map[string]any, path, key string) (string, error) {
	value, found, err := pickAt(obj, path, key)
	if err != nil {
		return "", err
	}
	if found == "" || value == nil {
		return "", NewBusinessError(CodeSchemaUnsupported, "required field %q is missing", key).
			WithContext("path", path+"."+key)
	}
	return scalarString(value, path+"."+key)
}

func optionalString(obj map[string]any, path, key string) (string, bool) {
	value, found, err := pickAt(obj, path, key)
	if err != nil || found == "" || value == nil {
		return "", false
	}
	s, err := scalarString(value, path+"."+key)
	if err != nil {
		return "", false
	}
	return s, true
}

func requiredInt(obj map[string]any, path, key string) (int64, error) {
	value, found, err := pickAt(obj, path, key)
	if err != nil {
		return 0, err
	}
	if found == "" || value == nil {
		return 0, NewBusinessError(CodeSchemaUnsupported, "required field %q is missing", key).
			WithContext("path", path+"."+key)
	}
	return scalarInt(value, path+"."+key)
}

func requiredTimeInt(obj map[string]any, path, key string) (int64, error) {
	value, err := requiredInt(obj, path, key)
	if err == nil {
		return value, nil
	}
	if be, ok := AsBusinessError(err); ok && be.Code == CodeSchemaUnsupported {
		return 0, NewBusinessError(CodeTimeBasisMissing, "time basis field %q is missing or invalid", key).
			WithContext("path", path+"."+key)
	}
	return 0, err
}

func optionalBool(obj map[string]any, path, key string) (bool, error) {
	value, found, err := pickAt(obj, path, key)
	if err != nil {
		return false, err
	}
	if found == "" || value == nil {
		return false, nil
	}
	v, ok := value.(bool)
	if !ok {
		return false, NewBusinessError(CodeSchemaUnsupported, "expected boolean").
			WithContext("path", path+"."+key).WithContext("got_type", fmt.Sprintf("%T", value))
	}
	return v, nil
}

func optionalStringArray(obj map[string]any, path, key string) ([]string, bool, error) {
	value, found, err := pickAt(obj, path, key)
	if err != nil {
		return nil, false, err
	}
	if found == "" || value == nil {
		return nil, false, nil
	}
	raw, err := arrayAt(value, path+"."+key)
	if err != nil {
		return nil, true, err
	}
	out := make([]string, 0, len(raw))
	for i, item := range raw {
		s, err := scalarString(item, fmt.Sprintf("%s.%s[%d]", path, key, i))
		if err != nil {
			return nil, true, err
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, true, nil
}

func scalarString(value any, path string) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", NewBusinessError(CodeSchemaUnsupported, "expected string").
			WithContext("path", path).WithContext("got_type", fmt.Sprintf("%T", value))
	}
	return s, nil
}

func scalarInt(value any, path string) (int64, error) {
	switch v := value.(type) {
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, NewBusinessError(CodeTimeBasisMissing, "expected integral number: %v", err).
				WithContext("path", path).WithContext("value", v.String())
		}
		return i, nil
	case float64:
		if math.Trunc(v) != v || v > math.MaxInt64 || v < math.MinInt64 {
			return 0, NewBusinessError(CodeTimeBasisMissing, "expected integral number").
				WithContext("path", path).WithContext("value", fmt.Sprint(v))
		}
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	default:
		return 0, NewBusinessError(CodeSchemaUnsupported, "expected integer").
			WithContext("path", path).WithContext("got_type", fmt.Sprintf("%T", value))
	}
}

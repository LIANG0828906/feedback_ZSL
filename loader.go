package layercache

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LoadProject loads any of the three supported schema versions and maps it to
// the same normalized domain model. The input byte slice is never modified.
func LoadProject(raw []byte) (*Project, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, bizError(CodeJSONInvalid, "", "$", "invalid JSON: %v", err)
	}
	var trailing any
	if dec.Decode(&trailing) == nil {
		return nil, bizError(CodeJSONInvalid, "", "$", "multiple JSON values")
	}
	obj, err := asObject(root, "$", "")
	if err != nil {
		return nil, err
	}
	version, err := detectVersion(obj)
	if err != nil {
		return nil, err
	}
	var project *Project
	switch version {
	case "v1":
		project, err = loadV1(obj)
	case "v2":
		project, err = loadV2(obj)
	case "v3":
		project, err = loadV3(obj)
	default:
		err = bizError(CodeVersionUnknown, version, "$", "unsupported schema version")
	}
	if err != nil {
		return nil, err
	}
	project.SourceHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	sort.SliceStable(project.Layers, func(i, j int) bool { return project.Layers[i].DeclOrder < project.Layers[j].DeclOrder })
	sort.SliceStable(project.Objects, func(i, j int) bool { return project.Objects[i].DeclOrder < project.Objects[j].DeclOrder })
	sort.SliceStable(project.Events, func(i, j int) bool {
		if project.Events[i].AtMS != project.Events[j].AtMS {
			return project.Events[i].AtMS < project.Events[j].AtMS
		}
		return project.Events[i].Order < project.Events[j].Order
	})
	if err := project.Validate(); err != nil {
		return nil, err
	}
	return project, nil
}

func detectVersion(root map[string]any) (string, error) {
	type marker struct {
		key   string
		value string
	}
	var markers []marker
	for _, key := range []string{"schemaVersion", "schema_version", "schema"} {
		if v, ok := root[key]; ok {
			s, ok := v.(string)
			if !ok {
				return "", bizError(CodeFieldType, "", key, "schema version must be a string")
			}
			markers = append(markers, marker{key, s})
		}
	}
	if len(markers) == 0 {
		return "", bizError(CodeVersionMissing, "", "$", "no schema version marker")
	}
	version := markers[0].value
	for _, m := range markers[1:] {
		if m.value != version {
			return "", bizError(CodeAmbiguousInput, version, m.key, "conflicting schema version markers")
		}
	}
	switch version {
	case "v1", "v2", "v3":
		return version, nil
	default:
		return "", bizError(CodeVersionUnknown, version, markers[0].key, "unknown schema version %q", version)
	}
}

func loadV1(root map[string]any) (*Project, error) {
	version := "v1"
	if _, ok := root["config"]; !ok {
		if _, hasSettings := root["settings"]; hasSettings {
			return nil, bizError(CodeVersionMismatch, version, "config", "v1 marker paired with v2 layout")
		}
		return nil, bizError(CodeFieldMissing, version, "config", "missing v1 config object")
	}
	if _, hasPolicy := root["policy"]; hasPolicy {
		return nil, bizError(CodeVersionMismatch, version, "policy", "v1 marker paired with v3 layout")
	}
	cfgMap, err := requireObject(root, "config", "$", version)
	if err != nil {
		return nil, err
	}
	seed, err := requireInt(root, "seed", "seed", version, 0)
	if err != nil {
		return nil, err
	}
	tickMS, err := requireInt(cfgMap, "tickMillis", "config.tickMillis", version, 0)
	if err != nil {
		return nil, err
	}
	capacityUnit, err := requireString(cfgMap, "capacityUnit", "config.capacityUnit", version)
	if err != nil {
		return nil, err
	}
	timeUnit, err := requireString(cfgMap, "timeUnit", "config.timeUnit", version)
	if err != nil {
		return nil, err
	}
	heatScale, err := requireInt(cfgMap, "heatScale", "config.heatScale", version, 0)
	if err != nil {
		return nil, err
	}
	migrationScale, err := requireInt(cfgMap, "migrationScale", "config.migrationScale", version, 0)
	if err != nil {
		return nil, err
	}
	fixedBits, err := requireInt(cfgMap, "fixedBits", "config.fixedBits", version, 0)
	if err != nil {
		return nil, err
	}
	windowTicks, err := requireInt(cfgMap, "heatWindowTicks", "config.heatWindowTicks", version, 0)
	if err != nil {
		return nil, err
	}
	decayTicks, err := requireInt(cfgMap, "heatDecayTicks", "config.heatDecayTicks", version, 0)
	if err != nil {
		return nil, err
	}
	maxCount, err := requireInt(cfgMap, "heatMaxCount", "config.heatMaxCount", version, 0)
	if err != nil {
		return nil, err
	}
	hotRaw, err := requireInt(cfgMap, "hotThreshold", "config.hotThreshold", version, 0)
	if err != nil {
		return nil, err
	}
	coldRaw, err := requireInt(cfgMap, "coldThreshold", "config.coldThreshold", version, 0)
	if err != nil {
		return nil, err
	}
	migHotRaw, err := requireInt(cfgMap, "migrationHotThreshold", "config.migrationHotThreshold", version, 0)
	if err != nil {
		return nil, err
	}
	migColdRaw, err := requireInt(cfgMap, "migrationColdThreshold", "config.migrationColdThreshold", version, 0)
	if err != nil {
		return nil, err
	}
	batchObjects, err := requireInt(cfgMap, "batchMaxObjects", "config.batchMaxObjects", version, 0)
	if err != nil {
		return nil, err
	}
	batchBytes, err := requireInt(cfgMap, "batchMaxBytes", "config.batchMaxBytes", version, 0)
	if err != nil {
		return nil, err
	}
	workers, err := requireInt(cfgMap, "workers", "config.workers", version, 0)
	if err != nil {
		return nil, err
	}
	buffer, err := requireInt(cfgMap, "perLayerBuffer", "config.perLayerBuffer", version, 0)
	if err != nil {
		return nil, err
	}
	if tickMS <= 0 {
		return nil, bizError(CodeInvalidScale, version, "config.tickMillis", "tick duration must be positive")
	}
	capUnit, err := parseCapacityUnit(capacityUnit, version, "config.capacityUnit")
	if err != nil {
		return nil, err
	}
	if err := expectTimeUnit(timeUnit, "tick", version, "config.timeUnit"); err != nil {
		return nil, err
	}
	if err := validateScale(int(heatScale), version, "config.heatScale"); err != nil {
		return nil, err
	}
	if err := validateScale(int(migrationScale), version, "config.migrationScale"); err != nil {
		return nil, err
	}
	hot, err := scaleToBasisPoints(hotRaw, int(heatScale), version, "config.hotThreshold")
	if err != nil {
		return nil, err
	}
	cold, err := scaleToBasisPoints(coldRaw, int(heatScale), version, "config.coldThreshold")
	if err != nil {
		return nil, err
	}
	migHot, err := scaleToBasisPoints(migHotRaw, int(migrationScale), version, "config.migrationHotThreshold")
	if err != nil {
		return nil, err
	}
	migCold, err := scaleToBasisPoints(migColdRaw, int(migrationScale), version, "config.migrationColdThreshold")
	if err != nil {
		return nil, err
	}
	cfg := Config{
		TimeUnit: "tick", CapacityUnit: capUnit, HeatScale: int(heatScale), MigrationScale: int(migrationScale),
		WatermarkMode:    WatermarkProvided,
		TickMS:           tickMS,
		FixedBits:        int(fixedBits),
		HeatWindowMS:     mulInt(windowTicks, tickMS),
		HeatDecayMS:      mulInt(decayTicks, tickMS),
		HeatMaxCount:     maxCount,
		HotThreshold:     hot,
		ColdThreshold:    cold,
		MigrateHotLimit:  migHot,
		MigrateColdLimit: migCold,
		BatchMaxObjects:  int(batchObjects),
		BatchMaxBytes:    batchBytes,
		Workers:          int(workers),
		PerLayerBuffer:   int(buffer),
	}
	return buildProject(version, seed, cfg, root)
}

func loadV2(root map[string]any) (*Project, error) {
	version := "v2"
	if _, ok := root["settings"]; !ok {
		if _, hasConfig := root["config"]; hasConfig {
			return nil, bizError(CodeVersionMismatch, version, "settings", "v2 marker paired with v1 layout")
		}
		return nil, bizError(CodeFieldMissing, version, "settings", "missing v2 settings object")
	}
	settings, err := requireObject(root, "settings", "$", version)
	if err != nil {
		return nil, err
	}
	seed, err := requireInt(root, "seed", "seed", version, 0)
	if err != nil {
		return nil, err
	}
	timeMap, err := requireObject(settings, "time", "settings.time", version)
	if err != nil {
		return nil, err
	}
	timeUnit, err := requireString(timeMap, "unit", "settings.time.unit", version)
	if err != nil {
		return nil, err
	}
	if err := expectTimeUnit(timeUnit, "ms", version, "settings.time.unit"); err != nil {
		return nil, err
	}
	tickMS, err := requireInt(timeMap, "tick_ms", "settings.time.tick_ms", version, 0)
	if err != nil {
		return nil, err
	}
	capMap, err := requireObject(settings, "capacity", "settings.capacity", version)
	if err != nil {
		return nil, err
	}
	capUnitRaw, err := requireString(capMap, "unit", "settings.capacity.unit", version)
	if err != nil {
		return nil, err
	}
	capUnit, err := parseCapacityUnit(capUnitRaw, version, "settings.capacity.unit")
	if err != nil {
		return nil, err
	}
	heatMap, err := requireObject(settings, "heat", "settings.heat", version)
	if err != nil {
		return nil, err
	}
	heatScale, err := requireInt(heatMap, "scale", "settings.heat.scale", version, 0)
	if err != nil {
		return nil, err
	}
	migMap, err := requireObject(settings, "migration", "settings.migration", version)
	if err != nil {
		return nil, err
	}
	migScale, err := requireInt(migMap, "scale", "settings.migration.scale", version, 0)
	if err != nil {
		return nil, err
	}
	windowMS, err := requireInt(heatMap, "window_ms", "settings.heat.window_ms", version, 0)
	if err != nil {
		return nil, err
	}
	decayMS, err := requireInt(heatMap, "decay_ms", "settings.heat.decay_ms", version, 0)
	if err != nil {
		return nil, err
	}
	maxCount, err := requireInt(heatMap, "max_count", "settings.heat.max_count", version, 0)
	if err != nil {
		return nil, err
	}
	hotRaw, err := requireInt(heatMap, "hot_threshold", "settings.heat.hot_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	coldRaw, err := requireInt(heatMap, "cold_threshold", "settings.heat.cold_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	migHotRaw, err := requireInt(migMap, "hot_threshold", "settings.migration.hot_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	migColdRaw, err := requireInt(migMap, "cold_threshold", "settings.migration.cold_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	batchObjects, err := requireInt(migMap, "max_objects", "settings.migration.max_objects", version, 0)
	if err != nil {
		return nil, err
	}
	batchBytes, err := requireInt(migMap, "max_bytes", "settings.migration.max_bytes", version, 0)
	if err != nil {
		return nil, err
	}
	execMap, err := requireObject(settings, "execution", "settings.execution", version)
	if err != nil {
		return nil, err
	}
	workers, err := requireInt(execMap, "workers", "settings.execution.workers", version, 0)
	if err != nil {
		return nil, err
	}
	buffer, err := requireInt(execMap, "per_layer_buffer", "settings.execution.per_layer_buffer", version, 0)
	if err != nil {
		return nil, err
	}
	fixedBits, err := requireInt(settings, "fixed_bits", "settings.fixed_bits", version, 0)
	if err != nil {
		return nil, err
	}
	if tickMS <= 0 {
		return nil, bizError(CodeInvalidScale, version, "settings.time.tick_ms", "tick duration must be positive")
	}
	if err := validateScale(int(heatScale), version, "settings.heat.scale"); err != nil {
		return nil, err
	}
	if err := validateScale(int(migScale), version, "settings.migration.scale"); err != nil {
		return nil, err
	}
	hot, err := scaleToBasisPoints(hotRaw, int(heatScale), version, "settings.heat.hot_threshold")
	if err != nil {
		return nil, err
	}
	cold, err := scaleToBasisPoints(coldRaw, int(heatScale), version, "settings.heat.cold_threshold")
	if err != nil {
		return nil, err
	}
	migHot, err := scaleToBasisPoints(migHotRaw, int(migScale), version, "settings.migration.hot_threshold")
	if err != nil {
		return nil, err
	}
	migCold, err := scaleToBasisPoints(migColdRaw, int(migScale), version, "settings.migration.cold_threshold")
	if err != nil {
		return nil, err
	}
	cfg := Config{
		TimeUnit: "ms", CapacityUnit: capUnit, HeatScale: int(heatScale), MigrationScale: int(migScale),
		WatermarkMode:    WatermarkDerived,
		TickMS:           tickMS,
		FixedBits:        int(fixedBits),
		HeatWindowMS:     windowMS,
		HeatDecayMS:      decayMS,
		HeatMaxCount:     maxCount,
		HotThreshold:     hot,
		ColdThreshold:    cold,
		MigrateHotLimit:  migHot,
		MigrateColdLimit: migCold,
		BatchMaxObjects:  int(batchObjects),
		BatchMaxBytes:    batchBytes,
		Workers:          int(workers),
		PerLayerBuffer:   int(buffer),
	}
	return buildProject(version, seed, cfg, root)
}

func loadV3(root map[string]any) (*Project, error) {
	version := "v3"
	if _, ok := root["policy"]; !ok {
		if _, hasConfig := root["config"]; hasConfig {
			return nil, bizError(CodeVersionMismatch, version, "policy", "v3 marker paired with v1 layout")
		}
		if _, hasSettings := root["settings"]; hasSettings {
			return nil, bizError(CodeVersionMismatch, version, "policy", "v3 marker paired with v2 layout")
		}
		return nil, bizError(CodeFieldMissing, version, "policy", "missing v3 policy object")
	}
	policy, err := requireObject(root, "policy", "$", version)
	if err != nil {
		return nil, err
	}
	seed, err := requireInt(root, "seed", "seed", version, 0)
	if err != nil {
		return nil, err
	}
	capacityUnit, err := requireString(policy, "capacity_unit", "policy.capacity_unit", version)
	if err != nil {
		return nil, err
	}
	timeUnit, err := requireString(policy, "time_unit", "policy.time_unit", version)
	if err != nil {
		return nil, err
	}
	heatScale, err := requireInt(policy, "heat_scale", "policy.heat_scale", version, 0)
	if err != nil {
		return nil, err
	}
	migScale, err := requireInt(policy, "migration_scale", "policy.migration_scale", version, 0)
	if err != nil {
		return nil, err
	}
	tickMS, err := requireInt(policy, "tick_ms", "policy.tick_ms", version, 0)
	if err != nil {
		return nil, err
	}
	fixedBits, err := requireInt(policy, "fixed_bits", "policy.fixed_bits", version, 0)
	if err != nil {
		return nil, err
	}
	windowMS, err := requireInt(policy, "heat_window_ms", "policy.heat_window_ms", version, 0)
	if err != nil {
		return nil, err
	}
	decayMS, err := requireInt(policy, "heat_decay_ms", "policy.heat_decay_ms", version, 0)
	if err != nil {
		return nil, err
	}
	maxCount, err := requireInt(policy, "heat_max_count", "policy.heat_max_count", version, 0)
	if err != nil {
		return nil, err
	}
	hotRaw, err := requireInt(policy, "hot_threshold", "policy.hot_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	coldRaw, err := requireInt(policy, "cold_threshold", "policy.cold_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	migHotRaw, err := requireInt(policy, "migration_hot_threshold", "policy.migration_hot_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	migColdRaw, err := requireInt(policy, "migration_cold_threshold", "policy.migration_cold_threshold", version, 0)
	if err != nil {
		return nil, err
	}
	batchObjects, err := requireInt(policy, "batch_max_objects", "policy.batch_max_objects", version, 0)
	if err != nil {
		return nil, err
	}
	batchBytes, err := requireInt(policy, "batch_max_bytes", "policy.batch_max_bytes", version, 0)
	if err != nil {
		return nil, err
	}
	workers, err := requireInt(policy, "workers", "policy.workers", version, 0)
	if err != nil {
		return nil, err
	}
	buffer, err := requireInt(policy, "per_layer_buffer", "policy.per_layer_buffer", version, 0)
	if err != nil {
		return nil, err
	}
	watermarkPolicy, err := requireString(policy, "watermark_policy", "policy.watermark_policy", version)
	if err != nil {
		return nil, err
	}
	if watermarkPolicy != string(WatermarkProvided) {
		return nil, bizError(CodeVersionMismatch, version, "policy.watermark_policy", "v3 declares provided watermarks")
	}
	if tickMS <= 0 {
		return nil, bizError(CodeInvalidScale, version, "policy.tick_ms", "tick duration must be positive")
	}
	if err := expectTimeUnit(timeUnit, "ms", version, "policy.time_unit"); err != nil {
		return nil, err
	}
	capUnit, err := parseCapacityUnit(capacityUnit, version, "policy.capacity_unit")
	if err != nil {
		return nil, err
	}
	if err := validateScale(int(heatScale), version, "policy.heat_scale"); err != nil {
		return nil, err
	}
	if err := validateScale(int(migScale), version, "policy.migration_scale"); err != nil {
		return nil, err
	}
	hot, err := scaleToBasisPoints(hotRaw, int(heatScale), version, "policy.hot_threshold")
	if err != nil {
		return nil, err
	}
	cold, err := scaleToBasisPoints(coldRaw, int(heatScale), version, "policy.cold_threshold")
	if err != nil {
		return nil, err
	}
	migHot, err := scaleToBasisPoints(migHotRaw, int(migScale), version, "policy.migration_hot_threshold")
	if err != nil {
		return nil, err
	}
	migCold, err := scaleToBasisPoints(migColdRaw, int(migScale), version, "policy.migration_cold_threshold")
	if err != nil {
		return nil, err
	}
	cfg := Config{
		TimeUnit: "ms", CapacityUnit: capUnit, HeatScale: int(heatScale), MigrationScale: int(migScale),
		WatermarkMode: WatermarkProvided, TickMS: tickMS, FixedBits: int(fixedBits),
		HeatWindowMS: windowMS, HeatDecayMS: decayMS, HeatMaxCount: maxCount,
		HotThreshold: hot, ColdThreshold: cold, MigrateHotLimit: migHot, MigrateColdLimit: migCold,
		BatchMaxObjects: int(batchObjects), BatchMaxBytes: batchBytes,
		Workers: int(workers), PerLayerBuffer: int(buffer),
	}
	return buildProject(version, seed, cfg, root)
}

// buildProject maps the version-specific collection layout into the canonical
// model. It is shared only where the already-normalized field names match.
func buildProject(version string, seed int64, cfg Config, root map[string]any) (*Project, error) {
	p := &Project{SchemaVersion: version, Seed: seed, Config: cfg}
	if version == "v2" {
		return p, buildV2Collections(p, root)
	}
	layersAny, ok := root["layers"]
	if !ok {
		return nil, bizError(CodeFieldMissing, version, "layers", "missing layers")
	}
	layerArr, ok := layersAny.([]any)
	if !ok {
		return nil, bizError(CodeFieldType, version, "layers", "layers must be an array for this version")
	}
	for i, value := range layerArr {
		path := fmt.Sprintf("layers[%d]", i)
		layerMap, err := asObject(value, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requireString(layerMap, "id", path+".id", version)
		if err != nil {
			return nil, err
		}
		var kindRaw string
		if version == "v1" {
			kindRaw, err = requireString(layerMap, "tier", path+".tier", version)
		} else {
			kindRaw, err = requireString(layerMap, "kind", path+".kind", version)
		}
		if err != nil {
			return nil, err
		}
		kind, err := parseLayerKind(kindRaw, version, path+".tier")
		if err != nil {
			return nil, err
		}
		var capacityRaw int64
		var capacityField string
		if version == "v1" {
			capacityRaw, err = requireInt(layerMap, "capacity", path+".capacity", version, 0)
			capacityField = path + ".capacity"
		} else {
			capacityRaw, err = requireInt(layerMap, "capacity_bytes", path+".capacity_bytes", version, 0)
			capacityField = path + ".capacity_bytes"
		}
		if err != nil {
			return nil, err
		}
		capacity, err := unitToBytes(capacityRaw, cfg.CapacityUnit, version, capacityField)
		if err != nil {
			return nil, err
		}
		allowed, err := parseAllowed(layerMap, version, path)
		if err != nil {
			return nil, err
		}
		declOrder, err := optionalInt(layerMap, "order", path+".order", version, int64(i))
		if err != nil {
			return nil, err
		}
		p.Layers = append(p.Layers, Layer{
			ID: id, Kind: kind, CapacityBytes: capacity, AllowedObjects: allowed, DeclOrder: int(declOrder),
		})
	}
	objectsAny, ok := root["objects"]
	if !ok {
		return nil, bizError(CodeFieldMissing, version, "objects", "missing objects")
	}
	objectArr, ok := objectsAny.([]any)
	if !ok {
		return nil, bizError(CodeFieldType, version, "objects", "objects must be a flat array for this version")
	}
	for i, value := range objectArr {
		path := fmt.Sprintf("objects[%d]", i)
		objectMap, err := asObject(value, path, version)
		if err != nil {
			return nil, err
		}
		o, err := parseObject(objectMap, version, path, cfg, int64(i), "")
		if err != nil {
			return nil, err
		}
		p.Objects = append(p.Objects, o)
	}
	if err := parseEventsInto(p, root, version, cfg, version != "v1"); err != nil {
		return nil, err
	}
	return p, nil
}

func buildV2Collections(p *Project, root map[string]any) error {
	version := "v2"
	layersAny, ok := root["layers"]
	if !ok {
		return bizError(CodeFieldMissing, version, "layers", "missing layers")
	}
	layersMap, ok := layersAny.(map[string]any)
	if !ok {
		return bizError(CodeFieldType, version, "layers", "v2 layers must be a tier-keyed object")
	}
	globalObjectOrder := int64(0)
	for _, tier := range []struct {
		key  string
		kind LayerKind
	}{{"hot", LayerHot}, {"cold", LayerCold}} {
		tierAny, ok := layersMap[tier.key]
		if !ok {
			return bizError(CodeFieldMissing, version, "layers."+tier.key, "missing tier")
		}
		tierArr, ok := tierAny.([]any)
		if !ok {
			return bizError(CodeFieldType, version, "layers."+tier.key, "tier must be an array")
		}
		for i, value := range tierArr {
			path := fmt.Sprintf("layers.%s[%d]", tier.key, i)
			layerMap, err := asObject(value, path, version)
			if err != nil {
				return err
			}
			id, err := requireString(layerMap, "layer_id", path+".layer_id", version)
			if err != nil {
				return err
			}
			capacityRaw, err := requireInt(layerMap, "capacity_kib", path+".capacity_kib", version, 0)
			if err != nil {
				return err
			}
			if capacityRaw < 0 {
				return bizError(CodeNegativeCapacity, version, path+".capacity_kib", "capacity must not be negative")
			}
			// v2 capacity is explicitly KiB, independent of the normalized config unit.
			capacity := mulInt(capacityRaw, 1024)
			allowed, err := parseAllowed(layerMap, version, path)
			if err != nil {
				return err
			}
			declOrder, err := optionalInt(layerMap, "order", path+".order", version, int64(len(p.Layers)))
			if err != nil {
				return err
			}
			objectsAny, ok := layerMap["objects"]
			if !ok {
				return bizError(CodeFieldMissing, version, path+".objects", "nested layer needs objects")
			}
			objectsArr, ok := objectsAny.([]any)
			if !ok {
				return bizError(CodeFieldType, version, path+".objects", "objects must be an array")
			}
			for j, objectValue := range objectsArr {
				objectPath := fmt.Sprintf("%s.objects[%d]", path, j)
				objectMap, err := asObject(objectValue, objectPath, version)
				if err != nil {
					return err
				}
				order, err := optionalInt(objectMap, "decl_order", objectPath+".decl_order", version, globalObjectOrder)
				if err != nil {
					return err
				}
				globalObjectOrder++
				obj, err := parseObject(objectMap, version, objectPath, p.Config, order, id)
				if err != nil {
					return err
				}
				p.Objects = append(p.Objects, obj)
			}
			p.Layers = append(p.Layers, Layer{
				ID: id, Kind: tier.kind, CapacityBytes: capacity, AllowedObjects: allowed, DeclOrder: int(declOrder),
			})
		}
	}
	if err := parseEventsInto(p, root, version, p.Config, true); err != nil {
		return err
	}
	return nil
}

func parseObject(objectMap map[string]any, version, path string, cfg Config, order int64, impliedLayer string) (Object, error) {
	id, err := requireString(objectMap, "id", path+".id", version)
	if err != nil {
		return Object{}, err
	}
	var sizeRaw int64
	switch version {
	case "v1":
		sizeRaw, err = requireInt(objectMap, "bytes", path+".bytes", version, 0)
	case "v2":
		sizeRaw, err = requireInt(objectMap, "size_kib", path+".size_kib", version, 0)
	default:
		sizeRaw, err = requireInt(objectMap, "size_bytes", path+".size_bytes", version, 0)
	}
	if err != nil {
		return Object{}, err
	}
	var size int64
	if version == "v2" {
		size = mulInt(sizeRaw, 1024)
	} else {
		size, err = unitToBytes(sizeRaw, cfg.CapacityUnit, version, path+".size")
		if err != nil {
			return Object{}, err
		}
	}
	if size <= 0 {
		return Object{}, bizError(CodeFieldType, version, path+".size", "size must be positive")
	}
	initialLayer := impliedLayer
	if initialLayer == "" {
		key := "initialLayer"
		if version == "v3" {
			key = "initial_layer"
		}
		initialLayer, err = requireString(objectMap, key, path+"."+key, version)
		if err != nil {
			return Object{}, err
		}
	}
	priority, err := optionalInt(objectMap, "priority", path+".priority", version, 0)
	if err != nil {
		return Object{}, err
	}
	var samplesAny any
	var samplesKey string
	switch version {
	case "v1":
		samplesKey = "accesses"
	case "v2":
		samplesKey = "accesses"
	default:
		samplesKey = "samples"
	}
	samplesAny, ok := objectMap[samplesKey]
	if !ok {
		return Object{}, bizError(CodeFieldMissing, version, path+"."+samplesKey, "missing access count sequence")
	}
	samplesArr, ok := samplesAny.([]any)
	if !ok {
		return Object{}, bizError(CodeFieldType, version, path+"."+samplesKey, "access sequence must be an array")
	}
	var samples []AccessSample
	for i, value := range samplesArr {
		samplePath := fmt.Sprintf("%s.%s[%d]", path, samplesKey, i)
		sampleMap, err := asObject(value, samplePath, version)
		if err != nil {
			return Object{}, err
		}
		var timeRaw, count int64
		var timeField string
		switch version {
		case "v1":
			timeRaw, err = requireInt(sampleMap, "tick", samplePath+".tick", version, 0)
			timeField = "tick"
		case "v2":
			timeRaw, err = requireInt(sampleMap, "at_ms", samplePath+".at_ms", version, 0)
			timeField = "at_ms"
		default:
			timeRaw, err = requireInt(sampleMap, "at_ms", samplePath+".at_ms", version, 0)
			timeField = "at_ms"
		}
		if err != nil {
			return Object{}, err
		}
		count, err = requireInt(sampleMap, "count", samplePath+".count", version, 0)
		if err != nil {
			return Object{}, err
		}
		var atMS int64
		if version == "v1" {
			atMS = mulInt(timeRaw, cfg.TickMS)
		} else {
			atMS = timeRaw
		}
		sample := AccessSample{AtMS: atMS, Count: count, SourceTick: timeRaw, SourceIndex: i}
		if cfg.WatermarkMode == WatermarkProvided {
			key := "watermark"
			if version == "v3" {
				key = "heat_watermark"
			}
			raw, err := requireInt(sampleMap, key, samplePath+"."+key, version, 0)
			if err != nil {
				return Object{}, err
			}
			wm, err := scaleToBasisPoints(raw, cfg.HeatScale, version, samplePath+"."+key)
			if err != nil {
				return Object{}, err
			}
			sample.Watermark = &wm
		} else {
			if _, has := sampleMap["watermark"]; has {
				return Object{}, bizError(CodeVersionMismatch, version, samplePath+".watermark", "version declares derived watermarks")
			}
			if _, has := sampleMap["heat_watermark"]; has {
				return Object{}, bizError(CodeVersionMismatch, version, samplePath+".heat_watermark", "version declares derived watermarks")
			}
		}
		_ = timeField
		samples = append(samples, sample)
	}
	return Object{
		ID: id, SizeBytes: size, InitialLayer: initialLayer, Priority: int(priority),
		DeclOrder: int(order), Samples: samples,
	}, nil
}

func parseEventsInto(p *Project, root map[string]any, version string, cfg Config, snake bool) error {
	eventsAny, ok := root["events"]
	if !ok {
		return bizError(CodeFieldMissing, version, "events", "missing events")
	}
	eventsArr, ok := eventsAny.([]any)
	if !ok {
		return bizError(CodeFieldType, version, "events", "events must be an array")
	}
	for i, value := range eventsArr {
		path := fmt.Sprintf("events[%d]", i)
		eventMap, err := asObject(value, path, version)
		if err != nil {
			return err
		}
		order, err := requireInt(eventMap, "order", path+".order", version, 0)
		if err != nil {
			return err
		}
		kindRaw, err := requireString(eventMap, "kind", path+".kind", version)
		if err != nil {
			return err
		}
		kind, err := parseEventKind(kindRaw, version, path+".kind")
		if err != nil {
			return err
		}
		timeKey, timeAlias := "at_ms", "at"
		if version == "v1" {
			timeKey, timeAlias = "tick", "at_ms"
		}
		timeRaw, err := requireInt(eventMap, timeKey, path+"."+timeKey, version, 0)
		if err != nil {
			if _, aliasOK := eventMap[timeAlias]; aliasOK {
				return bizError(CodeVersionMismatch, version, path+"."+timeKey, "event time spelling does not match schema version")
			}
			return err
		}
		atMS := timeRaw
		if version == "v1" {
			atMS = mulInt(timeRaw, cfg.TickMS)
		}
		ev := Event{Order: int(order), AtMS: atMS, Kind: kind}
		objectKey := "object"
		batchKey := "batch"
		sourceKey := "source"
		targetKey := "target"
		if snake {
			objectKey, batchKey, sourceKey, targetKey = "object_id", "batch_id", "source_layer", "target_layer"
		}
		if v, ok := eventMap[objectKey]; ok {
			ev.ObjectID, err = asString(v, path+"."+objectKey, version)
			if err != nil {
				return err
			}
		}
		if v, ok := eventMap[batchKey]; ok {
			ev.BatchID, err = asString(v, path+"."+batchKey, version)
			if err != nil {
				return err
			}
		}
		if v, ok := eventMap[sourceKey]; ok {
			ev.SourceLayer, err = asString(v, path+"."+sourceKey, version)
			if err != nil {
				return err
			}
		}
		if v, ok := eventMap[targetKey]; ok {
			ev.TargetLayer, err = asString(v, path+"."+targetKey, version)
			if err != nil {
				return err
			}
		}
		if v, ok := eventMap["count"]; ok {
			ev.Count, err = asInt(v, path+".count", version)
			if err != nil {
				return err
			}
		}
		p.Events = append(p.Events, ev)
	}
	return nil
}

func parseAllowed(layerMap map[string]any, version, path string) ([]string, error) {
	key := "allows"
	if version == "v1" {
		key = "allowed"
	}
	v, ok := layerMap[key]
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, bizError(CodeFieldType, version, path+"."+key, "allowed object set must be an array")
	}
	out := make([]string, 0, len(arr))
	for i, item := range arr {
		s, err := asString(item, fmt.Sprintf("%s.%s[%d]", path, key, i), version)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseLayerKind(raw, version, path string) (LayerKind, error) {
	switch strings.ToLower(raw) {
	case "hot":
		return LayerHot, nil
	case "cold":
		return LayerCold, nil
	default:
		return "", bizError(CodeFieldType, version, path, "layer kind must be hot or cold")
	}
}

func parseEventKind(raw, version, path string) (EventKind, error) {
	switch strings.ToLower(raw) {
	case "access":
		return EventAccess, nil
	case "rebalance":
		return EventRebalance, nil
	case "read":
		return EventRead, nil
	case "write":
		return EventWrite, nil
	case "commit":
		return EventCommit, nil
	case "abort":
		return EventAbort, nil
	case "migrate":
		return EventMigrate, nil
	default:
		return "", bizError(CodeInvalidEvent, version, path, "unknown event kind")
	}
}

func parseCapacityUnit(raw, version, path string) (string, error) {
	switch strings.ToLower(raw) {
	case "byte", "bytes", "b":
		return "bytes", nil
	case "kib", "kibibyte", "kibibytes":
		return "kib", nil
	default:
		return "", bizError(CodeInvalidScale, version, path, "capacity unit must be bytes or KiB")
	}
}

func expectTimeUnit(raw, expected, version, path string) error {
	normalized := strings.ToLower(raw)
	expectedNorm := expected
	if normalized != expectedNorm {
		return bizError(CodeVersionMismatch, version, path, "time unit %q does not match schema layout", raw)
	}
	return nil
}

func validateScale(scale int, version, path string) error {
	if scale != 1000 && scale != 10000 {
		return bizError(CodeInvalidScale, version, path, "scale must be 1000 (permille) or 10000 (basis points)")
	}
	return nil
}

func scaleToBasisPoints(raw int64, scale int, version, path string) (int64, error) {
	if raw < 0 || raw > int64(scale) {
		return 0, bizError(CodeInvalidScale, version, path, "value %d is outside scale 0..%d", raw, scale)
	}
	if scale == 10000 {
		return raw, nil
	}
	return mulInt(raw, 10), nil
}

func unitToBytes(raw int64, unit, version, path string) (int64, error) {
	if raw < 0 {
		return 0, bizError(CodeNegativeCapacity, version, path, "value must not be negative")
	}
	switch unit {
	case "bytes":
		return raw, nil
	case "kib":
		return mulInt(raw, 1024), nil
	default:
		return 0, bizError(CodeInvalidScale, version, path, "unsupported capacity unit")
	}
}

func mulInt(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > (1<<62)/b {
		return 1<<62 - 1
	}
	return a * b
}

func asObject(value any, path, version string) (map[string]any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, bizError(CodeFieldType, version, path, "expected object")
	}
	return m, nil
}

func requireObject(m map[string]any, key, path, version string) (map[string]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, bizError(CodeFieldMissing, version, path, "missing field")
	}
	return asObject(v, path, version)
}

func requireString(m map[string]any, key, path, version string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", bizError(CodeFieldMissing, version, path, "missing field")
	}
	return asString(v, path, version)
}

func asString(v any, path, version string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", bizError(CodeFieldType, version, path, "expected string")
	}
	return s, nil
}

func requireInt(m map[string]any, key, path, version string, min int64) (int64, error) {
	v, ok := m[key]
	if !ok {
		return 0, bizError(CodeFieldMissing, version, path, "missing field")
	}
	n, err := asInt(v, path, version)
	if err != nil {
		return 0, err
	}
	if n < min {
		return 0, bizError(CodeFieldType, version, path, "value must be >= %d", min)
	}
	return n, nil
}

func optionalInt(m map[string]any, key, path, version string, fallback int64) (int64, error) {
	v, ok := m[key]
	if !ok {
		return fallback, nil
	}
	return asInt(v, path, version)
}

func asInt(v any, path, version string) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, bizError(CodeFieldType, version, path, "expected integral number")
		}
		return i, nil
	case float64:
		if n != float64(int64(n)) {
			return 0, bizError(CodeFieldType, version, path, "expected integral number")
		}
		return int64(n), nil
	default:
		return 0, bizError(CodeFieldType, version, path, "expected number")
	}
}

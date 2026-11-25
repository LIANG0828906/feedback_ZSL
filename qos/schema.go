package qos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type namedObj struct {
	Path string
	Obj  map[string]json.RawMessage
}

type namedField struct {
	Path string
	Raw  json.RawMessage
}

func decodeRoot(data []byte) (map[string]json.RawMessage, error) {
	if !json.Valid(data) {
		return nil, NewError(EJSON, "$", "input is not valid JSON", nil)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, wrapError(EJSON, "$", err, nil)
	}
	return root, nil
}

func object(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, wrapError(EFieldType, path, err, map[string]any{"expected": "object"})
	}
	if o == nil {
		return nil, NewError(EFieldType, path, "expected object, got null", nil)
	}
	return o, nil
}

func array(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, wrapError(EFieldType, path, err, map[string]any{"expected": "array"})
	}
	return a, nil
}

func collectFields(objs []namedObj, names ...string) []namedField {
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	var out []namedField
	for _, no := range objs {
		keys := make([]string, 0, len(no.Obj))
		for k := range no.Obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !wanted[k] {
				continue
			}
			raw := no.Obj[k]
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			out = append(out, namedField{Path: no.Path + "." + k, Raw: raw})
		}
	}
	return out
}

func oneRaw(objs []namedObj, logicalPath string, names ...string) (json.RawMessage, bool, error) {
	fields := collectFields(objs, names...)
	if len(fields) == 0 {
		return nil, false, nil
	}
	canonical := bytes.TrimSpace(fields[0].Raw)
	for _, f := range fields[1:] {
		if !bytes.Equal(canonical, bytes.TrimSpace(f.Raw)) {
			return nil, false, NewError(EFieldConflict, logicalPath,
				"aliases carry different values", map[string]any{"fields": fieldPaths(fields)})
		}
	}
	return fields[0].Raw, true, nil
}

func fieldPaths(fields []namedField) []string {
	out := make([]string, len(fields))
	for i := range fields {
		out[i] = fields[i].Path
	}
	return out
}

func oneInt(objs []namedObj, logicalPath string, names ...string) (int64, bool, error) {
	fields := collectFields(objs, names...)
	if len(fields) == 0 {
		return 0, false, nil
	}
	values := make([]int64, len(fields))
	for i, f := range fields {
		if err := json.Unmarshal(f.Raw, &values[i]); err != nil {
			return 0, false, wrapError(EFieldType, f.Path, err, map[string]any{"expected": "integer"})
		}
	}
	for i := 1; i < len(values); i++ {
		if values[i] != values[0] {
			return 0, false, NewError(EFieldConflict, logicalPath,
				"integer aliases carry different values", map[string]any{"fields": fieldPaths(fields), "values": values})
		}
	}
	return values[0], true, nil
}

func oneString(objs []namedObj, logicalPath string, names ...string) (string, bool, error) {
	fields := collectFields(objs, names...)
	if len(fields) == 0 {
		return "", false, nil
	}
	values := make([]string, len(fields))
	for i, f := range fields {
		if err := json.Unmarshal(f.Raw, &values[i]); err != nil {
			return "", false, wrapError(EFieldType, f.Path, err, map[string]any{"expected": "string"})
		}
	}
	for i := 1; i < len(values); i++ {
		if values[i] != values[0] {
			return "", false, NewError(EFieldConflict, logicalPath,
				"string aliases carry different values", map[string]any{"fields": fieldPaths(fields), "values": values})
		}
	}
	return values[0], true, nil
}

func oneBool(objs []namedObj, logicalPath string, names ...string) (bool, bool, error) {
	fields := collectFields(objs, names...)
	if len(fields) == 0 {
		return false, false, nil
	}
	values := make([]bool, len(fields))
	for i, f := range fields {
		if err := json.Unmarshal(f.Raw, &values[i]); err != nil {
			return false, false, wrapError(EFieldType, f.Path, err, map[string]any{"expected": "boolean"})
		}
	}
	for i := 1; i < len(values); i++ {
		if values[i] != values[0] {
			return false, false, NewError(EFieldConflict, logicalPath, "boolean aliases carry different values", map[string]any{"fields": fieldPaths(fields)})
		}
	}
	return values[0], true, nil
}

func objectsAt(objs []namedObj, logicalPath string, names ...string) ([]namedObj, error) {
	fields := collectFields(objs, names...)
	out := make([]namedObj, 0, len(fields))
	for _, f := range fields {
		o, err := object(f.Raw, f.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, namedObj{Path: f.Path, Obj: o})
	}
	return out, nil
}

func requiredInt(objs []namedObj, path string, names ...string) (int64, error) {
	v, ok, err := oneInt(objs, path, names...)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, NewError(EFieldMissing, path, "required integer field is missing", map[string]any{"accepted_fields": names})
	}
	return v, nil
}

func requiredString(objs []namedObj, path string, names ...string) (string, error) {
	v, ok, err := oneString(objs, path, names...)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", NewError(EFieldMissing, path, "required string field is missing", map[string]any{"accepted_fields": names})
	}
	return v, nil
}

func schemaNumber(schema string) (int, bool) {
	s := strings.ToLower(strings.TrimSpace(schema))
	s = strings.TrimPrefix(s, "qos.v")
	s = strings.TrimPrefix(s, "qos/")
	s = strings.TrimPrefix(s, "v")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 3 {
		return 0, false
	}
	return n, true
}

func LoadProject(data []byte) (*Project, error) {
	root, err := decodeRoot(data)
	if err != nil {
		return nil, err
	}
	raw, ok := root["schema"]
	if !ok {
		return nil, NewError(EFieldMissing, "schema", "schema is required", nil)
	}
	var schema string
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, wrapError(EFieldType, "schema", err, map[string]any{"expected": "string"})
	}
	v, ok := schemaNumber(schema)
	if !ok {
		return nil, NewError(ESCHEMA, "schema", "unsupported schema", map[string]any{"schema": schema})
	}
	return LoadProjectVersion(data, v)
}

func LoadProjectVersion(data []byte, version int) (*Project, error) {
	if version < 1 || version > 3 {
		return nil, NewError(ESCHEMA, "schema", "supported versions are 1,2,3", map[string]any{"version": version})
	}
	root, err := decodeRoot(data)
	if err != nil {
		return nil, err
	}
	if raw, ok := root["schema"]; ok {
		var schema string
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, wrapError(EFieldType, "schema", err, map[string]any{"expected": "string"})
		}
		actual, ok := schemaNumber(schema)
		if !ok || actual != version {
			return nil, NewError(ESCHEMA, "schema", "requested loader does not match document schema", map[string]any{"requested": version, "actual": schema})
		}
	}
	projectRaw, ok := root["project"]
	if !ok {
		return nil, NewError(EFieldMissing, "project", "project object is required", nil)
	}
	projectObj, err := object(projectRaw, "project")
	if err != nil {
		return nil, err
	}
	switch version {
	case 1:
		return loadV1(projectObj)
	case 2:
		return loadV2(projectObj)
	case 3:
		return loadV3(projectObj)
	default:
		return nil, NewError(ESCHEMA, "project", "unsupported schema", map[string]any{"version": version})
	}
}

func projectBase(project map[string]json.RawMessage, schema int, runtimeObjs ...namedObj) (*Project, []namedObj, error) {
	base := []namedObj{{Path: "project", Obj: project}}
	all := append([]namedObj(nil), base...)
	all = append(all, runtimeObjs...)
	version, err := requiredString(base, "project.version", "version", "revision")
	if err != nil {
		return nil, nil, err
	}
	name := "unnamed"
	if v, ok, err := oneString(base, "project.name", "name", "project_name"); err != nil {
		return nil, nil, err
	} else if ok {
		name = v
	}
	tick, err := requiredInt(all, "project.tick_ms", "tick_ms")
	if err != nil {
		return nil, nil, err
	}
	if tick <= 0 {
		return nil, nil, NewError(ETimeAlignment, "project.tick_ms", "tick must be positive", map[string]any{"tick_ms": tick})
	}
	width, ok, err := oneInt(all, "project.integer_width", "integer_width", "width_bits", "width")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, NewError(EFieldMissing, "project.integer_width", "integer width is required", nil)
	}
	nominal, err := requiredInt(all, "project.nominal_packet_bytes", "nominal_packet_bytes", "packet_quantum_bytes")
	if err != nil {
		return nil, nil, err
	}
	workers, ok, err := oneInt(all, "project.worker_threads", "worker_threads", "worker_count", "threads")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		workers = defaultWorkerThreads
	}
	qd, ok, err := oneInt(all, "project.queue_depth", "queue_depth", "queue_limit")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		qd = defaultQueueDepth
	}
	horizonMS := int64(0)
	if v, ok, err := oneInt(all, "project.horizon_ms", "horizon_ms"); err != nil {
		return nil, nil, err
	} else if ok {
		horizonMS = v
	}
	if v, ok, err := oneInt(all, "project.horizon_ticks", "horizon_ticks"); err != nil {
		return nil, nil, err
	} else if ok {
		ms, overflow := MulChecked(v, tick, 64)
		if overflow || ms < 0 {
			return nil, nil, NewError(EOverflow, "project.horizon_ticks", "horizon conversion overflow", map[string]any{"ticks": v, "tick_ms": tick})
		}
		if horizonMS != 0 && horizonMS != ms {
			return nil, nil, NewError(EFieldConflict, "project.horizon", "horizon aliases carry different values", map[string]any{"horizon_ms": horizonMS, "derived_ms": ms})
		}
		horizonMS = ms
	}
	if horizonMS == 0 {
		return nil, nil, NewError(EFieldMissing, "project.horizon_ms", "horizon is required", nil)
	}
	p := &Project{
		Name: name, Version: version, TickMS: tick, Width: int(width),
		NominalPacketBytes: nominal, WorkerThreads: int(workers),
		QueueDepth: int(qd), HorizonMS: horizonMS, SchemaVersion: schema,
	}
	return p, all, nil
}

type scalarRep struct {
	Path  string
	Value int64
	Scale int64
	Unit  string
}

func rateRepresentations(objs []namedObj, path string) ([]scalarRep, error) {
	var reps []scalarRep
	for _, f := range collectFields(objs, "rate_permille") {
		v, err := rawInt(f)
		if err != nil {
			return nil, err
		}
		reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermille, Unit: "rate"})
	}
	for _, f := range collectFields(objs, "rate_permyriad") {
		v, err := rawInt(f)
		if err != nil {
			return nil, err
		}
		reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermyriad, Unit: "rate"})
	}
	if raw, ok, err := oneRaw(objs, path+".rate", "rate"); err != nil {
		return nil, err
	} else if ok {
		var number int64
		if err := json.Unmarshal(raw, &number); err == nil {
			reps = append(reps, scalarRep{Path: path + ".rate", Value: number, Scale: RateScalePermyriad, Unit: "rate"})
		} else {
			ro, err := object(raw, path+".rate")
			if err != nil {
				return nil, err
			}
			nested := []namedObj{{Path: path + ".rate", Obj: ro}}
			sub, err := rateObjectRepresentations(nested)
			if err != nil {
				return nil, err
			}
			reps = append(reps, sub...)
		}
	}
	if v, ok, err := oneInt(objs, path+".rate_value", "rate_value"); err != nil {
		return nil, err
	} else if ok {
		scale, ok, err := oneInt(objs, path+".rate_scale", "rate_scale")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, NewError(EFieldMissing, path+".rate_scale", "rate_scale is required with rate_value", nil)
		}
		reps = append(reps, scalarRep{Path: path + ".rate_value", Value: v, Scale: scale, Unit: "rate"})
	}
	return reps, nil
}

func rateObjectRepresentations(objs []namedObj) ([]scalarRep, error) {
	var reps []scalarRep
	for _, f := range collectFields(objs, "permille") {
		v, err := rawInt(f)
		if err != nil {
			return nil, err
		}
		reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermille, Unit: "rate"})
	}
	for _, f := range collectFields(objs, "permyriad") {
		v, err := rawInt(f)
		if err != nil {
			return nil, err
		}
		reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermyriad, Unit: "rate"})
	}
	v, vok, err := oneInt(objs, "rate.value", "value")
	if err != nil {
		return nil, err
	}
	scale, sok, err := oneInt(objs, "rate.scale", "scale")
	if err != nil {
		return nil, err
	}
	if vok || sok {
		if !vok || !sok {
			return nil, NewError(EFieldMissing, "rate", "rate value and scale must be provided together", nil)
		}
		reps = append(reps, scalarRep{Path: "rate.value", Value: v, Scale: scale, Unit: "rate"})
	}
	return reps, nil
}

func rawInt(f namedField) (int64, error) {
	var v int64
	if err := json.Unmarshal(f.Raw, &v); err != nil {
		return 0, wrapError(EFieldType, f.Path, err, map[string]any{"expected": "integer"})
	}
	return v, nil
}

func chooseRate(objs []namedObj, path string, width int) (int64, RateDecl, error) {
	reps, err := rateRepresentations(objs, path)
	if err != nil {
		return 0, RateDecl{}, err
	}
	if len(reps) == 0 {
		return 0, RateDecl{}, NewError(EFieldMissing, path+".rate", "rate is required", nil)
	}
	var canonical int64
	decl := RateDecl{Value: reps[0].Value, Scale: reps[0].Scale}
	for i, rep := range reps {
		v, err := ConvertRateToPermyriad(rep.Value, rep.Scale, width)
		if err != nil {
			if qe, ok := err.(*Error); ok {
				qe.Path = rep.Path
			}
			return 0, RateDecl{}, err
		}
		if i == 0 {
			canonical = v
			continue
		}
		if v != canonical {
			return 0, RateDecl{}, NewError(EFieldConflict, path+".rate",
				"rate representations normalize to different values",
				map[string]any{"declared": reps, "canonical_permyriad": []int64{canonical, v}})
		}
	}
	return canonical, decl, nil
}

func capacityRepresentations(objs []namedObj, path string, p *Project) ([]namedField, error) {
	var out []namedField
	out = append(out, collectFields(objs, "burst_packets", "capacity_packets", "bucket_packets")...)
	out = append(out, collectFields(objs, "burst_bytes", "capacity_bytes", "bucket_bytes")...)
	nested, err := objectsAt(objs, path+".burst", "burst", "capacity", "bucket")
	if err != nil {
		return nil, err
	}
	for _, o := range nested {
		out = append(out, collectFields([]namedObj{o}, "packets")...)
		out = append(out, collectFields([]namedObj{o}, "bytes")...)
	}
	return out, nil
}

func chooseCapacity(objs []namedObj, path string, p *Project) (int64, error) {
	fields, err := capacityRepresentations(objs, path, p)
	if err != nil {
		return 0, err
	}
	if len(fields) == 0 {
		return 0, NewError(EFieldMissing, path+".capacity", "bucket capacity is required", nil)
	}
	var canonical int64
	seen := false
	for _, f := range fields {
		v, err := rawInt(f)
		if err != nil {
			return 0, err
		}
		name := f.Path[strings.LastIndex(f.Path, ".")+1:]
		var bytes int64
		if strings.Contains(name, "packet") || strings.HasPrefix(f.Path[strings.LastIndex(f.Path, ".")+1:], "packets") {
			bytes, err = ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
		} else if name == "packets" {
			bytes, err = ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
		} else {
			if v < 0 {
				return 0, NewError(ECapacityNegative, f.Path, "capacity must not be negative", map[string]any{"value": v})
			}
			bytes = v
			if bytes > widthMax(p.Width) {
				return 0, NewError(EOverflow, f.Path, "capacity exceeds declared width", map[string]any{"value": v, "width": p.Width})
			}
		}
		if err != nil {
			if qe, ok := err.(*Error); ok {
				qe.Path = f.Path
			}
			return 0, err
		}
		if !seen {
			canonical = bytes
			seen = true
		} else if canonical != bytes {
			return 0, NewError(EFieldConflict, path+".capacity",
				"capacity representations normalize to different byte values",
				map[string]any{"fields": fieldPaths(fields), "canonical_bytes": []int64{canonical, bytes}})
		}
	}
	return canonical, nil
}

func chooseWatermark(objs []namedObj, path string, p *Project, capacity int64) (int64, error) {
	var fields []namedField
	fields = append(fields, collectFields(objs, "initial_watermark_packets", "initial_tokens_packets", "watermark_packets")...)
	fields = append(fields, collectFields(objs, "initial_watermark_bytes", "initial_tokens_bytes", "watermark_bytes")...)
	nested, err := objectsAt(objs, path+".watermark", "watermark", "initial_watermark", "initial_tokens")
	if err != nil {
		return 0, err
	}
	for _, o := range nested {
		fields = append(fields, collectFields([]namedObj{o}, "packets")...)
		fields = append(fields, collectFields([]namedObj{o}, "bytes")...)
	}
	if len(fields) == 0 {
		return capacity, nil
	}
	var canonical int64
	seen := false
	for _, f := range fields {
		v, err := rawInt(f)
		if err != nil {
			return 0, err
		}
		bytes := v
		if strings.Contains(strings.ToLower(f.Path), "packet") || strings.HasSuffix(f.Path, ".packets") {
			bytes, err = ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
			if err != nil {
				return 0, err
			}
		} else if v > widthMax(p.Width) {
			return 0, NewError(EOverflow, f.Path, "watermark exceeds declared width", map[string]any{"value": v, "width": p.Width})
		}
		if !seen {
			canonical = bytes
			seen = true
		} else if canonical != bytes {
			return 0, NewError(EFieldConflict, path+".watermark",
				"watermark representations normalize to different byte values",
				map[string]any{"fields": fieldPaths(fields), "canonical_bytes": []int64{canonical, bytes}})
		}
	}
	return canonical, nil
}

func chooseWeight(objs []namedObj, path string, width int) (int64, RateDecl, error) {
	var reps []scalarRep
	for _, f := range collectFields(objs, "weight_permille") {
		v, err := rawInt(f)
		if err != nil {
			return 0, RateDecl{}, err
		}
		reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermille, Unit: "weight"})
	}
	for _, f := range collectFields(objs, "weight_permyriad") {
		v, err := rawInt(f)
		if err != nil {
			return 0, RateDecl{}, err
		}
		reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermyriad, Unit: "weight"})
	}
	if raw, ok, err := oneRaw(objs, path+".weight", "weight"); err != nil {
		return 0, RateDecl{}, err
	} else if ok {
		var number int64
		if err := json.Unmarshal(raw, &number); err == nil {
			scale := int64(RateScalePermyriad)
			if v, ok, err := oneInt(objs, path+".weight_scale", "weight_scale"); err != nil {
				return 0, RateDecl{}, err
			} else if ok {
				scale = v
			}
			reps = append(reps, scalarRep{Path: path + ".weight", Value: number, Scale: scale, Unit: "weight"})
		} else {
			ro, err := object(raw, path+".weight")
			if err != nil {
				return 0, RateDecl{}, err
			}
			nested := []namedObj{{Path: path + ".weight", Obj: ro}}
			for _, f := range collectFields(nested, "permille") {
				v, err := rawInt(f)
				if err != nil {
					return 0, RateDecl{}, err
				}
				reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermille, Unit: "weight"})
			}
			for _, f := range collectFields(nested, "permyriad") {
				v, err := rawInt(f)
				if err != nil {
					return 0, RateDecl{}, err
				}
				reps = append(reps, scalarRep{Path: f.Path, Value: v, Scale: RateScalePermyriad, Unit: "weight"})
			}
			v, vok, err := oneInt(nested, "weight.value", "value")
			if err != nil {
				return 0, RateDecl{}, err
			}
			scale, sok, err := oneInt(nested, "weight.scale", "scale")
			if err != nil {
				return 0, RateDecl{}, err
			}
			if vok || sok {
				if !vok || !sok {
					return 0, RateDecl{}, NewError(EFieldMissing, path+".weight", "weight value and scale must be provided together", nil)
				}
				reps = append(reps, scalarRep{Path: path + ".weight.value", Value: v, Scale: scale, Unit: "weight"})
			}
		}
	}
	if v, ok, err := oneInt(objs, path+".weight_value", "weight_value"); err != nil {
		return 0, RateDecl{}, err
	} else if ok {
		scale, ok, err := oneInt(objs, path+".weight_scale", "weight_scale")
		if err != nil {
			return 0, RateDecl{}, err
		}
		if !ok {
			return 0, RateDecl{}, NewError(EFieldMissing, path+".weight_scale", "weight_scale is required with weight_value", nil)
		}
		reps = append(reps, scalarRep{Path: path + ".weight_value", Value: v, Scale: scale, Unit: "weight"})
	}
	if len(reps) == 0 {
		return 0, RateDecl{}, NewError(EFieldMissing, path+".weight", "weight is required", nil)
	}
	canonical, err := ConvertWeightToPermyriad(reps[0].Value, reps[0].Scale, width)
	if err != nil {
		return 0, RateDecl{}, err
	}
	for _, rep := range reps[1:] {
		v, err := ConvertWeightToPermyriad(rep.Value, rep.Scale, width)
		if err != nil {
			return 0, RateDecl{}, err
		}
		if v != canonical {
			return 0, RateDecl{}, NewError(EFieldConflict, path+".weight", "weight representations normalize to different values", map[string]any{"declared": reps})
		}
	}
	return canonical, RateDecl{Value: reps[0].Value, Scale: reps[0].Scale}, nil
}

func chooseQueueAge(objs []namedObj, path string, tick int64) (int64, error) {
	values := make([]scalarRep, 0, 4)
	for _, f := range collectFields(objs, "queue_age_ticks") {
		v, err := rawInt(f)
		if err != nil {
			return 0, err
		}
		values = append(values, scalarRep{Path: f.Path, Value: v, Scale: tick})
	}
	for _, f := range collectFields(objs, "queue_age_ms", "queue_age_remaining_ms") {
		v, err := rawInt(f)
		if err != nil {
			return 0, err
		}
		values = append(values, scalarRep{Path: f.Path, Value: v, Scale: 1})
	}
	nested, err := objectsAt(objs, path+".queue_age", "queue_age", "age")
	if err != nil {
		return 0, err
	}
	for _, o := range nested {
		for _, f := range collectFields([]namedObj{o}, "ticks") {
			v, err := rawInt(f)
			if err != nil {
				return 0, err
			}
			values = append(values, scalarRep{Path: f.Path, Value: v, Scale: tick})
		}
		for _, f := range collectFields([]namedObj{o}, "ms", "remaining_ms") {
			v, err := rawInt(f)
			if err != nil {
				return 0, err
			}
			values = append(values, scalarRep{Path: f.Path, Value: v, Scale: 1})
		}
	}
	if len(values) == 0 {
		return 0, NewError(EFieldMissing, path+".queue_age", "queue age is required", nil)
	}
	var canonical int64
	for i, v := range values {
		ms, overflow := MulChecked(v.Value, v.Scale, 64)
		if overflow || ms < 0 {
			return 0, NewError(ETimeNegative, v.Path, "queue age conversion overflow or negative", map[string]any{"value": v.Value, "scale": v.Scale})
		}
		if i == 0 {
			canonical = ms
		} else if canonical != ms {
			return 0, NewError(EFieldConflict, path+".queue_age", "queue age aliases normalize to different milliseconds", map[string]any{"declared": values, "canonical_ms": []int64{canonical, ms}})
		}
	}
	if canonical <= 0 {
		return 0, NewError(EDelayQueue, path+".queue_age", "queue age must be positive", map[string]any{"queue_age_ms": canonical})
	}
	return canonical, nil
}

func chooseMaxDelay(objs []namedObj, path string, tick int64) (int64, error) {
	var values []scalarRep
	for _, f := range collectFields(objs, "max_delay_ticks") {
		v, err := rawInt(f)
		if err != nil {
			return 0, err
		}
		values = append(values, scalarRep{Path: f.Path, Value: v, Scale: tick})
	}
	for _, f := range collectFields(objs, "max_delay_ms", "max_remaining_ms") {
		v, err := rawInt(f)
		if err != nil {
			return 0, err
		}
		values = append(values, scalarRep{Path: f.Path, Value: v, Scale: 1})
	}
	nested, err := objectsAt(objs, path+".max_delay", "max_delay")
	if err != nil {
		return 0, err
	}
	for _, o := range nested {
		for _, f := range collectFields([]namedObj{o}, "ticks") {
			v, err := rawInt(f)
			if err != nil {
				return 0, err
			}
			values = append(values, scalarRep{Path: f.Path, Value: v, Scale: tick})
		}
		for _, f := range collectFields([]namedObj{o}, "ms", "remaining_ms") {
			v, err := rawInt(f)
			if err != nil {
				return 0, err
			}
			values = append(values, scalarRep{Path: f.Path, Value: v, Scale: 1})
		}
	}
	if len(values) == 0 {
		return 0, nil
	}
	var canonical int64
	for i, v := range values {
		ms, overflow := MulChecked(v.Value, v.Scale, 64)
		if overflow || ms < 0 {
			return 0, NewError(ETimeNegative, v.Path, "max delay conversion overflow or negative", map[string]any{"value": v.Value, "scale": v.Scale})
		}
		if i == 0 {
			canonical = ms
		} else if canonical != ms {
			return 0, NewError(EFieldConflict, path+".max_delay", "max delay aliases normalize to different milliseconds", map[string]any{"declared": values})
		}
	}
	return canonical, nil
}

func choosePolicy(objs []namedObj, path string) (Policy, error) {
	v, err := requiredString(objs, path+".policy", "policy", "action")
	if err != nil {
		return "", err
	}
	switch strings.ToLower(v) {
	case "drop", "discard":
		return PolicyDrop, nil
	case "defer", "delay", "queue":
		return PolicyDefer, nil
	default:
		return "", NewError(EPolicy, path+".policy", "unsupported policy", map[string]any{"policy": v})
	}
}

func parseDSCPClasses(objs []namedObj, path string) ([]int, error) {
	var values []int
	fields := collectFields(objs, "dscp_boost_classes", "boost_dscp_classes")
	for _, f := range fields {
		var arr []int
		if err := json.Unmarshal(f.Raw, &arr); err != nil {
			return nil, wrapError(EFieldType, f.Path, err, map[string]any{"expected": "array of integers"})
		}
		values = append(values, arr...)
	}
	if v, ok, err := oneInt(objs, path+".boost_dscp", "boost_dscp"); err != nil {
		return nil, err
	} else if ok {
		values = append(values, int(v))
	}
	sort.Ints(values)
	out := values[:0]
	for _, v := range values {
		if v < 0 || v > 63 {
			return nil, NewError(EDSCPRange, path+".dscp_boost_classes", "DSCP class must be in [0,63]", map[string]any{"dscp": v})
		}
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out, nil
}

func parseChannel(o map[string]json.RawMessage, index int, p *Project, v2 bool) (Channel, error) {
	path := fmt.Sprintf("project.channels[%d]", index)
	objs := []namedObj{{Path: path, Obj: o}}
	if v2 {
		if raw, ok := o["qos"]; ok {
			qo, err := object(raw, path+".qos")
			if err != nil {
				return Channel{}, err
			}
			objs = append(objs, namedObj{Path: path + ".qos", Obj: qo})
		}
	}
	id, err := requiredString(objs, path+".id", "id", "channel", "queue")
	if err != nil {
		return Channel{}, err
	}
	order, ok, err := oneInt(objs, path+".order", "order", "ordinal", "slot")
	if err != nil {
		return Channel{}, err
	}
	if !ok {
		order = int64(index)
	}
	rate, _, err := chooseRate(objs, path, p.Width)
	if err != nil {
		return Channel{}, err
	}
	capacity, err := chooseCapacity(objs, path, p)
	if err != nil {
		return Channel{}, err
	}
	watermark, err := chooseWatermark(objs, path, p, capacity)
	if err != nil {
		return Channel{}, err
	}
	policy, err := choosePolicy(objs, path)
	if err != nil {
		return Channel{}, err
	}
	priority, ok, err := oneInt(objs, path+".priority", "priority", "prio")
	if err != nil {
		return Channel{}, err
	}
	if !ok {
		priority = 0
	}
	weight, _, err := chooseWeight(objs, path, p.Width)
	if err != nil {
		return Channel{}, err
	}
	queueAge, err := chooseQueueAge(objs, path, p.TickMS)
	if err != nil {
		return Channel{}, err
	}
	maxDelay, err := chooseMaxDelay(objs, path, p.TickMS)
	if err != nil {
		return Channel{}, err
	}
	boost, ok, err := oneInt(objs, path+".priority_boost", "priority_boost", "boost_priority", "boost")
	if err != nil {
		return Channel{}, err
	}
	if !ok {
		boost = 0
	}
	dscps, err := parseDSCPClasses(objs, path)
	if err != nil {
		return Channel{}, err
	}
	return Channel{
		ID: id, Order: int(order), RatePermyriad: rate,
		CapacityBytes: capacity, InitialTokens: watermark, Policy: policy,
		Priority: int(priority), WeightPermyriad: weight,
		QueueAgeMS: queueAge, MaxDelayMS: maxDelay,
		PriorityBoost: int(boost), DSCPBoostClasses: dscps,
	}, nil
}

func parsePacket(o map[string]json.RawMessage, index int, p *Project) (Packet, error) {
	path := fmt.Sprintf("project.packets[%d]", index)
	objs := []namedObj{{Path: path, Obj: o}}
	id, err := requiredString(objs, path+".id", "id", "packet", "packet_id")
	if err != nil {
		return Packet{}, err
	}
	channel, err := requiredString(objs, path+".channel", "channel_id", "channel", "queue")
	if err != nil {
		return Packet{}, err
	}
	arrival := int64(0)
	seenArrival := false
	if v, ok, err := oneInt(objs, path+".arrival", "arrival_ms", "at_ms"); err != nil {
		return Packet{}, err
	} else if ok {
		arrival = v
		seenArrival = true
	}
	if v, ok, err := oneInt(objs, path+".arrival", "arrival_tick", "at_tick"); err != nil {
		return Packet{}, err
	} else if ok {
		ms, overflow := MulChecked(v, p.TickMS, 64)
		if overflow || ms < 0 {
			return Packet{}, NewError(ETimeNegative, path+".arrival_tick", "arrival tick conversion overflow or negative", map[string]any{"tick": v, "tick_ms": p.TickMS})
		}
		if seenArrival && arrival != ms {
			return Packet{}, NewError(EFieldConflict, path+".arrival", "arrival tick and millisecond aliases differ", map[string]any{"arrival_ms": arrival, "derived_ms": ms})
		}
		arrival = ms
		seenArrival = true
	}
	if !seenArrival {
		return Packet{}, NewError(EFieldMissing, path+".arrival", "arrival time is required", nil)
	}
	size, err := requiredInt(objs, path+".size_bytes", "size_bytes", "bytes", "packet_bytes")
	if err != nil {
		return Packet{}, err
	}
	priority, ok, err := oneInt(objs, path+".priority", "priority", "prio")
	if err != nil {
		return Packet{}, err
	}
	if !ok {
		priority = 0
	}
	seq, ok, err := oneInt(objs, path+".seq", "seq", "declaration_order")
	if err != nil {
		return Packet{}, err
	}
	if !ok {
		seq = int64(index)
	}
	var dscp *int
	if v, ok, err := oneInt(objs, path+".dscp", "dscp", "dscp_class"); err != nil {
		return Packet{}, err
	} else if ok {
		if v < 0 || v > 63 {
			return Packet{}, NewError(EDSCPRange, path+".dscp", "DSCP class must be in [0,63]", map[string]any{"dscp": v})
		}
		d := int(v)
		dscp = &d
	}
	return Packet{
		ID: id, ChannelID: channel, ArrivalMS: arrival,
		SizeBytes: size, Priority: int(priority), DSCP: dscp, Seq: int(seq),
	}, nil
}

func loadV1(project map[string]json.RawMessage) (*Project, error) {
	p, base, err := projectBase(project, 1)
	if err != nil {
		return nil, err
	}
	budgetReps := []scalarRep{}
	if v, ok, err := oneInt(base, "project.egress_budget", "egress_budget_packets"); err != nil {
		return nil, err
	} else if ok {
		b, err := ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
		if err != nil {
			return nil, err
		}
		budgetReps = append(budgetReps, scalarRep{Path: "project.egress_budget_packets", Value: b, Scale: 1})
	}
	if v, ok, err := oneInt(base, "project.egress_budget", "egress_budget_bytes"); err != nil {
		return nil, err
	} else if ok {
		budgetReps = append(budgetReps, scalarRep{Path: "project.egress_budget_bytes", Value: v, Scale: 1})
	}
	if len(budgetReps) == 0 {
		return nil, NewError(EFieldMissing, "project.egress_budget", "egress budget is required", nil)
	}
	p.EgressBudgetBytes = budgetReps[0].Value
	for _, r := range budgetReps[1:] {
		if r.Value != p.EgressBudgetBytes {
			return nil, NewError(EFieldConflict, "project.egress_budget", "egress budget aliases normalize differently", map[string]any{"values": budgetReps})
		}
	}
	rawChannels, ok := project["shaping"]
	if !ok {
		return nil, NewError(EFieldMissing, "project.shaping", "shaping table is required", nil)
	}
	chArr, err := array(rawChannels, "project.shaping")
	if err != nil {
		return nil, err
	}
	for i, raw := range chArr {
		o, err := object(raw, fmt.Sprintf("project.shaping[%d]", i))
		if err != nil {
			return nil, err
		}
		ch, err := parseChannel(o, i, p, false)
		if err != nil {
			return nil, err
		}
		p.Channels = append(p.Channels, ch)
	}
	rawPackets, ok := project["arrivals"]
	if !ok {
		return nil, NewError(EFieldMissing, "project.arrivals", "arrival stream is required", nil)
	}
	pktArr, err := array(rawPackets, "project.arrivals")
	if err != nil {
		return nil, err
	}
	for i, raw := range pktArr {
		o, err := object(raw, fmt.Sprintf("project.arrivals[%d]", i))
		if err != nil {
			return nil, err
		}
		pkt, err := parsePacket(o, i, p)
		if err != nil {
			return nil, err
		}
		p.Packets = append(p.Packets, pkt)
	}
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(project map[string]json.RawMessage) (*Project, error) {
	var runtimeObjs []namedObj
	if raw, ok := project["runtime"]; ok {
		o, err := object(raw, "project.runtime")
		if err != nil {
			return nil, err
		}
		runtimeObjs = append(runtimeObjs, namedObj{Path: "project.runtime", Obj: o})
	}
	p, _, err := projectBase(project, 2, runtimeObjs...)
	if err != nil {
		return nil, err
	}
	all := append([]namedObj{{Path: "project", Obj: project}}, runtimeObjs...)
	if v, ok, err := oneInt(all, "project.egress_budget", "egress_budget_bytes"); err != nil {
		return nil, err
	} else if ok {
		p.EgressBudgetBytes = v
	} else if v, ok, err := oneInt(all, "project.egress_budget", "egress_budget_packets"); err != nil {
		return nil, err
	} else if ok {
		p.EgressBudgetBytes, err = ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, NewError(EFieldMissing, "project.egress_budget", "egress budget is required", nil)
	}
	channelsObj, ok := project["channels"]
	if !ok {
		return nil, NewError(EFieldMissing, "project.channels", "channels group is required", nil)
	}
	channels, err := object(channelsObj, "project.channels")
	if err != nil {
		return nil, err
	}
	rawProfiles, ok := channels["profiles"]
	if !ok {
		return nil, NewError(EFieldMissing, "project.channels.profiles", "channel profiles are required", nil)
	}
	profiles, err := array(rawProfiles, "project.channels.profiles")
	if err != nil {
		return nil, err
	}
	for i, raw := range profiles {
		o, err := object(raw, fmt.Sprintf("project.channels.profiles[%d]", i))
		if err != nil {
			return nil, err
		}
		ch, err := parseChannel(o, i, p, true)
		if err != nil {
			return nil, err
		}
		p.Channels = append(p.Channels, ch)
	}
	rawPackets, ok := project["arrival_stream"]
	if !ok {
		if rawPackets, ok = project["packets"]; !ok {
			return nil, NewError(EFieldMissing, "project.arrival_stream", "arrival stream is required", nil)
		}
	}
	pktArr, err := array(rawPackets, "project.arrival_stream")
	if err != nil {
		return nil, err
	}
	for i, raw := range pktArr {
		o, err := object(raw, fmt.Sprintf("project.arrival_stream[%d]", i))
		if err != nil {
			return nil, err
		}
		pkt, err := parsePacket(o, i, p)
		if err != nil {
			return nil, err
		}
		p.Packets = append(p.Packets, pkt)
	}
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(project map[string]json.RawMessage) (*Project, error) {
	var defaultsObjs []namedObj
	if raw, ok := project["defaults"]; ok {
		o, err := object(raw, "project.defaults")
		if err != nil {
			return nil, err
		}
		defaultsObjs = append(defaultsObjs, namedObj{Path: "project.defaults", Obj: o})
	}
	p, _, err := projectBase(project, 3, defaultsObjs...)
	if err != nil {
		return nil, err
	}
	all := append([]namedObj{{Path: "project", Obj: project}}, defaultsObjs...)
	if v, ok, err := oneInt(all, "project.egress_budget", "egress_budget_bytes"); err != nil {
		return nil, err
	} else if ok {
		p.EgressBudgetBytes = v
	} else if v, ok, err := oneInt(all, "project.egress_budget", "egress_budget_packets"); err != nil {
		return nil, err
	} else if ok {
		p.EgressBudgetBytes, err = ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
		if err != nil {
			return nil, err
		}
	} else if raw, ok, err := oneRaw(all, "project.egress_budget", "queue_budget"); err != nil {
		return nil, err
	} else if ok {
		bo, err := object(raw, "project.queue_budget")
		if err != nil {
			return nil, err
		}
		boObjs := []namedObj{{Path: "project.queue_budget", Obj: bo}}
		if v, ok, err := oneInt(boObjs, "project.queue_budget.bytes", "bytes"); err != nil {
			return nil, err
		} else if ok {
			p.EgressBudgetBytes = v
		} else if v, ok, err := oneInt(boObjs, "project.queue_budget.packets", "packets"); err != nil {
			return nil, err
		} else if ok {
			p.EgressBudgetBytes, err = ConvertPacketsToBytes(v, p.NominalPacketBytes, p.Width)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, NewError(EFieldMissing, "project.queue_budget", "queue budget needs bytes or packets", nil)
		}
	} else {
		return nil, NewError(EFieldMissing, "project.egress_budget", "egress budget is required", nil)
	}
	rawChannels, ok := project["shaping_table"]
	if !ok {
		if rawChannels, ok = project["channels"]; !ok {
			return nil, NewError(EFieldMissing, "project.shaping_table", "shaping table is required", nil)
		}
	}
	chArr, err := array(rawChannels, "project.shaping_table")
	if err != nil {
		return nil, err
	}
	for i, raw := range chArr {
		o, err := object(raw, fmt.Sprintf("project.shaping_table[%d]", i))
		if err != nil {
			return nil, err
		}
		ch, err := parseChannel(o, i, p, false)
		if err != nil {
			return nil, err
		}
		p.Channels = append(p.Channels, ch)
	}
	rawPackets, ok := project["packets"]
	if !ok {
		return nil, NewError(EFieldMissing, "project.packets", "packet stream is required", nil)
	}
	pktArr, err := array(rawPackets, "project.packets")
	if err != nil {
		return nil, err
	}
	for i, raw := range pktArr {
		o, err := object(raw, fmt.Sprintf("project.packets[%d]", i))
		if err != nil {
			return nil, err
		}
		pkt, err := parsePacket(o, i, p)
		if err != nil {
			return nil, err
		}
		p.Packets = append(p.Packets, pkt)
	}
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func finalizeProject(p *Project) error {
	if err := ValidateProject(p); err != nil {
		return err
	}
	return nil
}

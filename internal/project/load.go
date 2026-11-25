package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"credrot/internal/bizerr"
	"credrot/internal/hashutil"
	"credrot/internal/model"
)

const (
	schemaV1 = 1
	schemaV2 = 2
	schemaV3 = 3
)

// Load parses one of the three declared schema versions and normalizes it.
// The input byte slice is never modified.
func Load(raw []byte) (*model.Loaded, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, bizerr.Wrap(bizerr.CodeSchemaInvalid, "document is not a JSON object", err, map[string]any{
			"offset": offsetFromJSONError(err),
		})
	}
	version, err := detectSchemaVersion(root)
	if err != nil {
		return nil, err
	}

	var (
		p     *model.Project
		notes []string
	)
	switch version {
	case schemaV1:
		p, notes, err = loadV1(root)
	case schemaV2:
		p, notes, err = loadV2(root)
	case schemaV3:
		p, notes, err = loadV3(root)
	default:
		return nil, bizerr.New(bizerr.CodeSchemaUnsupported, "unsupported schema version", map[string]any{"schema_version": version})
	}
	if err != nil {
		return nil, err
	}
	p.Sort()
	if err := Validate(p); err != nil {
		return nil, err
	}
	return &model.Loaded{
		Project:      p,
		SourceSchema: fmt.Sprintf("v%d", version),
		RawSHA256:    hashutil.SHA256(raw),
		Notes:        notes,
	}, nil
}

func detectSchemaVersion(root map[string]json.RawMessage) (int, error) {
	var versions []int
	if raw, ok := root["schema"]; ok && !isJSONNull(raw) {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "schema field must be a string", map[string]any{"field": "schema"})
		}
		v, ok := schemaFromText(text)
		if !ok {
			return 0, bizerr.New(bizerr.CodeSchemaUnsupported, "unrecognized schema identifier", map[string]any{"schema": text})
		}
		versions = append(versions, v)
	}
	if raw, ok := root["schema_version"]; ok && !isJSONNull(raw) {
		v, err := rawInt64(raw)
		if err != nil {
			return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "schema_version must be an integer", map[string]any{"field": "schema_version"})
		}
		if v < 1 || v > 3 {
			return 0, bizerr.New(bizerr.CodeSchemaUnsupported, "unsupported schema_version", map[string]any{"schema_version": v})
		}
		versions = append(versions, int(v))
	}
	if len(versions) == 0 {
		return 0, bizerr.New(bizerr.CodeSchemaUnsupported, "schema or schema_version is required", nil)
	}
	for _, v := range versions[1:] {
		if v != versions[0] {
			return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "conflicting schema version fields", map[string]any{
				"versions": versions,
			})
		}
	}
	return versions[0], nil
}

func schemaFromText(text string) (int, bool) {
	s := strings.ToLower(strings.TrimSpace(text))
	switch s {
	case "1", "v1", "credential-project/v1", "credential_project_v1":
		return schemaV1, true
	case "2", "v2", "credential-project/v2", "credential_project_v2":
		return schemaV2, true
	case "3", "v3", "credential-project/v3", "credential_project_v3":
		return schemaV3, true
	default:
		return 0, false
	}
}

func loadV1(root map[string]json.RawMessage) (*model.Project, []string, error) {
	p := &model.Project{}
	var err error
	if p.ProjectID, err = requiredString(root, "project_id"); err != nil {
		return nil, nil, err
	}
	if p.ManifestVersion, err = requiredString(root, "manifest_version"); err != nil {
		return nil, nil, err
	}
	timeObj, err := requiredObject(root, "time")
	if err != nil {
		return nil, nil, err
	}
	unit, err := requiredString(timeObj, "unit")
	if err != nil {
		return nil, nil, pathErr(err, "time.unit")
	}
	if !isTickUnit(unit) {
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v1 time unit must be logical_tick", map[string]any{"field": "time.unit", "value": unit})
	}
	if p.TickMillis, err = requiredInt64(timeObj, "tick_ms"); err != nil {
		return nil, nil, pathErr(err, "time.tick_ms")
	}
	if p.HorizonTicks, err = requiredInt64(root, "horizon_ticks"); err != nil {
		return nil, nil, err
	}
	ttlUnit, err := requiredString(root, "ttl_unit")
	if err != nil {
		return nil, nil, err
	}
	if !isSecondUnit(ttlUnit) {
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v1 ttl unit must be seconds", map[string]any{
			"field": "ttl_unit",
			"value": ttlUnit,
		})
	}

	credRows, err := requiredArray(root, "credentials")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range credRows {
		m, err := asObject(raw)
		if err != nil {
			return nil, nil, pathErr(err, fmt.Sprintf("credentials[%d]", i))
		}
		c, err := loadV1Credential(m, p.TickMillis, i)
		if err != nil {
			return nil, nil, err
		}
		p.Credentials = append(p.Credentials, c)
	}

	subRows, err := requiredArray(root, "subscriptions")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range subRows {
		m, err := asObject(raw)
		if err != nil {
			return nil, nil, pathErr(err, fmt.Sprintf("subscriptions[%d]", i))
		}
		sub, err := loadV1Subscription(m, i)
		if err != nil {
			return nil, nil, err
		}
		p.Subscriptions = append(p.Subscriptions, sub)
	}

	if p.Rotations, err = loadRotations(root, "rotations", "credential_id"); err != nil {
		return nil, nil, err
	}
	reloadObj, err := requiredObject(root, "reload")
	if err != nil {
		return nil, nil, err
	}
	p.Reload, err = loadReload(reloadObj, p.TickMillis, true, "max_per_batch", "retry_limit", "retry_step_ticks")
	if err != nil {
		return nil, nil, err
	}

	notes := []string{"ttl seconds normalized to milliseconds", "logical ticks normalized to milliseconds"}
	if raw, ok := root["last_rotation_watermark_tick"]; ok && !isJSONNull(raw) {
		t, err := rawInt64(raw)
		if err != nil {
			return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "watermark tick must be an integer", map[string]any{"field": "last_rotation_watermark_tick"})
		}
		p.LastRotationWatermarkMillis, err = mulChecked(t, p.TickMillis)
		if err != nil {
			return nil, nil, err
		}
		notes = append(notes, "watermark declared as logical tick")
	} else {
		p.LastRotationWatermarkMillis = inferWatermark(p.Credentials)
		notes = append(notes, "watermark inferred from version-1 credentials")
	}
	if raw, ok := root["last_rotation_watermark_ms"]; ok && !isJSONNull(raw) {
		if _, existed := root["last_rotation_watermark_tick"]; existed {
			return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "watermark is expressed in both ticks and milliseconds", map[string]any{
				"fields": []string{"last_rotation_watermark_tick", "last_rotation_watermark_ms"},
			})
		}
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v1 document uses a millisecond watermark", map[string]any{
			"field": "last_rotation_watermark_ms",
		})
	}
	return p, notes, nil
}

func loadV1Credential(m map[string]json.RawMessage, tickMillis int64, index int) (model.Credential, error) {
	path := fmt.Sprintf("credentials[%d]", index)
	id, err := requiredAnyString(m, path+".id", "credential_id", "id")
	if err != nil {
		return model.Credential{}, err
	}
	version, err := requiredAnyInt64(m, path+".version", "credential_version", "version")
	if err != nil {
		return model.Credential{}, err
	}
	batch, err := requiredAnyInt64(m, path+".reload_batch", "reload_batch", "batch")
	if err != nil {
		return model.Credential{}, err
	}
	issued, err := singleTimeField(m, parseTick, tickMillis, path, "issued_tick", "issued_at_ms", true)
	if err != nil {
		return model.Credential{}, err
	}
	ttl, err := singleTTLField(m, secondTTL, true, path, "ttl", "ttl_sec", "ttl_seconds")
	if err != nil {
		return model.Credential{}, err
	}
	supersedes, _, _ := optionalAnyString(m, "supersedes", "previous_credential_id")
	if version < 0 || batch < 0 {
		return model.Credential{}, bizerr.New(bizerr.CodeSchemaInvalid, "credential version and batch must be non-negative", map[string]any{
			"path": path,
		})
	}
	return model.Credential{
		ID:             id,
		Version:        uint64(version),
		IssuedAtMillis: issued,
		TTLMillis:      ttl,
		ReloadBatch:    batch,
		Supersedes:     supersedes,
	}, nil
}

func loadV1Subscription(m map[string]json.RawMessage, index int) (model.Subscription, error) {
	path := fmt.Sprintf("subscriptions[%d]", index)
	id, err := requiredAnyString(m, path+".id", "subscription_id", "id")
	if err != nil {
		return model.Subscription{}, err
	}
	active := true
	if raw, ok := m["active"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &active); err != nil {
			return model.Subscription{}, bizerr.New(bizerr.CodeSchemaInvalid, "active must be boolean", map[string]any{"path": path + ".active"})
		}
	}
	deps, err := stringArray(m, "depends_on")
	if err != nil {
		return model.Subscription{}, pathErr(err, path+".depends_on")
	}
	refRows, err := requiredAnyArray(m, path+".refs", "refs", "credential_refs")
	if err != nil {
		return model.Subscription{}, err
	}
	refs, err := parseRefs(refRows, path+".refs", "credential_id", "version", "role")
	if err != nil {
		return model.Subscription{}, err
	}
	return model.Subscription{ID: id, Active: active, DependsOn: deps, Refs: refs}, nil
}

func loadV2(root map[string]json.RawMessage) (*model.Project, []string, error) {
	p := &model.Project{}
	var err error
	if p.ProjectID, err = requiredString(root, "project_id"); err != nil {
		return nil, nil, err
	}
	if p.ManifestVersion, err = requiredAnyString(root, "manifest", "manifest_version", "manifest_revision"); err != nil {
		return nil, nil, err
	}
	clock, err := requiredObject(root, "clock")
	if err != nil {
		return nil, nil, err
	}
	basis, err := requiredString(clock, "basis")
	if err != nil {
		return nil, nil, pathErr(err, "clock.basis")
	}
	if !isTickUnit(basis) {
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v2 clock basis must be logical_tick", map[string]any{
			"field": "clock.basis",
			"value": basis,
		})
	}
	if p.TickMillis, err = requiredInt64(clock, "tick_ms"); err != nil {
		return nil, nil, pathErr(err, "clock.tick_ms")
	}
	if p.HorizonTicks, err = requiredInt64(root, "horizon_ticks"); err != nil {
		return nil, nil, err
	}
	ttlUnit, err := requiredString(root, "ttl_unit")
	if err != nil {
		return nil, nil, err
	}
	if !isSecondUnit(ttlUnit) {
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v2 ttl unit must be seconds", map[string]any{
			"field": "ttl_unit",
			"value": ttlUnit,
		})
	}

	tree, err := requiredObject(root, "credential_tree")
	if err != nil {
		return nil, nil, err
	}
	faces, err := requiredObject(root, "faces")
	if err != nil {
		return nil, nil, err
	}
	faceIDs := sortedKeys(faces)
	for _, faceID := range faceIDs {
		rawFace := faces[faceID]
		fm, err := asObject(rawFace)
		if err != nil {
			return nil, nil, pathErr(err, "faces."+faceID)
		}
		active := true
		if raw, ok := fm["active"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &active); err != nil {
				return nil, nil, bizerr.New(bizerr.CodeSchemaInvalid, "active must be boolean", map[string]any{"path": "faces." + faceID + ".active"})
			}
		}
		deps, err := stringArray(fm, "depends_on_faces")
		if err != nil {
			return nil, nil, pathErr(err, "faces."+faceID+".depends_on_faces")
		}
		refRows, err := requiredAnyArray(fm, "faces."+faceID+".key_refs", "key_refs", "refs")
		if err != nil {
			return nil, nil, err
		}
		refs, err := parseRefs(refRows, "faces."+faceID+".key_refs", "key_id", "generation", "purpose")
		if err != nil {
			return nil, nil, err
		}
		p.Subscriptions = append(p.Subscriptions, model.Subscription{ID: faceID, Active: active, DependsOn: deps, Refs: refs})
	}

	seenCreds := make(map[string]model.Credential)
	for _, faceID := range sortedKeys(tree) {
		rows, err := asArray(tree[faceID])
		if err != nil {
			return nil, nil, pathErr(err, "credential_tree."+faceID)
		}
		for i, raw := range rows {
			m, err := asObject(raw)
			if err != nil {
				return nil, nil, pathErr(err, fmt.Sprintf("credential_tree.%s[%d]", faceID, i))
			}
			c, err := loadV2Credential(m, p.TickMillis, fmt.Sprintf("credential_tree.%s[%d]", faceID, i))
			if err != nil {
				return nil, nil, err
			}
			if previous, ok := seenCreds[c.ID]; ok {
				if !credentialsEqual(previous, c) {
					return nil, nil, bizerr.New(bizerr.CodeDuplicateCredential, "grouped credential definitions conflict", map[string]any{
						"credential_id": c.ID,
						"first_group":   previous,
						"other_group":   faceID,
					})
				}
				continue
			}
			seenCreds[c.ID] = c
			p.Credentials = append(p.Credentials, c)
		}
	}

	if p.Rotations, err = loadRotations(root, "rotations", "credential_id"); err != nil {
		return nil, nil, err
	}
	reloadObj, err := requiredAnyObject(root, "reload_limits")
	if err != nil {
		return nil, nil, err
	}
	p.Reload, err = loadReload(reloadObj, p.TickMillis, true, "batch_cap", "retry_ceiling", "retry_step_ticks")
	if err != nil {
		return nil, nil, err
	}
	p.LastRotationWatermarkMillis = inferWatermark(p.Credentials)
	return p, []string{
		"v2 credential version/reload batch fields use legacy swapped semantics",
		"grouped credential tree deduplicated by credential id",
		"watermark inferred from version-1 credentials",
	}, nil
}

func loadV2Credential(m map[string]json.RawMessage, tickMillis int64, path string) (model.Credential, error) {
	id, err := requiredAnyString(m, path+".id", "key_id", "credential_id", "id")
	if err != nil {
		return model.Credential{}, err
	}
	// V2 intentionally uses the opposite field names for these two values.
	batch, err := requiredAnyInt64(m, path+".version(legacy=batch)", "version")
	if err != nil {
		return model.Credential{}, err
	}
	version, err := requiredAnyInt64(m, path+".batch(legacy=version)", "batch")
	if err != nil {
		return model.Credential{}, err
	}
	issued, err := singleTimeField(m, parseTick, tickMillis, path, "issued_tick", "issued_at_ms", true)
	if err != nil {
		return model.Credential{}, err
	}
	ttl, err := singleTTLField(m, secondTTL, true, path, "ttl", "ttl_sec", "ttl_seconds")
	if err != nil {
		return model.Credential{}, err
	}
	supersedes, _, _ := optionalAnyString(m, "supersedes", "previous_key_id")
	if version < 0 || batch < 0 {
		return model.Credential{}, bizerr.New(bizerr.CodeSchemaInvalid, "credential version and batch must be non-negative", map[string]any{
			"path": path,
		})
	}
	return model.Credential{
		ID:             id,
		Version:        uint64(version),
		IssuedAtMillis: issued,
		TTLMillis:      ttl,
		ReloadBatch:    batch,
		Supersedes:     supersedes,
	}, nil
}

func loadV3(root map[string]json.RawMessage) (*model.Project, []string, error) {
	p := &model.Project{}
	var err error
	if p.ProjectID, err = requiredString(root, "project_id"); err != nil {
		return nil, nil, err
	}
	if p.ManifestVersion, err = requiredString(root, "manifest_version"); err != nil {
		return nil, nil, err
	}
	clock, err := requiredObject(root, "clock")
	if err != nil {
		return nil, nil, err
	}
	basis, err := requiredString(clock, "basis")
	if err != nil {
		return nil, nil, pathErr(err, "clock.basis")
	}
	if !isMillisecondUnit(basis) {
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v3 clock basis must be milliseconds", map[string]any{
			"field": "clock.basis",
			"value": basis,
		})
	}
	if p.TickMillis, err = requiredInt64(clock, "tick_ms"); err != nil {
		return nil, nil, pathErr(err, "clock.tick_ms")
	}
	horizonMS, err := requiredAnyInt64(root, "horizon", "horizon_ms", "horizon_millis")
	if err != nil {
		return nil, nil, err
	}
	if p.TickMillis <= 0 {
		return nil, nil, bizerr.New(bizerr.CodeSchemaInvalid, "tick_ms must be positive", map[string]any{"tick_ms": p.TickMillis})
	}
	p.HorizonTicks = (horizonMS + p.TickMillis - 1) / p.TickMillis
	if horizonMS < 0 {
		p.HorizonTicks = -1
	}
	ttlUnit, err := requiredString(root, "ttl_unit")
	if err != nil {
		return nil, nil, err
	}
	if !isMillisecondUnit(ttlUnit) {
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v3 ttl unit must be milliseconds", map[string]any{
			"field": "ttl_unit",
			"value": ttlUnit,
		})
	}

	credRows, err := requiredArray(root, "credentials")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range credRows {
		m, err := asObject(raw)
		if err != nil {
			return nil, nil, pathErr(err, fmt.Sprintf("credentials[%d]", i))
		}
		c, err := loadV3Credential(m, i)
		if err != nil {
			return nil, nil, err
		}
		p.Credentials = append(p.Credentials, c)
	}

	subRows, err := requiredAnyArray(root, "subscribers", "subscribers", "subscriptions")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range subRows {
		m, err := asObject(raw)
		if err != nil {
			return nil, nil, pathErr(err, fmt.Sprintf("subscribers[%d]", i))
		}
		id, err := requiredAnyString(m, fmt.Sprintf("subscribers[%d].id", i), "subscriber_id", "id")
		if err != nil {
			return nil, nil, err
		}
		active := true
		if raw, ok := m["active"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &active); err != nil {
				return nil, nil, bizerr.New(bizerr.CodeSchemaInvalid, "active must be boolean", map[string]any{
					"path": fmt.Sprintf("subscribers[%d].active", i),
				})
			}
		}
		deps, err := stringArray(m, "depends_on")
		if err != nil {
			return nil, nil, pathErr(err, fmt.Sprintf("subscribers[%d].depends_on", i))
		}
		refRows, err := requiredAnyArray(m, fmt.Sprintf("subscribers[%d].refs", i), "refs", "credential_refs")
		if err != nil {
			return nil, nil, err
		}
		refs, err := parseRefs(refRows, fmt.Sprintf("subscribers[%d].refs", i), "credential_id", "generation", "role")
		if err != nil {
			return nil, nil, err
		}
		p.Subscriptions = append(p.Subscriptions, model.Subscription{ID: id, Active: active, DependsOn: deps, Refs: refs})
	}

	if p.Rotations, err = loadRotations(root, "rotations", "credential_id"); err != nil {
		return nil, nil, err
	}
	reloadObj, err := requiredObject(root, "reload")
	if err != nil {
		return nil, nil, err
	}
	p.Reload, err = loadReload(reloadObj, 1, false, "max_per_batch", "retry_limit", "retry_step_ms")
	if err != nil {
		return nil, nil, err
	}

	if raw, ok := root["last_rotation_watermark_ms"]; ok && !isJSONNull(raw) {
		p.LastRotationWatermarkMillis, err = rawInt64(raw)
		if err != nil {
			return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "watermark must be an integer", map[string]any{
				"field": "last_rotation_watermark_ms",
			})
		}
	} else {
		p.LastRotationWatermarkMillis = inferWatermark(p.Credentials)
	}
	if raw, ok := root["last_rotation_watermark_tick"]; ok && !isJSONNull(raw) {
		if _, existed := root["last_rotation_watermark_ms"]; existed {
			return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "watermark is expressed in both ticks and milliseconds", map[string]any{
				"fields": []string{"last_rotation_watermark_tick", "last_rotation_watermark_ms"},
			})
		}
		return nil, nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "v3 document uses a logical-tick watermark", map[string]any{
			"field": "last_rotation_watermark_tick",
		})
	}
	return p, []string{
		"timestamps and ttl used millisecond units",
	}, nil
}

func loadV3Credential(m map[string]json.RawMessage, index int) (model.Credential, error) {
	path := fmt.Sprintf("credentials[%d]", index)
	id, err := requiredAnyString(m, path+".id", "credential_id", "id")
	if err != nil {
		return model.Credential{}, err
	}
	version, err := requiredAnyInt64(m, path+".version", "version", "generation", "credential_version")
	if err != nil {
		return model.Credential{}, err
	}
	batch, err := requiredAnyInt64(m, path+".batch", "batch", "reload_batch")
	if err != nil {
		return model.Credential{}, err
	}
	issued, err := singleTimeField(m, parseMillis, 1, path, "issued_tick", "issued_at_ms", false)
	if err != nil {
		return model.Credential{}, err
	}
	ttl, err := singleTTLField(m, millisecondTTL, false, path, "ttl_ms")
	if err != nil {
		return model.Credential{}, err
	}
	supersedes, _, _ := optionalAnyString(m, "supersedes", "previous_credential_id")
	if version < 0 || batch < 0 {
		return model.Credential{}, bizerr.New(bizerr.CodeSchemaInvalid, "credential version and batch must be non-negative", map[string]any{
			"path": path,
		})
	}
	return model.Credential{
		ID:             id,
		Version:        uint64(version),
		IssuedAtMillis: issued,
		TTLMillis:      ttl,
		ReloadBatch:    batch,
		Supersedes:     supersedes,
	}, nil
}

type timeParser func(int64, int64) (int64, error)

func parseTick(value, tickMillis int64) (int64, error) {
	return mulChecked(value, tickMillis)
}

func parseMillis(value, _ int64) (int64, error) {
	return value, nil
}

type ttlParser func(int64) (int64, error)

func secondTTL(value int64) (int64, error) {
	return mulChecked(value, 1000)
}

func millisecondTTL(value int64) (int64, error) {
	return value, nil
}

func singleTimeField(m map[string]json.RawMessage, parser timeParser, factor int64, path string, tickName, millisName string, tickBasis bool) (int64, error) {
	tickRaw, hasTick := m[tickName]
	millisRaw, hasMillis := m[millisName]
	if hasTick && hasMillis && !isJSONNull(tickRaw) && !isJSONNull(millisRaw) {
		return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "time field is expressed in two bases", map[string]any{
			"path":   path,
			"fields": []string{tickName, millisName},
		})
	}
	if hasTick && !isJSONNull(tickRaw) {
		if !tickBasis {
			return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "logical-tick time field is not valid in a millisecond schema", map[string]any{
				"path":  path,
				"field": tickName,
			})
		}
		v, err := rawInt64(tickRaw)
		if err != nil {
			return 0, bizerr.New(bizerr.CodeSchemaInvalid, "time field must be an integer", map[string]any{"path": path + "." + tickName})
		}
		return parser(v, factor)
	}
	if hasMillis && !isJSONNull(millisRaw) {
		if tickBasis {
			return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "millisecond time field is not valid in a logical-tick schema", map[string]any{
				"path":  path,
				"field": millisName,
			})
		}
		v, err := rawInt64(millisRaw)
		if err != nil {
			return 0, bizerr.New(bizerr.CodeSchemaInvalid, "time field must be an integer", map[string]any{"path": path + "." + millisName})
		}
		return parser(v, factor)
	}
	return 0, bizerr.New(bizerr.CodeSchemaInvalid, "time field is required", map[string]any{
		"path":   path,
		"fields": []string{tickName, millisName},
	})
}

func singleTTLField(m map[string]json.RawMessage, parser ttlParser, secondBasis bool, path string, names ...string) (int64, error) {
	foreignNames := []string{"ttl_ms"}
	if !secondBasis {
		foreignNames = []string{"ttl", "ttl_sec", "ttl_seconds"}
	}
	for _, foreign := range foreignNames {
		if raw, ok := m[foreign]; ok && !isJSONNull(raw) {
			return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "credential ttl uses the wrong unit for this schema", map[string]any{
				"path":  path,
				"field": foreign,
			})
		}
	}
	present := make([]string, 0, len(names))
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return 0, bizerr.New(bizerr.CodeSchemaInvalid, "credential ttl is required", map[string]any{"path": path})
	}
	if len(present) > 1 {
		return 0, bizerr.New(bizerr.CodeSchemaAmbiguous, "credential ttl is expressed more than once", map[string]any{
			"path":   path,
			"fields": present,
		})
	}
	raw := m[present[0]]
	v, err := rawInt64(raw)
	if err != nil {
		return 0, bizerr.New(bizerr.CodeSchemaInvalid, "credential ttl must be an integer", map[string]any{
			"path": path + "." + present[0],
		})
	}
	return parser(v)
}

func parseRefs(rows []json.RawMessage, path, idName, versionName, roleName string) ([]model.CredentialRef, error) {
	refs := make([]model.CredentialRef, 0, len(rows))
	for i, raw := range rows {
		m, err := asObject(raw)
		if err != nil {
			return nil, pathErr(err, fmt.Sprintf("%s[%d]", path, i))
		}
		id, err := requiredAnyString(m, fmt.Sprintf("%s[%d].id", path, i), idName, "credential_id", "key_id", "id")
		if err != nil {
			return nil, err
		}
		version, _, err := optionalAnyInt64(m, versionName, "credential_version", "version", "generation")
		if err != nil {
			return nil, pathErr(err, fmt.Sprintf("%s[%d].%s", path, i, versionName))
		}
		if version < 0 {
			return nil, bizerr.New(bizerr.CodeInvalidRotation, "credential reference version must be non-negative", map[string]any{
				"path":  path,
				"index": i,
			})
		}
		role, _, _ := optionalAnyString(m, roleName, "role", "purpose")
		refs = append(refs, model.CredentialRef{CredentialID: id, ExpectedVersion: uint64(version), Role: role})
	}
	return refs, nil
}

func loadRotations(root map[string]json.RawMessage, field, idField string) ([]model.Rotation, error) {
	rows, err := optionalAnyArray(root, field)
	if err != nil {
		return nil, err
	}
	out := make([]model.Rotation, 0, len(rows))
	for i, raw := range rows {
		m, err := asObject(raw)
		if err != nil {
			return nil, pathErr(err, fmt.Sprintf("%s[%d]", field, i))
		}
		id, err := requiredAnyString(m, fmt.Sprintf("%s[%d].id", field, i), idField, "credential_id", "key_id")
		if err != nil {
			return nil, err
		}
		from, err := requiredAnyInt64(m, fmt.Sprintf("%s[%d].from_version", field, i), "from_version", "from_generation")
		if err != nil {
			return nil, err
		}
		to, err := requiredAnyInt64(m, fmt.Sprintf("%s[%d].to_version", field, i), "to_version", "to_generation")
		if err != nil {
			return nil, err
		}
		if from < 0 || to < 0 {
			return nil, bizerr.New(bizerr.CodeInvalidRotation, "rotation versions must be non-negative", map[string]any{
				"rotation_index": i,
			})
		}
		out = append(out, model.Rotation{CredentialID: id, FromVersion: uint64(from), ToVersion: uint64(to)})
	}
	return out, nil
}

func loadReload(m map[string]json.RawMessage, tickMillis int64, stepsInTicks bool, maxField, retryField, stepField string) (model.ReloadConfig, error) {
	cfg := model.ReloadConfig{}
	var err error
	cfg.ScheduleStartTick, _, err = optionalAnyInt64(m, "schedule_start_tick", "start_tick")
	if err != nil {
		return cfg, pathErr(err, "reload.schedule_start_tick")
	}
	max, err := requiredAnyInt64(m, "reload."+maxField, maxField, "max_per_batch", "batch_cap")
	if err != nil {
		return cfg, err
	}
	if max < 0 || max > math.MaxInt {
		return cfg, bizerr.New(bizerr.CodeBatchLimit, "batch capacity is outside supported range", map[string]any{
			"max_per_batch": max,
			"constraint":    "REL-CAP-001",
		})
	}
	cfg.MaxPerBatch = int(max)
	retry, err := requiredAnyInt64(m, "reload."+retryField, retryField, "retry_limit", "retry_ceiling")
	if err != nil {
		return cfg, err
	}
	if retry < 0 || retry > math.MaxInt {
		return cfg, bizerr.New(bizerr.CodeSchemaInvalid, "retry limit is outside supported range", map[string]any{"retry_limit": retry})
	}
	cfg.RetryLimit = int(retry)
	step, err := requiredAnyInt64(m, "reload."+stepField, stepField, "retry_step_ticks", "retry_step_ms")
	if err != nil {
		return cfg, err
	}
	if stepsInTicks {
		cfg.RetryStepMillis, err = mulChecked(step, tickMillis)
		if err != nil {
			return cfg, err
		}
	} else {
		cfg.RetryStepMillis = step
	}
	rows, err := optionalAnyArray(m, "failure_plan", "failures")
	if err != nil {
		return cfg, pathErr(err, "reload.failure_plan")
	}
	for i, raw := range rows {
		fm, err := asObject(raw)
		if err != nil {
			return cfg, pathErr(err, fmt.Sprintf("reload.failure_plan[%d]", i))
		}
		id, err := requiredAnyString(fm, fmt.Sprintf("reload.failure_plan[%d].subscription_id", i), "subscription_id", "face_id", "id")
		if err != nil {
			return cfg, err
		}
		attemptRows, err := requiredAnyArray(fm, fmt.Sprintf("reload.failure_plan[%d].attempts", i), "attempts")
		if err != nil {
			return cfg, err
		}
		attempts := make([]int, 0, len(attemptRows))
		for j, attemptRaw := range attemptRows {
			attempt, err := rawInt64(attemptRaw)
			if err != nil || attempt > math.MaxInt {
				return cfg, bizerr.New(bizerr.CodeSchemaInvalid, "failure attempt must be an integer", map[string]any{
					"plan_index": i,
					"attempt":    j,
				})
			}
			attempts = append(attempts, int(attempt))
		}
		cfg.FailurePlan = append(cfg.FailurePlan, model.FailureInjection{SubscriptionID: id, Attempts: attempts})
	}
	// Validate rejects a zero cap. Checking here keeps the context tied to the
	// source field and makes the code stable across all three schemas.
	if cfg.MaxPerBatch == 0 {
		return cfg, bizerr.New(bizerr.CodeBatchLimit, "max subscriptions per reload batch must be positive", map[string]any{
			"max_per_batch": cfg.MaxPerBatch,
			"constraint":    "REL-CAP-001",
			"field":         maxField,
		})
	}
	return cfg, nil
}

func inferWatermark(credentials []model.Credential) int64 {
	var (
		found      bool
		watermark  int64
		sawVersion bool
	)
	for _, c := range credentials {
		if c.Version != 1 {
			continue
		}
		sawVersion = true
		if !found || c.IssuedAtMillis < watermark {
			found = true
			watermark = c.IssuedAtMillis
		}
	}
	if sawVersion {
		return watermark
	}
	for _, c := range credentials {
		if !found || c.IssuedAtMillis < watermark {
			found = true
			watermark = c.IssuedAtMillis
		}
	}
	if !found {
		return 0
	}
	return watermark
}

func credentialsEqual(a, b model.Credential) bool {
	return a == b
}

func rawInt64(raw json.RawMessage) (int64, error) {
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func requiredString(m map[string]json.RawMessage, field string) (string, error) {
	raw, ok := m[field]
	if !ok || isJSONNull(raw) {
		return "", bizerr.New(bizerr.CodeSchemaInvalid, "required string field is missing", map[string]any{"field": field})
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", bizerr.New(bizerr.CodeSchemaInvalid, "field must be a string", map[string]any{"field": field})
	}
	return out, nil
}

func requiredAnyString(m map[string]json.RawMessage, path string, names ...string) (string, error) {
	value, name, err := optionalAnyString(m, names...)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", bizerr.New(bizerr.CodeSchemaInvalid, "required string field is missing", map[string]any{
			"path":   path,
			"fields": names,
		})
	}
	return value, nil
}

func optionalAnyString(m map[string]json.RawMessage, names ...string) (string, string, error) {
	names = uniqueNames(names)
	present := make([]string, 0, len(names))
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			present = append(present, name)
		}
	}
	if len(present) > 1 {
		return "", "", bizerr.New(bizerr.CodeSchemaAmbiguous, "field is expressed more than once", map[string]any{
			"fields": present,
		})
	}
	if len(present) == 0 {
		return "", "", nil
	}
	var out string
	if err := json.Unmarshal(m[present[0]], &out); err != nil {
		return "", "", bizerr.New(bizerr.CodeSchemaInvalid, "field must be a string", map[string]any{"field": present[0]})
	}
	return out, present[0], nil
}

func requiredInt64(m map[string]json.RawMessage, field string) (int64, error) {
	raw, ok := m[field]
	if !ok || isJSONNull(raw) {
		return 0, bizerr.New(bizerr.CodeSchemaInvalid, "required integer field is missing", map[string]any{"field": field})
	}
	v, err := rawInt64(raw)
	if err != nil {
		return 0, bizerr.New(bizerr.CodeSchemaInvalid, "field must be an integer", map[string]any{"field": field})
	}
	return v, nil
}

func requiredAnyInt64(m map[string]json.RawMessage, path string, names ...string) (int64, error) {
	v, name, err := optionalAnyInt64(m, names...)
	if err != nil {
		return 0, err
	}
	if name == "" {
		return 0, bizerr.New(bizerr.CodeSchemaInvalid, "required integer field is missing", map[string]any{
			"path":   path,
			"fields": names,
		})
	}
	return v, nil
}

func optionalAnyInt64(m map[string]json.RawMessage, names ...string) (int64, string, error) {
	names = uniqueNames(names)
	present := make([]string, 0, len(names))
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			present = append(present, name)
		}
	}
	if len(present) > 1 {
		return 0, "", bizerr.New(bizerr.CodeSchemaAmbiguous, "integer field is expressed more than once", map[string]any{
			"fields": present,
		})
	}
	if len(present) == 0 {
		return 0, "", nil
	}
	v, err := rawInt64(m[present[0]])
	if err != nil {
		return 0, "", bizerr.New(bizerr.CodeSchemaInvalid, "field must be an integer", map[string]any{"field": present[0]})
	}
	return v, present[0], nil
}

func requiredObject(m map[string]json.RawMessage, field string) (map[string]json.RawMessage, error) {
	raw, ok := m[field]
	if !ok || isJSONNull(raw) {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "required object field is missing", map[string]any{"field": field})
	}
	out, err := asObject(raw)
	if err != nil {
		return nil, pathErr(err, field)
	}
	return out, nil
}

func requiredAnyObject(m map[string]json.RawMessage, names ...string) (map[string]json.RawMessage, error) {
	names = uniqueNames(names)
	if present := presentFields(m, names); len(present) > 1 {
		return nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "object field is expressed more than once", map[string]any{"fields": present})
	}
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			out, err := asObject(raw)
			if err != nil {
				return nil, pathErr(err, name)
			}
			return out, nil
		}
	}
	return nil, bizerr.New(bizerr.CodeSchemaInvalid, "required object field is missing", map[string]any{"fields": names})
}

func requiredArray(m map[string]json.RawMessage, field string) ([]json.RawMessage, error) {
	raw, ok := m[field]
	if !ok || isJSONNull(raw) {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "required array field is missing", map[string]any{"field": field})
	}
	out, err := asArray(raw)
	if err != nil {
		return nil, pathErr(err, field)
	}
	return out, nil
}

func requiredAnyArray(m map[string]json.RawMessage, path string, names ...string) ([]json.RawMessage, error) {
	names = uniqueNames(names)
	if present := presentFields(m, names); len(present) > 1 {
		return nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "array field is expressed more than once", map[string]any{
			"path":   path,
			"fields": present,
		})
	}
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			out, err := asArray(raw)
			if err != nil {
				return nil, pathErr(err, path)
			}
			return out, nil
		}
	}
	return nil, bizerr.New(bizerr.CodeSchemaInvalid, "required array field is missing", map[string]any{
		"path":   path,
		"fields": names,
	})
}

func optionalAnyArray(m map[string]json.RawMessage, names ...string) ([]json.RawMessage, error) {
	names = uniqueNames(names)
	if present := presentFields(m, names); len(present) > 1 {
		return nil, bizerr.New(bizerr.CodeSchemaAmbiguous, "array field is expressed more than once", map[string]any{"fields": present})
	}
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			out, err := asArray(raw)
			if err != nil {
				return nil, pathErr(err, name)
			}
			return out, nil
		}
	}
	return nil, nil
}

func asObject(raw []byte) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, bizerr.Wrap(bizerr.CodeSchemaInvalid, "value must be an object", err, nil)
	}
	return out, nil
}

func asArray(raw []byte) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, bizerr.Wrap(bizerr.CodeSchemaInvalid, "value must be an array", err, nil)
	}
	return out, nil
}

func stringArray(m map[string]json.RawMessage, field string) ([]string, error) {
	raw, ok := m[field]
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "field must be a string array", map[string]any{"field": field})
	}
	return out, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isTickUnit(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "logical_tick", "logical", "tick", "ticks":
		return true
	default:
		return false
	}
}

func isSecondUnit(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "s", "sec", "secs", "second", "seconds":
		return true
	default:
		return false
	}
}

func isMillisecondUnit(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ms", "milli", "millis", "millisecond", "milliseconds":
		return true
	default:
		return false
	}
}

func mulChecked(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return 0, bizerr.New(bizerr.CodeNumericOverflow, "unit conversion overflows int64", map[string]any{
			"left":  a,
			"right": b,
		})
	}
	if a < 0 && b > 0 && a < math.MinInt64/b {
		return 0, bizerr.New(bizerr.CodeNumericOverflow, "unit conversion underflows int64", map[string]any{
			"left":  a,
			"right": b,
		})
	}
	return a * b, nil
}

func pathErr(err error, path string) error {
	var be *bizerr.Error
	if ok := asBizError(err, &be); ok {
		if be.Context == nil {
			be.Context = map[string]any{}
		}
		if _, exists := be.Context["path"]; !exists {
			be.Context["path"] = path
		}
		return be
	}
	return err
}

func asBizError(err error, target **bizerr.Error) bool {
	for err != nil {
		if be, ok := err.(*bizerr.Error); ok {
			*target = be
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func uniqueNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func presentFields(m map[string]json.RawMessage, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if raw, ok := m[name]; ok && !isJSONNull(raw) {
			out = append(out, name)
		}
	}
	return out
}

func offsetFromJSONError(err error) int64 {
	if se, ok := err.(*json.SyntaxError); ok {
		return se.Offset
	}
	if text := err.Error(); strings.Contains(text, "unexpected end") {
		return -1
	}
	return 0
}

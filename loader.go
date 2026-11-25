package skillcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// Load auto-detects the declared format version and loads a rule project.
func Load(data []byte) (*Project, error) {
	version, err := DetectVersion(data)
	if err != nil {
		return nil, err
	}
	return LoadVersion(data, version)
}

func LoadFile(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, wrapErr(CodeInput, path, "read rule project", err)
	}
	return Load(data)
}

// DetectVersion reads only the version discriminator used by each schema.
func DetectVersion(data []byte) (int, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0, wrapErr(CodeVersion, "", "malformed JSON rule project", err)
	}
	if raw, ok := probe["schema_version"]; ok {
		version, err := decodeVersionNumber(raw, "schema_version")
		if err != nil {
			return 0, err
		}
		if version != 1 {
			return 0, unsupportedVersion("schema_version", version)
		}
		return version, nil
	}
	if raw, ok := probe["schemaVersion"]; ok {
		version, err := decodeVersionNumber(raw, "schemaVersion")
		if err != nil {
			return 0, err
		}
		if version != 2 {
			return 0, unsupportedVersion("schemaVersion", version)
		}
		return version, nil
	}
	if raw, ok := probe["format"]; ok {
		var format string
		if err := json.Unmarshal(raw, &format); err != nil {
			return 0, wrapErr(CodeVersion, "format", "format must be a string", err)
		}
		if format != "skill-rules" {
			return 0, domainErr(CodeVersion, "format", fmt.Sprintf("unknown rule format %q", format))
		}
		rawVersion, ok := probe["version"]
		if !ok {
			return 0, domainErr(CodeVersion, "version", "required field missing")
		}
		version, err := decodeVersionNumber(rawVersion, "version")
		if err != nil {
			return 0, err
		}
		if version != 3 {
			return 0, unsupportedVersion("version", version)
		}
		return version, nil
	}
	return 0, domainErr(CodeVersion, "schemaVersion", "required version discriminator is missing")
}

func decodeVersionNumber(raw json.RawMessage, path string) (int, error) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, wrapErr(CodeVersion, path, "version must be an integer", err)
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, wrapErr(CodeVersion, path, "version must be an integer", err)
	}
	return int(value), nil
}

func unsupportedVersion(path string, version int) *Error {
	return domainErr(CodeVersion, path, fmt.Sprintf("unsupported rule version %d", version))
}

// LoadVersion loads a specific format version. A document that declares a
// different version fails with E_VERSION rather than being guessed.
func LoadVersion(data []byte, version int) (*Project, error) {
	detected, err := DetectVersion(data)
	if err != nil {
		return nil, err
	}
	if detected != version {
		return nil, domainErr(CodeVersion, "version", fmt.Sprintf("document declares version %d, requested %d", detected, version))
	}
	switch version {
	case 1:
		return loadV1(data)
	case 2:
		return loadV2(data)
	case 3:
		return loadV3(data)
	default:
		return nil, unsupportedVersion("version", version)
	}
}

func decodeDocument(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		var typeErr *json.UnmarshalTypeError
		if ok := asJSONTypeError(err, &typeErr); ok && typeErr.Field != "" {
			return wrapErr(CodeVersion, typeErr.Field, "field has the wrong JSON type", err)
		}
		return wrapErr(CodeVersion, "", "malformed rule project", err)
	}
	return nil
}

func asJSONTypeError(err error, target **json.UnmarshalTypeError) bool {
	typeErr, ok := err.(*json.UnmarshalTypeError)
	if ok {
		*target = typeErr
	}
	return ok
}

func missing(path string) *Error {
	return domainErr(CodeVersion, path, "required field missing")
}

func require[T any](value *T, path string) (*T, error) {
	if value == nil {
		return nil, missing(path)
	}
	return value, nil
}

func requireString(value *string, path string) (string, error) {
	if value == nil {
		return "", missing(path)
	}
	if *value == "" {
		return "", domainErr(CodeVersion, path, "required field is empty")
	}
	return *value, nil
}

func parseRoundingField(value *string, path string) (Rounding, error) {
	if value == nil {
		return "", missing(path)
	}
	rounding, err := ParseRounding(*value)
	if err != nil {
		if domain, ok := err.(*Error); ok {
			domain.Path = path
			domain.Field = path
		}
		return "", err
	}
	return rounding, nil
}

// Schema version 1: snake_case, ticks, permille, flat constants.
type v1Document struct {
	SchemaVersion *int64     `json:"schema_version"`
	TickMillis    *int64     `json:"tick_millis"`
	ResourceScale *int64     `json:"resource_scale"`
	Rounding      *string    `json:"rounding"`
	Pools         *[]v1Pool  `json:"resource_pools"`
	Skills        *[]v1Skill `json:"skills"`
}

type v1Pool struct {
	ID           *string `json:"pool_id"`
	Capacity     *int64  `json:"capacity"`
	Initial      *int64  `json:"initial"`
	RegenPerTick *int64  `json:"regen_per_tick"`
}

type v1Skill struct {
	ID            *string `json:"skill_id"`
	PoolID        *string `json:"pool_id"`
	Cost          *int64  `json:"cost"`
	CooldownTicks *int64  `json:"cooldown_ticks"`
	CastTicks     *int64  `json:"cast_ticks"`
	CDRPermille   *int64  `json:"cooldown_reduction_permille"`
	Priority      *int    `json:"priority"`
	SharedGroup   *string `json:"shared_group"`
}

func loadV1(data []byte) (*Project, error) {
	var doc v1Document
	if err := decodeDocument(data, &doc); err != nil {
		return nil, err
	}
	if _, err := require(doc.SchemaVersion, "schema_version"); err != nil {
		return nil, err
	}
	tickMillis, err := require(doc.TickMillis, "tick_millis")
	if err != nil {
		return nil, err
	}
	scale, err := require(doc.ResourceScale, "resource_scale")
	if err != nil {
		return nil, err
	}
	if *scale != 1000 {
		return nil, domainErr(CodeVersion, "resource_scale", fmt.Sprintf("version 1 requires resource_scale=1000, got %d", *scale))
	}
	rounding, err := parseRoundingField(doc.Rounding, "rounding")
	if err != nil {
		return nil, err
	}
	if doc.Pools == nil {
		return nil, missing("resource_pools")
	}
	if doc.Skills == nil {
		return nil, missing("skills")
	}
	project := &Project{
		SourceVersion:  1,
		TickMillis:     *tickMillis,
		Rounding:       rounding,
		ConflictPolicy: ConflictPriorityThenID,
		Present:        map[string]bool{},
	}
	for i, raw := range *doc.Pools {
		base := fmt.Sprintf("resource_pools[%d]", i)
		id, err := requireString(raw.ID, base+".pool_id")
		if err != nil {
			return nil, err
		}
		capacity, err := require(raw.Capacity, base+".capacity")
		if err != nil {
			return nil, err
		}
		initial, err := require(raw.Initial, base+".initial")
		if err != nil {
			return nil, err
		}
		regen, err := require(raw.RegenPerTick, base+".regen_per_tick")
		if err != nil {
			return nil, err
		}
		internalCapacity, err := ScaleToInternal(*capacity, 1000, rounding, base+".capacity")
		if err != nil {
			return nil, err
		}
		internalInitial, err := ScaleToInternal(*initial, 1000, rounding, base+".initial")
		if err != nil {
			return nil, err
		}
		internalRegen, err := ScaleToInternal(*regen, 1000, rounding, base+".regen_per_tick")
		if err != nil {
			return nil, err
		}
		project.Pools = append(project.Pools, ResourcePool{
			ID:      id,
			Max:     internalCapacity,
			Initial: internalInitial,
			Regen:   Const(internalRegen),
		})
	}
	for i, raw := range *doc.Skills {
		base := fmt.Sprintf("skills[%d]", i)
		id, err := requireString(raw.ID, base+".skill_id")
		if err != nil {
			return nil, err
		}
		poolID, err := requireString(raw.PoolID, base+".pool_id")
		if err != nil {
			return nil, err
		}
		cost, err := require(raw.Cost, base+".cost")
		if err != nil {
			return nil, err
		}
		cooldown, err := require(raw.CooldownTicks, base+".cooldown_ticks")
		if err != nil {
			return nil, err
		}
		cast, err := require(raw.CastTicks, base+".cast_ticks")
		if err != nil {
			return nil, err
		}
		cdr, err := require(raw.CDRPermille, base+".cooldown_reduction_permille")
		if err != nil {
			return nil, err
		}
		priority, err := require(raw.Priority, base+".priority")
		if err != nil {
			return nil, err
		}
		group, err := require(raw.SharedGroup, base+".shared_group")
		if err != nil {
			return nil, err
		}
		internalCost, err := ScaleToInternal(*cost, 1000, rounding, base+".cost")
		if err != nil {
			return nil, err
		}
		internalCDR, err := PermilleToBP(*cdr, rounding, base+".cooldown_reduction_permille")
		if err != nil {
			return nil, err
		}
		project.Skills = append(project.Skills, Skill{
			ID:          id,
			PoolID:      poolID,
			Cost:        internalCost,
			Cooldown:    Const(*cooldown),
			CastTicks:   *cast,
			CDRBp:       internalCDR,
			Priority:    *priority,
			SharedGroup: *group,
			Enabled:     true,
		})
	}
	if err := project.Normalize(); err != nil {
		return nil, err
	}
	return project, nil
}

// Schema version 2: camelCase, milliseconds, permyriad, flat constants and
// optional enabled/description fields.
type v2Document struct {
	SchemaVersion *int64     `json:"schemaVersion"`
	MSPerTick     *int64     `json:"msPerTick"`
	ResourceScale *int64     `json:"resourceScale"`
	Rounding      *string    `json:"rounding"`
	Pools         *[]v2Pool  `json:"pools"`
	Abilities     *[]v2Skill `json:"abilities"`
}

type v2Pool struct {
	ID           *string `json:"id"`
	Max          *int64  `json:"max"`
	Initial      *int64  `json:"initial"`
	RegenPerTick *int64  `json:"regenPerTick"`
}

type v2Skill struct {
	ID          *string `json:"id"`
	Pool        *string `json:"pool"`
	Cost        *int64  `json:"cost"`
	CooldownMS  *int64  `json:"cooldownMs"`
	CastMS      *int64  `json:"castMs"`
	CDRBp       *int64  `json:"cdrBp"`
	Priority    *int    `json:"priority"`
	SharedGroup *string `json:"sharedGroup"`
	Enabled     *bool   `json:"enabled"`
	Description *string `json:"description"`
}

func loadV2(data []byte) (*Project, error) {
	var doc v2Document
	if err := decodeDocument(data, &doc); err != nil {
		return nil, err
	}
	if _, err := require(doc.SchemaVersion, "schemaVersion"); err != nil {
		return nil, err
	}
	tickMillis, err := require(doc.MSPerTick, "msPerTick")
	if err != nil {
		return nil, err
	}
	if *tickMillis <= 0 {
		return nil, domainErr(CodeVersion, "msPerTick", "must be greater than zero")
	}
	scale, err := require(doc.ResourceScale, "resourceScale")
	if err != nil {
		return nil, err
	}
	if *scale != InternalScale {
		return nil, domainErr(CodeVersion, "resourceScale", fmt.Sprintf("version 2 requires resourceScale=10000, got %d", *scale))
	}
	rounding, err := parseRoundingField(doc.Rounding, "rounding")
	if err != nil {
		return nil, err
	}
	if doc.Pools == nil {
		return nil, missing("pools")
	}
	if doc.Abilities == nil {
		return nil, missing("abilities")
	}
	project := &Project{
		SourceVersion:  2,
		TickMillis:     *tickMillis,
		Rounding:       rounding,
		ConflictPolicy: ConflictPriorityThenID,
		Present:        map[string]bool{},
	}
	for i, raw := range *doc.Pools {
		base := fmt.Sprintf("pools[%d]", i)
		id, err := requireString(raw.ID, base+".id")
		if err != nil {
			return nil, err
		}
		max, err := require(raw.Max, base+".max")
		if err != nil {
			return nil, err
		}
		initial, err := require(raw.Initial, base+".initial")
		if err != nil {
			return nil, err
		}
		regen, err := require(raw.RegenPerTick, base+".regenPerTick")
		if err != nil {
			return nil, err
		}
		project.Pools = append(project.Pools, ResourcePool{
			ID:      id,
			Max:     *max,
			Initial: *initial,
			Regen:   Const(*regen),
		})
	}
	for i, raw := range *doc.Abilities {
		base := fmt.Sprintf("abilities[%d]", i)
		id, err := requireString(raw.ID, base+".id")
		if err != nil {
			return nil, err
		}
		poolID, err := requireString(raw.Pool, base+".pool")
		if err != nil {
			return nil, err
		}
		cost, err := require(raw.Cost, base+".cost")
		if err != nil {
			return nil, err
		}
		cooldownMS, err := require(raw.CooldownMS, base+".cooldownMs")
		if err != nil {
			return nil, err
		}
		castMS, err := require(raw.CastMS, base+".castMs")
		if err != nil {
			return nil, err
		}
		cdr, err := require(raw.CDRBp, base+".cdrBp")
		if err != nil {
			return nil, err
		}
		priority, err := require(raw.Priority, base+".priority")
		if err != nil {
			return nil, err
		}
		group, err := require(raw.SharedGroup, base+".sharedGroup")
		if err != nil {
			return nil, err
		}
		cooldownTicks, err := MillisToTicks(*cooldownMS, *tickMillis, rounding, base+".cooldownMs")
		if err != nil {
			return nil, err
		}
		castTicks, err := MillisToTicks(*castMS, *tickMillis, rounding, base+".castMs")
		if err != nil {
			return nil, err
		}
		enabled := true
		if raw.Enabled != nil {
			enabled = *raw.Enabled
			project.Present[skillPresence(id, "enabled")] = true
		}
		description := ""
		if raw.Description != nil {
			description = *raw.Description
			project.Present[skillPresence(id, "description")] = true
		}
		project.Skills = append(project.Skills, Skill{
			ID:          id,
			PoolID:      poolID,
			Cost:        *cost,
			Cooldown:    Const(cooldownTicks),
			CastTicks:   castTicks,
			CDRBp:       *cdr,
			Priority:    *priority,
			SharedGroup: *group,
			Enabled:     enabled,
			Description: description,
		})
		project.Present[skillPresence(id, "sharedGroup")] = true
	}
	if err := project.Normalize(); err != nil {
		return nil, err
	}
	return project, nil
}

// Schema version 3: compact names, ticks, permyriad, expression trees and all
// optional descriptive fields.
type v3Document struct {
	Format         *string    `json:"format"`
	Version        *int64     `json:"version"`
	Clock          *v3Clock   `json:"clock"`
	ResourceScale  *int64     `json:"resourceScale"`
	Rounding       *string    `json:"rounding"`
	ConflictPolicy *string    `json:"conflictPolicy"`
	Pools          *[]v3Pool  `json:"pools"`
	Skills         *[]v3Skill `json:"skills"`
}

type v3Clock struct {
	Unit          *string `json:"unit"`
	MillisPerTick *int64  `json:"millisPerTick"`
}

type v3Pool struct {
	ID    *string `json:"id"`
	Max   *int64  `json:"max"`
	Start *int64  `json:"start"`
	Regen *Expr   `json:"regen"`
}

type v3Skill struct {
	ID          *string     `json:"id"`
	Pool        *string     `json:"pool"`
	Cost        *int64      `json:"cost"`
	Cooldown    *v3Duration `json:"cooldown"`
	Cast        *v3Cast     `json:"cast"`
	CDRBp       *int64      `json:"cdrBp"`
	Priority    *int        `json:"priority"`
	Group       *string     `json:"group"`
	Enabled     *bool       `json:"enabled"`
	Description *string     `json:"description"`
	Tags        *[]string   `json:"tags"`
}

type v3Duration struct {
	Unit *string `json:"unit"`
	Expr *Expr   `json:"expr"`
}

type v3Cast struct {
	Unit  *string `json:"unit"`
	Value *int64  `json:"value"`
}

func loadV3(data []byte) (*Project, error) {
	var doc v3Document
	if err := decodeDocument(data, &doc); err != nil {
		return nil, err
	}
	format, err := requireString(doc.Format, "format")
	if err != nil {
		return nil, err
	}
	if format != "skill-rules" {
		return nil, domainErr(CodeVersion, "format", fmt.Sprintf("unknown rule format %q", format))
	}
	if _, err := require(doc.Version, "version"); err != nil {
		return nil, err
	}
	if doc.Clock == nil {
		return nil, missing("clock")
	}
	clockUnit, err := requireString(doc.Clock.Unit, "clock.unit")
	if err != nil {
		return nil, err
	}
	if clockUnit != string(UnitTick) {
		return nil, domainErr(CodeVersion, "clock.unit", "version 3 uses a tick clock; cooldown expressions return ticks")
	}
	tickMillis, err := require(doc.Clock.MillisPerTick, "clock.millisPerTick")
	if err != nil {
		return nil, err
	}
	if *tickMillis <= 0 {
		return nil, domainErr(CodeVersion, "clock.millisPerTick", "must be greater than zero")
	}
	scale, err := require(doc.ResourceScale, "resourceScale")
	if err != nil {
		return nil, err
	}
	if *scale != InternalScale {
		return nil, domainErr(CodeVersion, "resourceScale", fmt.Sprintf("version 3 requires resourceScale=10000, got %d", *scale))
	}
	rounding, err := parseRoundingField(doc.Rounding, "rounding")
	if err != nil {
		return nil, err
	}
	policy := ConflictPriorityThenID
	if doc.ConflictPolicy != nil {
		policy, err = ParseConflictPolicy(*doc.ConflictPolicy)
		if err != nil {
			return nil, err
		}
	}
	if doc.Pools == nil {
		return nil, missing("pools")
	}
	if doc.Skills == nil {
		return nil, missing("skills")
	}
	project := &Project{
		SourceVersion:  3,
		TickMillis:     *tickMillis,
		Rounding:       rounding,
		ConflictPolicy: policy,
		Present:        map[string]bool{},
	}
	for i, raw := range *doc.Pools {
		base := fmt.Sprintf("pools[%d]", i)
		id, err := requireString(raw.ID, base+".id")
		if err != nil {
			return nil, err
		}
		max, err := require(raw.Max, base+".max")
		if err != nil {
			return nil, err
		}
		start, err := require(raw.Start, base+".start")
		if err != nil {
			return nil, err
		}
		regen, err := require(raw.Regen, base+".regen")
		if err != nil {
			return nil, err
		}
		if err := regen.Validate(base + ".regen"); err != nil {
			return nil, err
		}
		project.Pools = append(project.Pools, ResourcePool{
			ID:      id,
			Max:     *max,
			Initial: *start,
			Regen:   regen.Clone(),
		})
	}
	for i, raw := range *doc.Skills {
		base := fmt.Sprintf("skills[%d]", i)
		id, err := requireString(raw.ID, base+".id")
		if err != nil {
			return nil, err
		}
		poolID, err := requireString(raw.Pool, base+".pool")
		if err != nil {
			return nil, err
		}
		cost, err := require(raw.Cost, base+".cost")
		if err != nil {
			return nil, err
		}
		if raw.Cooldown == nil {
			return nil, missing(base + ".cooldown")
		}
		cooldownUnit, err := requireString(raw.Cooldown.Unit, base+".cooldown.unit")
		if err != nil {
			return nil, err
		}
		if cooldownUnit != string(UnitTick) {
			return nil, domainErr(CodeVersion, base+".cooldown.unit", "version 3 cooldown expressions use ticks")
		}
		cooldown, err := require(raw.Cooldown.Expr, base+".cooldown.expr")
		if err != nil {
			return nil, err
		}
		if err := cooldown.Validate(base + ".cooldown.expr"); err != nil {
			return nil, err
		}
		if raw.Cast == nil {
			return nil, missing(base + ".cast")
		}
		castUnit, err := requireString(raw.Cast.Unit, base+".cast.unit")
		if err != nil {
			return nil, err
		}
		castValue, err := require(raw.Cast.Value, base+".cast.value")
		if err != nil {
			return nil, err
		}
		castTicks, err := ConvertSerializedDuration(*castValue, Unit(castUnit), *tickMillis, rounding, base+".cast.value")
		if err != nil {
			return nil, err
		}
		cdr, err := require(raw.CDRBp, base+".cdrBp")
		if err != nil {
			return nil, err
		}
		priority, err := require(raw.Priority, base+".priority")
		if err != nil {
			return nil, err
		}
		group := ""
		if raw.Group != nil {
			group = *raw.Group
			project.Present[skillPresence(id, "group")] = true
		}
		enabled := true
		if raw.Enabled != nil {
			enabled = *raw.Enabled
			project.Present[skillPresence(id, "enabled")] = true
		}
		description := ""
		if raw.Description != nil {
			description = *raw.Description
			project.Present[skillPresence(id, "description")] = true
		}
		var tags []string
		if raw.Tags != nil {
			tags = append([]string(nil), (*raw.Tags)...)
			project.Present[skillPresence(id, "tags")] = true
		}
		project.Skills = append(project.Skills, Skill{
			ID:          id,
			PoolID:      poolID,
			Cost:        *cost,
			Cooldown:    cooldown.Clone(),
			CastTicks:   castTicks,
			CDRBp:       *cdr,
			Priority:    *priority,
			SharedGroup: group,
			Enabled:     enabled,
			Description: description,
			Tags:        tags,
		})
	}
	if doc.ConflictPolicy != nil {
		project.Present["conflictPolicy"] = true
	}
	if err := project.Normalize(); err != nil {
		return nil, err
	}
	return project, nil
}

func skillPresence(skillID, field string) string {
	return fmt.Sprintf("skills[%s].%s", skillID, field)
}

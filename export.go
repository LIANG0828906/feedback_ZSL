package skillcore

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

type ExportResult struct {
	Version    int         `json:"version"`
	Data       []byte      `json:"-"`
	Project    *Project    `json:"project"`
	Losses     []Loss      `json:"losses,omitempty"`
	FillTraces []FillTrace `json:"fillTraces,omitempty"`
}

// Export converts an internal project to a concrete schema version. Safe
// degradations are returned in Losses with code E_LOSSY. A loss that would
// change cooldown, resource or availability semantics aborts the export.
func (p *Project) Export(version int) (*ExportResult, error) {
	if p == nil {
		return nil, domainErr(CodeInput, "", "nil project")
	}
	clone := p.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	if version < 1 || version > 3 {
		return nil, unsupportedVersion("version", version)
	}
	losses := make([]Loss, 0)
	if clone.ConflictPolicy == ConflictStrictPriority && version < 3 {
		losses = append(losses, Loss{
			Code:        CodeLossy,
			Path:        "conflictPolicy",
			Value:       string(clone.ConflictPolicy),
			Disposition: "reject-export",
			Reason:      "format version does not support strict-priority conflict resolution",
		})
		return nil, lossyError("conflictPolicy", "cannot preserve conflict semantics", losses)
	}

	var (
		data []byte
		err  error
	)
	switch version {
	case 1:
		data, losses, err = exportV1(clone, losses)
	case 2:
		data, losses, err = exportV2(clone, losses)
	case 3:
		data, losses, err = exportV3(clone, losses)
	}
	if err != nil {
		return nil, err
	}

	loaded, err := LoadVersion(data, version)
	if err != nil {
		return nil, wrapErr(CodeInternal, fmt.Sprintf("version[%d]", version), "exported document failed read-back validation", err)
	}
	if !semanticallyEquivalent(clone, loaded) {
		losses = append(losses, Loss{
			Code:        CodeLossy,
			Path:        "project",
			Disposition: "reject-export",
			Reason:      "read-back document does not restore the same cooldown/resource semantics",
		})
		return nil, lossyError("project", "read-back semantic mismatch", losses)
	}
	traces := upgradeTraces(clone, version)
	return &ExportResult{
		Version:    version,
		Data:       data,
		Project:    loaded,
		Losses:     losses,
		FillTraces: traces,
	}, nil
}

func lossyError(path, message string, losses []Loss) *Error {
	return &Error{
		Code:    CodeLossy,
		Path:    path,
		Field:   path,
		Message: message,
		Losses:  append([]Loss(nil), losses...),
	}
}

func addLoss(losses *[]Loss, path, value, disposition, reason string) {
	*losses = append(*losses, Loss{
		Code:        CodeLossy,
		Path:        path,
		Value:       value,
		Disposition: disposition,
		Reason:      reason,
	})
}

func flatDuration(expr Expr, rounding Rounding, path string, losses *[]Loss) (int64, error) {
	value, constant, err := expr.Constant(rounding, path)
	if err != nil {
		return 0, err
	}
	if !constant {
		addLoss(losses, path, exprDescription(expr), "reject-export",
			"tick-dependent expression cannot be represented by a flat legacy field")
		return 0, lossyError(path, "expression cannot be flattened without changing semantics", *losses)
	}
	if !expr.IsConst() {
		addLoss(losses, path, exprDescription(expr), "fold-to-flat",
			"constant expression tree folded to the target flat field")
	}
	return value, nil
}

func flatResourceExpr(expr Expr, rounding Rounding, path string, losses *[]Loss) (int64, error) {
	value, err := flatDuration(expr, rounding, path, losses)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, domainErr(CodeInput, path, "resource regeneration must be non-negative")
	}
	return value, nil
}

func exprDescription(expr Expr) string {
	data, err := json.Marshal(expr)
	if err != nil {
		return expr.Op
	}
	return string(data)
}

type v1ExportPool struct {
	ID           string `json:"pool_id"`
	Capacity     int64  `json:"capacity"`
	Initial      int64  `json:"initial"`
	RegenPerTick int64  `json:"regen_per_tick"`
}

type v1ExportSkill struct {
	ID          string `json:"skill_id"`
	PoolID      string `json:"pool_id"`
	Cost        int64  `json:"cost"`
	Cooldown    int64  `json:"cooldown_ticks"`
	Cast        int64  `json:"cast_ticks"`
	CDRPermille int64  `json:"cooldown_reduction_permille"`
	Priority    int    `json:"priority"`
	SharedGroup string `json:"shared_group"`
}

type v1ExportDocument struct {
	SchemaVersion int             `json:"schema_version"`
	TickMillis    int64           `json:"tick_millis"`
	ResourceScale int64           `json:"resource_scale"`
	Rounding      Rounding        `json:"rounding"`
	Pools         []v1ExportPool  `json:"resource_pools"`
	Skills        []v1ExportSkill `json:"skills"`
}

func exportV1(p *Project, losses []Loss) ([]byte, []Loss, error) {
	doc := v1ExportDocument{
		SchemaVersion: 1,
		TickMillis:    p.TickMillis,
		ResourceScale: 1000,
		Rounding:      p.Rounding,
		Pools:         make([]v1ExportPool, 0, len(p.Pools)),
		Skills:        make([]v1ExportSkill, 0, len(p.Skills)),
	}
	for i := range p.Pools {
		pool := &p.Pools[i]
		path := fmt.Sprintf("pools[%s]", pool.ID)
		max, exact, err := InternalToScaleExact(pool.Max, 1000, path+".max")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".max", "resource", losses)
		}
		initial, exact, err := InternalToScaleExact(pool.Initial, 1000, path+".initial")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".initial", "resource", losses)
		}
		regen, err := flatResourceExpr(pool.Regen, p.Rounding, path+".regen", &losses)
		if err != nil {
			return nil, losses, err
		}
		regenPermille, exact, err := InternalToScaleExact(regen, 1000, path+".regen")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".regen", "resource", losses)
		}
		doc.Pools = append(doc.Pools, v1ExportPool{
			ID:           pool.ID,
			Capacity:     max,
			Initial:      initial,
			RegenPerTick: regenPermille,
		})
	}
	for i := range p.Skills {
		skill := &p.Skills[i]
		path := fmt.Sprintf("skills[%s]", skill.ID)
		if !skill.Enabled {
			addLoss(&losses, path+".enabled", "false", "reject-export",
				"version 1 has no enabled field and would activate a disabled skill")
			return nil, losses, lossyError(path+".enabled", "disabled skill cannot be represented", losses)
		}
		if skill.Description != "" {
			addLoss(&losses, path+".description", skill.Description, "drop-optional",
				"version 1 has no description field; field is non-semantic")
		}
		if len(skill.Tags) > 0 {
			addLoss(&losses, path+".tags", fmt.Sprint(skill.Tags), "drop-optional",
				"version 1 has no tags field; field is non-semantic")
		}
		cooldown, err := flatDuration(skill.Cooldown, p.Rounding, path+".cooldown", &losses)
		if err != nil {
			return nil, losses, err
		}
		cost, exact, err := InternalToScaleExact(skill.Cost, 1000, path+".cost")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".cost", "resource", losses)
		}
		cdr, exact, err := BPToPermilleExact(skill.CDRBp, path+".cdr")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".cdr", "permille", losses)
		}
		doc.Skills = append(doc.Skills, v1ExportSkill{
			ID:          skill.ID,
			PoolID:      skill.PoolID,
			Cost:        cost,
			Cooldown:    cooldown,
			Cast:        skill.CastTicks,
			CDRPermille: cdr,
			Priority:    skill.Priority,
			SharedGroup: skill.SharedGroup,
		})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, losses, wrapErr(CodeInternal, "version[1]", "marshal export", err)
	}
	return append(data, '\n'), losses, nil
}

func quantizeError(path, unit string, losses []Loss) *Error {
	addLoss(&losses, path, "", "reject-export",
		"value is not exactly representable at the target fixed-point scale")
	return lossyError(path, fmt.Sprintf("%s cannot be represented exactly at target scale", unit), losses)
}

type v2ExportPool struct {
	ID           string `json:"id"`
	Max          int64  `json:"max"`
	Initial      int64  `json:"initial"`
	RegenPerTick int64  `json:"regenPerTick"`
}

type v2ExportSkill struct {
	ID          string `json:"id"`
	Pool        string `json:"pool"`
	Cost        int64  `json:"cost"`
	CooldownMS  int64  `json:"cooldownMs"`
	CastMS      int64  `json:"castMs"`
	CDRBp       int64  `json:"cdrBp"`
	Priority    int    `json:"priority"`
	SharedGroup string `json:"sharedGroup"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description,omitempty"`
}

type v2ExportDocument struct {
	SchemaVersion int             `json:"schemaVersion"`
	MSPerTick     int64           `json:"msPerTick"`
	ResourceScale int64           `json:"resourceScale"`
	Rounding      Rounding        `json:"rounding"`
	Pools         []v2ExportPool  `json:"pools"`
	Skills        []v2ExportSkill `json:"abilities"`
}

func exportV2(p *Project, losses []Loss) ([]byte, []Loss, error) {
	doc := v2ExportDocument{
		SchemaVersion: 2,
		MSPerTick:     p.TickMillis,
		ResourceScale: InternalScale,
		Rounding:      p.Rounding,
		Pools:         make([]v2ExportPool, 0, len(p.Pools)),
		Skills:        make([]v2ExportSkill, 0, len(p.Skills)),
	}
	for i := range p.Pools {
		pool := &p.Pools[i]
		path := fmt.Sprintf("pools[%s]", pool.ID)
		regen, err := flatResourceExpr(pool.Regen, p.Rounding, path+".regen", &losses)
		if err != nil {
			return nil, losses, err
		}
		doc.Pools = append(doc.Pools, v2ExportPool{
			ID:           pool.ID,
			Max:          pool.Max,
			Initial:      pool.Initial,
			RegenPerTick: regen,
		})
	}
	for i := range p.Skills {
		skill := &p.Skills[i]
		path := fmt.Sprintf("skills[%s]", skill.ID)
		if len(skill.Tags) > 0 {
			addLoss(&losses, path+".tags", fmt.Sprint(skill.Tags), "drop-optional",
				"version 2 has no tags field; field is non-semantic")
		}
		cooldownTicks, err := flatDuration(skill.Cooldown, p.Rounding, path+".cooldown", &losses)
		if err != nil {
			return nil, losses, err
		}
		cooldownMS, exact, err := TicksToMillisExact(cooldownTicks, p.TickMillis, path+".cooldownMs")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".cooldownMs", "millisecond", losses)
		}
		castMS, exact, err := TicksToMillisExact(skill.CastTicks, p.TickMillis, path+".castMs")
		if err != nil {
			return nil, losses, err
		}
		if !exact {
			return nil, losses, quantizeError(path+".castMs", "millisecond", losses)
		}
		doc.Skills = append(doc.Skills, v2ExportSkill{
			ID:          skill.ID,
			Pool:        skill.PoolID,
			Cost:        skill.Cost,
			CooldownMS:  cooldownMS,
			CastMS:      castMS,
			CDRBp:       skill.CDRBp,
			Priority:    skill.Priority,
			SharedGroup: skill.SharedGroup,
			Enabled:     skill.Enabled,
			Description: skill.Description,
		})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, losses, wrapErr(CodeInternal, "version[2]", "marshal export", err)
	}
	return append(data, '\n'), losses, nil
}

type v3ExportPool struct {
	ID    string `json:"id"`
	Max   int64  `json:"max"`
	Start int64  `json:"start"`
	Regen Expr   `json:"regen"`
}

type v3ExportSkill struct {
	ID          string           `json:"id"`
	Pool        string           `json:"pool"`
	Cost        int64            `json:"cost"`
	Cooldown    v3ExportDuration `json:"cooldown"`
	Cast        v3ExportCast     `json:"cast"`
	CDRBp       int64            `json:"cdrBp"`
	Priority    int              `json:"priority"`
	Group       string           `json:"group,omitempty"`
	Enabled     bool             `json:"enabled"`
	Description string           `json:"description,omitempty"`
	Tags        []string         `json:"tags,omitempty"`
}

type v3ExportDuration struct {
	Unit string `json:"unit"`
	Expr Expr   `json:"expr"`
}

type v3ExportCast struct {
	Unit  string `json:"unit"`
	Value int64  `json:"value"`
}

type v3ExportClock struct {
	Unit          string `json:"unit"`
	MillisPerTick int64  `json:"millisPerTick"`
}

type v3ExportDocument struct {
	Format         string          `json:"format"`
	Version        int             `json:"version"`
	Clock          v3ExportClock   `json:"clock"`
	ResourceScale  int64           `json:"resourceScale"`
	Rounding       Rounding        `json:"rounding"`
	ConflictPolicy ConflictPolicy  `json:"conflictPolicy"`
	Pools          []v3ExportPool  `json:"pools"`
	Skills         []v3ExportSkill `json:"skills"`
}

func exportV3(p *Project, losses []Loss) ([]byte, []Loss, error) {
	doc := v3ExportDocument{
		Format:         "skill-rules",
		Version:        3,
		Clock:          v3ExportClock{Unit: string(UnitTick), MillisPerTick: p.TickMillis},
		ResourceScale:  InternalScale,
		Rounding:       p.Rounding,
		ConflictPolicy: p.ConflictPolicy,
		Pools:          make([]v3ExportPool, 0, len(p.Pools)),
		Skills:         make([]v3ExportSkill, 0, len(p.Skills)),
	}
	for i := range p.Pools {
		pool := &p.Pools[i]
		doc.Pools = append(doc.Pools, v3ExportPool{
			ID:    pool.ID,
			Max:   pool.Max,
			Start: pool.Initial,
			Regen: pool.Regen.Clone(),
		})
	}
	for i := range p.Skills {
		skill := &p.Skills[i]
		doc.Skills = append(doc.Skills, v3ExportSkill{
			ID:   skill.ID,
			Pool: skill.PoolID,
			Cost: skill.Cost,
			Cooldown: v3ExportDuration{
				Unit: string(UnitTick),
				Expr: skill.Cooldown.Clone(),
			},
			Cast: v3ExportCast{
				Unit:  string(UnitTick),
				Value: skill.CastTicks,
			},
			CDRBp:       skill.CDRBp,
			Priority:    skill.Priority,
			Group:       skill.SharedGroup,
			Enabled:     skill.Enabled,
			Description: skill.Description,
			Tags:        append([]string(nil), skill.Tags...),
		})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, losses, wrapErr(CodeInternal, "version[3]", "marshal export", err)
	}
	return append(data, '\n'), losses, nil
}

func upgradeTraces(source *Project, target int) []FillTrace {
	if target <= source.SourceVersion {
		return nil
	}
	var traces []FillTrace
	add := func(path, value, defaultValue, sourceDefault string) {
		traces = append(traces, FillTrace{
			Path:         path,
			OldValue:     "<absent>",
			DefaultValue: defaultValue,
			Source:       sourceDefault,
		})
	}
	for i := range source.Skills {
		skill := &source.Skills[i]
		path := fmt.Sprintf("skills[%s]", skill.ID)
		if source.SourceVersion == 1 {
			add(path+".enabled", "true", "true", "v2:declaration-default")
			add(path+".description", "", `""`, "v2:declaration-default")
		}
		if target == 3 {
			if source.SourceVersion < 3 {
				add(path+".tags", "[]", "[]", "v3:declaration-default")
			}
		}
	}
	if target == 3 && source.SourceVersion < 3 {
		add("conflictPolicy", string(ConflictPriorityThenID), string(ConflictPriorityThenID), "v3:declaration-default")
	}
	sort.Slice(traces, func(i, j int) bool { return traces[i].Path < traces[j].Path })
	return traces
}

func semanticallyEquivalent(a, b *Project) bool {
	if a == nil || b == nil || a.TickMillis != b.TickMillis || a.Rounding != b.Rounding ||
		a.ConflictPolicy != b.ConflictPolicy || len(a.Pools) != len(b.Pools) || len(a.Skills) != len(b.Skills) {
		return false
	}
	for i := range a.Pools {
		left, right := a.Pools[i], b.Pools[i]
		if left.ID != right.ID || left.Max != right.Max || left.Initial != right.Initial ||
			!exprEquivalent(left.Regen, right.Regen, a.Rounding) {
			return false
		}
	}
	for i := range a.Skills {
		left, right := a.Skills[i], b.Skills[i]
		if left.ID != right.ID || left.PoolID != right.PoolID || left.Cost != right.Cost ||
			!exprEquivalent(left.Cooldown, right.Cooldown, a.Rounding) ||
			left.CastTicks != right.CastTicks || left.CDRBp != right.CDRBp ||
			left.Priority != right.Priority || left.SharedGroup != right.SharedGroup ||
			left.Enabled != right.Enabled {
			return false
		}
	}
	return true
}

func exprEquivalent(a, b Expr, rounding Rounding) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	if a.ContainsTick() || b.ContainsTick() {
		return false
	}
	av, errA := a.Eval(ExprContext{}, rounding, "expression")
	bv, errB := b.Eval(ExprContext{}, rounding, "expression")
	return errA == nil && errB == nil && av == bv
}

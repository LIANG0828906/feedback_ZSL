package skillcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	InternalScale    int64 = 10000
	AlgorithmVersion       = "skillbudget.engine.v1"
)

type Rounding string

const (
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
	RoundTrunc   Rounding = "trunc"
)

func ParseRounding(s string) (Rounding, error) {
	switch Rounding(s) {
	case RoundFloor, RoundCeil, RoundNearest, RoundTrunc:
		return Rounding(s), nil
	default:
		return "", domainErr(CodeVersion, "rounding", fmt.Sprintf("unknown rounding mode %q", s))
	}
}

type ConflictPolicy string

const (
	ConflictPriorityThenID ConflictPolicy = "priority_then_id"
	ConflictStrictPriority ConflictPolicy = "strict_priority"
)

func ParseConflictPolicy(s string) (ConflictPolicy, error) {
	switch ConflictPolicy(s) {
	case "", ConflictPriorityThenID:
		return ConflictPriorityThenID, nil
	case ConflictStrictPriority:
		return ConflictStrictPriority, nil
	default:
		return "", domainErr(CodeVersion, "conflictPolicy", fmt.Sprintf("unknown conflict policy %q", s))
	}
}

// Project is the normalized, version-independent rule model. All durations are
// ticks, all resource amounts and coefficients use 1/10000 units.
type Project struct {
	SourceVersion  int            `json:"sourceVersion"`
	TickMillis     int64          `json:"tickMillis"`
	Rounding       Rounding       `json:"rounding"`
	ConflictPolicy ConflictPolicy `json:"conflictPolicy"`
	Pools          []ResourcePool `json:"pools"`
	Skills         []Skill        `json:"skills"`

	// Present tracks optional fields observed in the source document. It is
	// intentionally excluded from semantic hashing and serialized checkpoints.
	Present map[string]bool `json:"-"`
}

type ResourcePool struct {
	ID      string `json:"id"`
	Max     int64  `json:"max"`
	Initial int64  `json:"initial"`
	Regen   Expr   `json:"regen"`
}

type Skill struct {
	ID          string   `json:"id"`
	PoolID      string   `json:"pool"`
	Cost        int64    `json:"cost"`
	Cooldown    Expr     `json:"cooldown"`
	CastTicks   int64    `json:"castTicks"`
	CDRBp       int64    `json:"cdrBp"`
	Priority    int      `json:"priority"`
	SharedGroup string   `json:"sharedGroup,omitempty"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Normalize validates, sorts and canonicalizes a project.
func (p *Project) Normalize() error {
	if p == nil {
		return domainErr(CodeInput, "", "nil project")
	}
	if p.SourceVersion < 1 || p.SourceVersion > 3 {
		return domainErr(CodeVersion, "schemaVersion", fmt.Sprintf("unsupported source version %d", p.SourceVersion))
	}
	if p.TickMillis <= 0 {
		return domainErr(CodeVersion, "tickMillis", "must be greater than zero")
	}
	switch p.Rounding {
	case RoundFloor, RoundCeil, RoundNearest, RoundTrunc:
	default:
		return domainErr(CodeVersion, "rounding", fmt.Sprintf("unknown rounding mode %q", p.Rounding))
	}
	if p.ConflictPolicy == "" {
		p.ConflictPolicy = ConflictPriorityThenID
	}
	if _, err := ParseConflictPolicy(string(p.ConflictPolicy)); err != nil {
		return err
	}
	sort.Slice(p.Pools, func(i, j int) bool { return p.Pools[i].ID < p.Pools[j].ID })
	sort.Slice(p.Skills, func(i, j int) bool { return p.Skills[i].ID < p.Skills[j].ID })

	poolIDs := make(map[string]struct{}, len(p.Pools))
	for i := range p.Pools {
		pool := &p.Pools[i]
		path := fmt.Sprintf("pools[%d]", i)
		if pool.ID == "" {
			return domainErr(CodeVersion, path+".id", "required field is empty")
		}
		if _, exists := poolIDs[pool.ID]; exists {
			return domainErr(CodeInput, path+".id", "duplicate pool id")
		}
		poolIDs[pool.ID] = struct{}{}
		if pool.Max < 0 || pool.Initial < 0 || pool.Initial > pool.Max {
			return domainErr(CodeInput, path, "resource bounds must satisfy 0 <= initial <= max")
		}
		if err := pool.Regen.Validate(path + ".regen"); err != nil {
			return err
		}
	}
	skillIDs := make(map[string]struct{}, len(p.Skills))
	groups := make(map[string]struct{})
	for i := range p.Skills {
		skill := &p.Skills[i]
		path := fmt.Sprintf("skills[%d]", i)
		if skill.ID == "" {
			return domainErr(CodeVersion, path+".id", "required field is empty")
		}
		if _, exists := skillIDs[skill.ID]; exists {
			return domainErr(CodeInput, path+".id", "duplicate skill id")
		}
		skillIDs[skill.ID] = struct{}{}
		if _, ok := poolIDs[skill.PoolID]; !ok {
			return domainErr(CodeInput, path+".pool", fmt.Sprintf("unknown resource pool %q", skill.PoolID))
		}
		if skill.Cost < 0 || skill.CastTicks < 0 {
			return domainErr(CodeInput, path, "cost and cast duration must be non-negative")
		}
		if skill.CDRBp < 0 || skill.CDRBp > InternalScale {
			return domainErr(CodeInput, path+".cdrBp", "must be in [0,10000]")
		}
		if err := skill.Cooldown.Validate(path + ".cooldown"); err != nil {
			return err
		}
		if skill.Priority < 0 {
			return domainErr(CodeInput, path+".priority", "must be non-negative")
		}
		sort.Strings(skill.Tags)
		if skill.SharedGroup != "" {
			groups[skill.SharedGroup] = struct{}{}
		}
	}
	_ = groups
	if p.Present == nil {
		p.Present = make(map[string]bool)
	}
	return nil
}

func (p *Project) PoolByID(id string) *ResourcePool {
	for i := range p.Pools {
		if p.Pools[i].ID == id {
			return &p.Pools[i]
		}
	}
	return nil
}

func (p *Project) SkillByID(id string) *Skill {
	for i := range p.Skills {
		if p.Skills[i].ID == id {
			return &p.Skills[i]
		}
	}
	return nil
}

func (p *Project) SkillsInGroup(group string) []*Skill {
	if group == "" {
		return nil
	}
	var out []*Skill
	for i := range p.Skills {
		if p.Skills[i].SharedGroup == group {
			out = append(out, &p.Skills[i])
		}
	}
	return out
}

// ContentHash hashes semantic rule content only. Source format and presence
// metadata do not alter the hash.
func (p *Project) ContentHash() string {
	type semanticSkill struct {
		ID          string `json:"id"`
		PoolID      string `json:"pool"`
		Cost        int64  `json:"cost"`
		Cooldown    Expr   `json:"cooldown"`
		CastTicks   int64  `json:"castTicks"`
		CDRBp       int64  `json:"cdrBp"`
		Priority    int    `json:"priority"`
		SharedGroup string `json:"sharedGroup,omitempty"`
		Enabled     bool   `json:"enabled"`
	}
	type semantic struct {
		TickMillis     int64           `json:"tickMillis"`
		Rounding       Rounding        `json:"rounding"`
		ConflictPolicy ConflictPolicy  `json:"conflictPolicy"`
		Pools          []ResourcePool  `json:"pools"`
		Skills         []semanticSkill `json:"skills"`
	}
	copyProject := *p
	copyProject.Pools = append([]ResourcePool(nil), p.Pools...)
	for i := range copyProject.Pools {
		copyProject.Pools[i].Regen = canonicalHashExpr(copyProject.Pools[i].Regen, copyProject.Rounding)
	}
	skills := make([]semanticSkill, len(p.Skills))
	for i := range p.Skills {
		skill := p.Skills[i]
		skills[i] = semanticSkill{
			ID:          skill.ID,
			PoolID:      skill.PoolID,
			Cost:        skill.Cost,
			Cooldown:    canonicalHashExpr(skill.Cooldown, p.Rounding),
			CastTicks:   skill.CastTicks,
			CDRBp:       skill.CDRBp,
			Priority:    skill.Priority,
			SharedGroup: skill.SharedGroup,
			Enabled:     skill.Enabled,
		}
	}
	sort.Slice(copyProject.Pools, func(i, j int) bool { return copyProject.Pools[i].ID < copyProject.Pools[j].ID })
	sort.Slice(skills, func(i, j int) bool { return skills[i].ID < skills[j].ID })
	data, _ := json.Marshal(semantic{
		TickMillis:     copyProject.TickMillis,
		Rounding:       copyProject.Rounding,
		ConflictPolicy: copyProject.ConflictPolicy,
		Pools:          copyProject.Pools,
		Skills:         skills,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func canonicalHashExpr(expr Expr, rounding Rounding) Expr {
	if expr.ContainsTick() {
		return expr.Clone()
	}
	value, err := expr.Eval(ExprContext{}, rounding, "expression")
	if err == nil {
		return Const(value)
	}
	return expr.Clone()
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.Pools = append([]ResourcePool(nil), p.Pools...)
	out.Skills = make([]Skill, len(p.Skills))
	for i := range p.Skills {
		out.Skills[i] = p.Skills[i]
		out.Skills[i].Tags = append([]string(nil), p.Skills[i].Tags...)
		out.Skills[i].Cooldown = p.Skills[i].Cooldown.Clone()
	}
	out.Present = make(map[string]bool, len(p.Present))
	for k, v := range p.Present {
		out.Present[k] = v
	}
	return &out
}

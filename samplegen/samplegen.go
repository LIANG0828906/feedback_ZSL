package samplegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"skillbudget"
)

const (
	TickMillis = int64(4)
	TotalTicks = int64(28)
	WindowSize = int64(7)
)

type Bundle struct {
	Seed       int64                    `json:"seed"`
	TickMillis int64                    `json:"tickMillis"`
	TotalTicks int64                    `json:"totalTicks"`
	WindowSize int64                    `json:"windowSize"`
	RuleHash   string                   `json:"ruleHash"`
	Project    *skillcore.Project       `json:"project"`
	Versions   map[int][]byte           `json:"-"`
	Intents    []skillcore.Intent       `json:"intents"`
	Truth      []skillcore.TickSnapshot `json:"truth"`
}

type specPool struct {
	ID      string
	Max     int64
	Initial int64
	Regen   int64
}

type specSkill struct {
	ID          string
	Pool        string
	Cost        int64
	Cooldown    int64
	Cast        int64
	CDR         int64
	Priority    int
	Group       string
	Description string
	Tags        []string
}

type spec struct {
	Pools  []specPool
	Skills []specSkill
}

// Generate creates a deterministic rule project, all three serialized schema
// versions, an intent script and an independently constructed per-tick oracle.
func Generate(seed int64) (*Bundle, error) {
	rng := splitMix64{state: uint64(seed)}
	manaMax := roundTo(int64(8000+rng.Intn(4001)), 100)
	manaStart := roundTo(int64(3500+rng.Intn(3001)), 100)
	manaRegen := int64(40 + rng.Intn(10)*10)
	energyMax := roundTo(int64(6000+rng.Intn(2001)), 100)
	energyStart := roundTo(int64(2500+rng.Intn(2501)), 100)
	energyRegen := int64(30 + rng.Intn(8)*10)

	projectSpec := spec{
		Pools: []specPool{
			{ID: "mana", Max: manaMax, Initial: manaStart, Regen: manaRegen},
			{ID: "energy", Max: energyMax, Initial: energyStart, Regen: energyRegen},
		},
		Skills: []specSkill{
			{
				ID: "arc_nova", Pool: "mana", Cost: 2800, Cooldown: 16, Cast: 2,
				CDR: 1000, Priority: 30, Group: "burst", Description: "area burst",
				Tags: []string{"magic", "area"},
			},
			{
				ID: "ice_lance", Pool: "mana", Cost: 1600, Cooldown: 12, Cast: 1,
				CDR: 500, Priority: 20, Group: "burst", Description: "line projectile",
				Tags: []string{"magic", "projectile"},
			},
			{
				ID: "heal", Pool: "mana", Cost: 2400, Cooldown: 20, Cast: 4,
				CDR: 0, Priority: 20, Group: "support", Description: "restore ally",
			},
			{
				ID: "dash", Pool: "energy", Cost: 1800, Cooldown: 8, Cast: 0,
				CDR: 1000, Priority: 25, Group: "mobility",
			},
			{
				ID: "guard", Pool: "energy", Cost: 1200, Cooldown: 12, Cast: 2,
				CDR: 0, Priority: 10, Group: "mobility", Description: "raise shield",
			},
		},
	}
	// Keep every amount representable in the version-1 permille scale.
	for i := range projectSpec.Skills {
		projectSpec.Skills[i].Cost = roundTo(projectSpec.Skills[i].Cost, 100)
		projectSpec.Skills[i].CDR = roundTo(projectSpec.Skills[i].CDR, 10)
	}
	intents := generateIntents(&rng, projectSpec)
	truth, err := constructTruth(projectSpec, intents)
	if err != nil {
		return nil, err
	}
	internalProject := buildInternalProject(projectSpec)
	if err := internalProject.Normalize(); err != nil {
		return nil, err
	}
	versions, err := encodeVersions(projectSpec)
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{
		Seed:       seed,
		TickMillis: TickMillis,
		TotalTicks: TotalTicks,
		WindowSize: WindowSize,
		RuleHash:   internalProject.ContentHash(),
		Project:    internalProject,
		Versions:   versions,
		Intents:    intents,
		Truth:      truth,
	}
	return bundle, nil
}

type splitMix64 struct {
	state uint64
}

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitMix64) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func roundTo(value, step int64) int64 {
	if step <= 1 {
		return value
	}
	return (value / step) * step
}

func generateIntents(rng *splitMix64, spec spec) []skillcore.Intent {
	targets := []string{"enemy-alpha", "enemy-beta", "enemy-gamma", "point-a", "point-b"}
	skills := make([]string, 0, len(spec.Skills))
	for _, skill := range spec.Skills {
		skills = append(skills, skill.ID)
	}
	sort.Strings(skills)
	var intents []skillcore.Intent
	add := func(tick int64, skillID string) {
		target := targets[rng.Intn(len(targets))]
		if skillID == "heal" {
			target = "ally-alpha"
		}
		intents = append(intents, skillcore.Intent{
			Tick:     tick,
			SkillID:  skillID,
			TargetID: target,
			Sequence: int64(len(intents) + 1),
		})
	}
	// Fixed conflict windows make budget and shared-group arbitration visible;
	// the selected target and additional intents remain seed-dependent.
	add(0, "arc_nova")
	add(0, "ice_lance")
	add(2, "heal")
	add(2, "ice_lance")
	for tick := int64(3); tick < TotalTicks; tick++ {
		switch {
		case tick%7 == 5:
			add(tick, "dash")
			add(tick, "guard")
		case tick%9 == 4:
			add(tick, "ice_lance")
			add(tick, "heal")
		default:
			if rng.Intn(100) < 42 {
				add(tick, skills[rng.Intn(len(skills))])
			}
			if rng.Intn(100) < 18 {
				add(tick, skills[rng.Intn(len(skills))])
			}
		}
	}
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].Tick != intents[j].Tick {
			return intents[i].Tick < intents[j].Tick
		}
		return intents[i].Sequence < intents[j].Sequence
	})
	return intents
}

func buildInternalProject(s spec) *skillcore.Project {
	project := &skillcore.Project{
		SourceVersion:  3,
		TickMillis:     TickMillis,
		Rounding:       skillcore.RoundFloor,
		ConflictPolicy: skillcore.ConflictPriorityThenID,
		Present:        map[string]bool{},
	}
	for _, pool := range s.Pools {
		project.Pools = append(project.Pools, skillcore.ResourcePool{
			ID:      pool.ID,
			Max:     pool.Max,
			Initial: pool.Initial,
			Regen: skillcore.Expr{Op: "add", Args: []skillcore.Expr{
				skillcore.Const(pool.Regen), skillcore.Const(0),
			}},
		})
	}
	for _, skill := range s.Skills {
		project.Skills = append(project.Skills, skillcore.Skill{
			ID:          skill.ID,
			PoolID:      skill.Pool,
			Cost:        skill.Cost,
			Cooldown:    skillcore.Expr{Op: "add", Args: []skillcore.Expr{skillcore.Const(skill.Cooldown), skillcore.Const(0)}},
			CastTicks:   skill.Cast,
			CDRBp:       skill.CDR,
			Priority:    skill.Priority,
			SharedGroup: skill.Group,
			Enabled:     true,
			Description: skill.Description,
			Tags:        append([]string(nil), skill.Tags...),
		})
	}
	return project
}

// constructTruth is deliberately an independent implementation of the public
// tick semantics. It consumes the generator spec, never the engine.
func constructTruth(s spec, intents []skillcore.Intent) ([]skillcore.TickSnapshot, error) {
	pools := append([]specPool(nil), s.Pools...)
	skills := append([]specSkill(nil), s.Skills...)
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	sort.Slice(skills, func(i, j int) bool { return skills[i].ID < skills[j].ID })
	poolAmount := make(map[string]int64, len(pools))
	for _, pool := range pools {
		poolAmount[pool.ID] = pool.Initial
	}
	cooldown := make(map[string]int64, len(skills))
	castRemaining := make(map[string]int64, len(skills))
	byID := make(map[string]specSkill, len(skills))
	groups := make(map[string][]specSkill)
	for _, skill := range skills {
		byID[skill.ID] = skill
		if skill.Group != "" {
			groups[skill.Group] = append(groups[skill.Group], skill)
		}
	}
	byTick := make(map[int64][]skillcore.Intent)
	for _, intent := range intents {
		byTick[intent.Tick] = append(byTick[intent.Tick], intent)
	}
	truth := make([]skillcore.TickSnapshot, 0, TotalTicks)
	for tick := int64(0); tick < TotalTicks; tick++ {
		for _, pool := range pools {
			amount := poolAmount[pool.ID] + pool.Regen
			if amount > pool.Max {
				amount = pool.Max
			}
			poolAmount[pool.ID] = amount
		}
		for _, skill := range skills {
			if cooldown[skill.ID] > 0 {
				cooldown[skill.ID]--
			}
			if castRemaining[skill.ID] > 0 {
				castRemaining[skill.ID]--
			}
		}
		tickIntents := append([]skillcore.Intent(nil), byTick[tick]...)
		sort.Slice(tickIntents, func(i, j int) bool {
			left, right := byID[tickIntents[i].SkillID], byID[tickIntents[j].SkillID]
			if left.Priority != right.Priority {
				return left.Priority > right.Priority
			}
			if tickIntents[i].SkillID != tickIntents[j].SkillID {
				return tickIntents[i].SkillID < tickIntents[j].SkillID
			}
			if tickIntents[i].Sequence != tickIntents[j].Sequence {
				return tickIntents[i].Sequence < tickIntents[j].Sequence
			}
			return tickIntents[i].TargetID < tickIntents[j].TargetID
		})
		groupUsed := make(map[string]string)
		decisions := make([]skillcore.Decision, 0, len(tickIntents))
		for order, intent := range tickIntents {
			skill := byID[intent.SkillID]
			conflictType := truthConflictType(byID, tickIntents, order)
			decision := skillcore.Decision{
				Tick: tick, Order: order, SkillID: skill.ID, TargetID: intent.TargetID,
				Priority: skill.Priority, ConflictType: conflictType,
				PoolAfter: poolAmount[skill.Pool], CooldownAfter: cooldown[skill.ID],
			}
			reject := func(reason string) {
				decision.Result = "rejected"
				decision.Reason = reason
				decisions = append(decisions, decision)
			}
			if cooldown[skill.ID] > 0 || castRemaining[skill.ID] > 0 {
				reject("cooldown")
				continue
			}
			if skill.Group != "" {
				if caster, ok := groupUsed[skill.Group]; ok && caster != skill.ID {
					reject("shared_cooldown")
					continue
				}
			}
			if poolAmount[skill.Pool] < skill.Cost {
				reject("insufficient_resource")
				continue
			}
			effective := skill.Cooldown * (10000 - skill.CDR) / 10000
			poolAmount[skill.Pool] -= skill.Cost
			cooldown[skill.ID] = effective
			castRemaining[skill.ID] = skill.Cast
			if skill.Group != "" {
				for _, member := range groups[skill.Group] {
					if cooldown[member.ID] < effective {
						cooldown[member.ID] = effective
					}
				}
				groupUsed[skill.Group] = skill.ID
			}
			decision.Result = "accepted"
			decision.Reason = "cast"
			decision.PoolAfter = poolAmount[skill.Pool]
			decision.CooldownAfter = cooldown[skill.ID]
			decisions = append(decisions, decision)
		}
		snapshot := skillcore.TickSnapshot{
			Tick:      tick,
			Pools:     make([]skillcore.PoolSnapshot, 0, len(pools)),
			Skills:    make([]skillcore.SkillSnapshot, 0, len(skills)),
			Decisions: decisions,
		}
		for _, pool := range pools {
			snapshot.Pools = append(snapshot.Pools, skillcore.PoolSnapshot{
				ID: pool.ID, Amount: poolAmount[pool.ID], Max: pool.Max,
			})
		}
		for _, skill := range skills {
			timersReady := cooldown[skill.ID] == 0 && castRemaining[skill.ID] == 0
			affordable := poolAmount[skill.Pool] >= skill.Cost
			snapshot.Skills = append(snapshot.Skills, skillcore.SkillSnapshot{
				ID: skill.ID, Enabled: true, TimersReady: timersReady,
				Affordable: affordable, Available: timersReady && affordable,
				CooldownTicks: cooldown[skill.ID], CastRemainingTicks: castRemaining[skill.ID],
			})
		}
		truth = append(truth, snapshot)
	}
	return truth, nil
}

func truthConflictType(skills map[string]specSkill, intents []skillcore.Intent, index int) string {
	skill := skills[intents[index].SkillID]
	resource, group := false, false
	for i, other := range intents {
		if i == index {
			continue
		}
		otherSkill := skills[other.SkillID]
		if skill.Pool == otherSkill.Pool {
			resource = true
		}
		if skill.Group != "" && skill.Group == otherSkill.Group {
			group = true
		}
	}
	switch {
	case resource && group:
		return "resource+shared_cooldown"
	case resource:
		return "resource"
	case group:
		return "shared_cooldown"
	default:
		return "none"
	}
}

type v1PoolJSON struct {
	ID           string `json:"pool_id"`
	Capacity     int64  `json:"capacity"`
	Initial      int64  `json:"initial"`
	RegenPerTick int64  `json:"regen_per_tick"`
}

type v1SkillJSON struct {
	ID          string `json:"skill_id"`
	PoolID      string `json:"pool_id"`
	Cost        int64  `json:"cost"`
	Cooldown    int64  `json:"cooldown_ticks"`
	Cast        int64  `json:"cast_ticks"`
	CDRPermille int64  `json:"cooldown_reduction_permille"`
	Priority    int    `json:"priority"`
	SharedGroup string `json:"shared_group"`
}

type v1DocumentJSON struct {
	SchemaVersion int           `json:"schema_version"`
	TickMillis    int64         `json:"tick_millis"`
	ResourceScale int64         `json:"resource_scale"`
	Rounding      string        `json:"rounding"`
	Pools         []v1PoolJSON  `json:"resource_pools"`
	Skills        []v1SkillJSON `json:"skills"`
}

type v2PoolJSON struct {
	ID           string `json:"id"`
	Max          int64  `json:"max"`
	Initial      int64  `json:"initial"`
	RegenPerTick int64  `json:"regenPerTick"`
}

type v2SkillJSON struct {
	ID          string  `json:"id"`
	Pool        string  `json:"pool"`
	Cost        int64   `json:"cost"`
	CooldownMS  int64   `json:"cooldownMs"`
	CastMS      int64   `json:"castMs"`
	CDRBp       int64   `json:"cdrBp"`
	Priority    int     `json:"priority"`
	SharedGroup string  `json:"sharedGroup"`
	Enabled     *bool   `json:"enabled,omitempty"`
	Description *string `json:"description,omitempty"`
}

type v2DocumentJSON struct {
	SchemaVersion int           `json:"schemaVersion"`
	MSPerTick     int64         `json:"msPerTick"`
	ResourceScale int64         `json:"resourceScale"`
	Rounding      string        `json:"rounding"`
	Pools         []v2PoolJSON  `json:"pools"`
	Abilities     []v2SkillJSON `json:"abilities"`
}

type v3ClockJSON struct {
	Unit          string `json:"unit"`
	MillisPerTick int64  `json:"millisPerTick"`
}

type v3PoolJSON struct {
	ID    string         `json:"id"`
	Max   int64          `json:"max"`
	Start int64          `json:"start"`
	Regen skillcore.Expr `json:"regen"`
}

type v3DurationJSON struct {
	Unit string         `json:"unit"`
	Expr skillcore.Expr `json:"expr"`
}

type v3CastJSON struct {
	Unit  string `json:"unit"`
	Value int64  `json:"value"`
}

type v3SkillJSON struct {
	ID          string         `json:"id"`
	Pool        string         `json:"pool"`
	Cost        int64          `json:"cost"`
	Cooldown    v3DurationJSON `json:"cooldown"`
	Cast        v3CastJSON     `json:"cast"`
	CDRBp       int64          `json:"cdrBp"`
	Priority    int            `json:"priority"`
	Group       string         `json:"group,omitempty"`
	Enabled     *bool          `json:"enabled,omitempty"`
	Description string         `json:"description,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
}

type v3DocumentJSON struct {
	Format         string        `json:"format"`
	Version        int           `json:"version"`
	Clock          v3ClockJSON   `json:"clock"`
	ResourceScale  int64         `json:"resourceScale"`
	Rounding       string        `json:"rounding"`
	ConflictPolicy string        `json:"conflictPolicy"`
	Pools          []v3PoolJSON  `json:"pools"`
	Skills         []v3SkillJSON `json:"skills"`
}

func encodeVersions(s spec) (map[int][]byte, error) {
	pools := append([]specPool(nil), s.Pools...)
	skills := append([]specSkill(nil), s.Skills...)
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	sort.Slice(skills, func(i, j int) bool { return skills[i].ID < skills[j].ID })

	v1 := v1DocumentJSON{
		SchemaVersion: 1, TickMillis: TickMillis, ResourceScale: 1000, Rounding: "floor",
	}
	v2 := v2DocumentJSON{
		SchemaVersion: 2, MSPerTick: TickMillis, ResourceScale: 10000, Rounding: "floor",
	}
	v3 := v3DocumentJSON{
		Format: "skill-rules", Version: 3,
		Clock:          v3ClockJSON{Unit: "tick", MillisPerTick: TickMillis},
		ResourceScale:  10000,
		Rounding:       "floor",
		ConflictPolicy: "priority_then_id",
	}
	for _, pool := range pools {
		v1.Pools = append(v1.Pools, v1PoolJSON{
			ID: pool.ID, Capacity: pool.Max / 10, Initial: pool.Initial / 10, RegenPerTick: pool.Regen / 10,
		})
		v2.Pools = append(v2.Pools, v2PoolJSON{
			ID: pool.ID, Max: pool.Max, Initial: pool.Initial, RegenPerTick: pool.Regen,
		})
		v3.Pools = append(v3.Pools, v3PoolJSON{
			ID: pool.ID, Max: pool.Max, Start: pool.Initial,
			Regen: skillcore.Expr{Op: "add", Args: []skillcore.Expr{skillcore.Const(pool.Regen), skillcore.Const(0)}},
		})
	}
	for index, skill := range skills {
		v1.Skills = append(v1.Skills, v1SkillJSON{
			ID: skill.ID, PoolID: skill.Pool, Cost: skill.Cost / 10,
			Cooldown: skill.Cooldown, Cast: skill.Cast, CDRPermille: skill.CDR / 10,
			Priority: skill.Priority, SharedGroup: skill.Group,
		})
		var enabled *bool
		if index%2 == 0 {
			value := true
			enabled = &value
		}
		var description *string
		if skill.Description != "" {
			value := skill.Description
			description = &value
		}
		v2.Abilities = append(v2.Abilities, v2SkillJSON{
			ID: skill.ID, Pool: skill.Pool, Cost: skill.Cost,
			CooldownMS: skill.Cooldown * TickMillis, CastMS: skill.Cast * TickMillis,
			CDRBp: skill.CDR, Priority: skill.Priority, SharedGroup: skill.Group,
			Enabled: enabled, Description: description,
		})
		enabledV3 := true
		v3.Skills = append(v3.Skills, v3SkillJSON{
			ID: skill.ID, Pool: skill.Pool, Cost: skill.Cost,
			Cooldown: v3DurationJSON{
				Unit: "tick",
				Expr: skillcore.Expr{Op: "add", Args: []skillcore.Expr{skillcore.Const(skill.Cooldown), skillcore.Const(0)}},
			},
			Cast:        v3CastJSON{Unit: "tick", Value: skill.Cast},
			CDRBp:       skill.CDR,
			Priority:    skill.Priority,
			Group:       skill.Group,
			Enabled:     &enabledV3,
			Description: skill.Description,
			Tags:        append([]string(nil), skill.Tags...),
		})
	}
	out := make(map[int][]byte, 3)
	for version, document := range map[int]any{1: v1, 2: v2, 3: v3} {
		data, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal version %d: %w", version, err)
		}
		out[version] = append(data, '\n')
	}
	return out, nil
}

func (b *Bundle) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for version, data := range b.Versions {
		path := filepath.Join(dir, fmt.Sprintf("rules-v%d.json", version))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	intents, err := json.MarshalIndent(b.Intents, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "intents.json"), append(intents, '\n'), 0o644); err != nil {
		return err
	}
	truth, err := json.MarshalIndent(b.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), append(truth, '\n'), 0o644); err != nil {
		return err
	}
	manifest := struct {
		Seed       int64    `json:"seed"`
		TickMillis int64    `json:"tickMillis"`
		TotalTicks int64    `json:"totalTicks"`
		WindowSize int64    `json:"windowSize"`
		RuleHash   string   `json:"ruleHash"`
		Files      []string `json:"files"`
	}{
		Seed: b.Seed, TickMillis: b.TickMillis, TotalTicks: b.TotalTicks,
		WindowSize: b.WindowSize, RuleHash: b.RuleHash,
		Files: []string{"rules-v1.json", "rules-v2.json", "rules-v3.json", "intents.json", "truth.json"},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return nil
}

func (b *Bundle) Summary() string {
	var decisions int
	for _, tick := range b.Truth {
		decisions += len(tick.Decisions)
	}
	return fmt.Sprintf("seed=%d ticks=%d windows=%d skills=%d intents=%d decisions=%d hash=%s",
		b.Seed, b.TotalTicks, (b.TotalTicks+b.WindowSize-1)/b.WindowSize,
		len(b.Project.Skills), len(b.Intents), decisions, shortHash(b.RuleHash))
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return strings.TrimSpace(hash)
}

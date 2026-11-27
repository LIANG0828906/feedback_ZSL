// Package model 定义跨 schema 版本统一的内部规则与排程模型。
// 内部口径：时间为逻辑 tick，比例为万分比（fixed.InternalScale），全部整型。
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"skillsched/internal/expr"
)

// Version 标识规则工程来源 schema 版本。
type Version string

const (
	V1 Version = "v1"
	V2 Version = "v2"
	V3 Version = "v3"
)

// Pool 是一个共享资源池，amount 为当前/初始余量（整型资源点）。
type Pool struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Capacity   int64      `json:"capacity"`
	Regen      *expr.Flat `json:"regen"`       // 资源回复常量表（内部统一形态）
	RegenTree  *expr.Node `json:"regen_tree"`  // 等价回复表达式树
	RegenScale int64      `json:"regen_scale"` // 回复量系数刻度（内部为 InternalScale）
}

// Skill 是技能定义。所有时间字段单位为 tick，比例字段为万分比。
type Skill struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	PoolID      string     `json:"pool_id"`
	Cost        int64      `json:"cost"`         // 施放消耗（资源点，整数）
	Cooldown    int64      `json:"cooldown"`     // 冷却时长（tick）
	CDR         int64      `json:"cdr"`          // 冷却缩减系数（万分比）
	CDRCap      int64      `json:"cdr_cap"`      // 冷却缩减上界（万分比，防止超界）
	CDRTree     *expr.Node `json:"cdr_tree"`     // 冷却缩减表达式树（与 CDR 常系数等价或更复杂）
	SharedGroup string     `json:"shared_group"` // 共享冷却组 ID，空串表示无
	Priority    int        `json:"priority"`     // 同 tick 争用裁决优先级（数值小者先施放）
}

// SharedGroup 是共享冷却组：组内任一技能施放后，整组进入 GroupCooldown。
type SharedGroup struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	GroupCooldown int64    `json:"group_cooldown"` // tick
	Members       []string `json:"members"`
}

// RuleSet 是一份完整规则工程的内部模型。
type RuleSet struct {
	SourceVersion Version        `json:"source_version"`
	Params        Params         `json:"params"`
	Pools         []*Pool        `json:"pools"`
	Skills        []*Skill       `json:"skills"`
	Groups        []*SharedGroup `json:"groups"`
}

// Params 是排程参数，参数版本变化会驱动依赖图缓存键变化。
type Params struct {
	ParamVersion int   `json:"param_version"`
	TickMillis   int64 `json:"tick_millis"`
	Horizon      int   `json:"horizon"` // 总 tick 数
	WindowSize   int   `json:"window_size"`
}

// Intent 是玩家在某 tick 对某技能的施放意图。
type Intent struct {
	Tick    int    `json:"tick"`
	SkillID string `json:"skill_id"`
	Seq     int    `json:"seq"` // 同 tick 内稳定次序（生成器保证）
}

// SkillTick 是真值清单中单个技能在单个 tick 的状态。
type SkillTick struct {
	SkillID     string `json:"skill_id"`
	Available   bool   `json:"available"`
	CooldownRem int64  `json:"cooldown_rem"` // 剩余冷却（tick）
	PoolID      string `json:"pool_id"`
	PoolRemain  int64  `json:"pool_remain"` // 该 tick 结算后资源池余量
	State       string `json:"state"`       // 技能状态机当前态
	Reason      string `json:"reason,omitempty"`
}

// Arbitration 记录一次同 tick 争用裁决。
type Arbitration struct {
	Tick       int    `json:"tick"`
	SkillID    string `json:"skill_id"`
	Contention string `json:"contention"` // resource / shared_group
	Winner     string `json:"winner"`
	Suppressed string `json:"suppressed"`
	Basis      string `json:"basis"`
}

// TickTruth 是逐 tick 真值。
type TickTruth struct {
	Tick         int              `json:"tick"`
	Skills       []SkillTick      `json:"skills"`
	Pools        map[string]int64 `json:"pools"` // 结算后各池余量
	Arbitrations []Arbitration    `json:"arbitrations"`
}

// Truth 是完整逐 tick 真值清单（仅来自生成器的解析构造）。
type Truth struct {
	Scenario string      `json:"scenario"`
	Seed     int64       `json:"seed"`
	Horizon  int         `json:"horizon"`
	Ticks    []TickTruth `json:"ticks"`
}

// PoolByID 建索引。
func (r *RuleSet) PoolByID() map[string]*Pool {
	m := make(map[string]*Pool, len(r.Pools))
	for _, p := range r.Pools {
		m[p.ID] = p
	}
	return m
}

// SkillByID 建索引。
func (r *RuleSet) SkillByID() map[string]*Skill {
	m := make(map[string]*Skill, len(r.Skills))
	for _, s := range r.Skills {
		m[s.ID] = s
	}
	return m
}

// GroupByID 建索引。
func (r *RuleSet) GroupByID() map[string]*SharedGroup {
	m := make(map[string]*SharedGroup, len(r.Groups))
	for _, g := range r.Groups {
		m[g.ID] = g
	}
	return m
}

// Clone 深拷贝规则工程，保证编辑隔离。
func (r *RuleSet) Clone() *RuleSet {
	b, _ := json.Marshal(r)
	var out RuleSet
	_ = json.Unmarshal(b, &out)
	return &out
}

// ContentHash 对规范化后的工程内容计算 sha256。切片先排序以消除遍历顺序影响。
func (r *RuleSet) ContentHash() string {
	cp := r.Clone()
	sort.Slice(cp.Pools, func(i, j int) bool { return cp.Pools[i].ID < cp.Pools[j].ID })
	sort.Slice(cp.Skills, func(i, j int) bool { return cp.Skills[i].ID < cp.Skills[j].ID })
	sort.Slice(cp.Groups, func(i, j int) bool { return cp.Groups[i].ID < cp.Groups[j].ID })
	for _, g := range cp.Groups {
		sort.Strings(g.Members)
	}
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

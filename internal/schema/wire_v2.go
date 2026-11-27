package schema

import "skillsched/internal/expr"

// v2：snake_case 字段名；时间单位毫秒；冷却缩减千分比；扁平常量表；
// 含共享冷却组字段；仍无冷却缩减上界字段（升级内部模型时按默认补齐）。
type wireV2 struct {
	SchemaVersion int           `json:"schema_version"`
	Params        wireV2Params  `json:"params"`
	Pools         []wireV2Pool  `json:"pools"`
	Skills        []wireV2Skill `json:"skills"`
	SharedGroups  []wireV2Group `json:"shared_groups"`
}

type wireV2Params struct {
	ParamVersion int   `json:"param_version"`
	TickMs       int64 `json:"tick_ms"`
	HorizonMs    int   `json:"horizon_ms"`
	WindowMs     int   `json:"window_ms"`
}

type wireV2Pool struct {
	PoolID     string     `json:"pool_id"`
	PoolName   string     `json:"pool_name"`
	MaxAmount  int64      `json:"max_amount"`
	RegenTable *expr.Flat `json:"regen_table"`
}

type wireV2Skill struct {
	SkillID                string `json:"skill_id"`
	SkillName              string `json:"skill_name"`
	UsePool                string `json:"use_pool"`
	ResourceCost           int64  `json:"resource_cost"`
	CooldownMs             int64  `json:"cooldown_ms"`
	CooldownReducePermille int64  `json:"cooldown_reduce_permille"`
	SharedGroupID          string `json:"shared_group_id"`
	OrderWeight            int    `json:"order_weight"`
}

type wireV2Group struct {
	GroupID        string   `json:"group_id"`
	GroupName      string   `json:"group_name"`
	LockMs         int64    `json:"lock_ms"`
	MemberSkillIDs []string `json:"member_skill_ids"`
}

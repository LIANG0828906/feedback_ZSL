package schema

import "skillsched/internal/expr"

// v1：camelCase 字段名；时间单位毫秒；冷却缩减千分比；扁平常量表；
// 无共享冷却组字段；无冷却缩减上界字段。
type wireV1 struct {
	SchemaVersion int           `json:"schemaVersion"`
	Params        wireV1Params  `json:"params"`
	Pools         []wireV1Pool  `json:"pools"`
	Skills        []wireV1Skill `json:"skills"`
}

type wireV1Params struct {
	ParamVersion int   `json:"paramVersion"`
	TickMs       int64 `json:"tickMs"`
	HorizonMs    int   `json:"horizonMs"`
	WindowMs     int   `json:"windowMs"`
}

type wireV1Pool struct {
	PoolId     string     `json:"poolId"`
	PoolName   string     `json:"poolName"`
	MaxAmount  int64      `json:"maxAmount"`
	RegenTable *expr.Flat `json:"regenTable"`
}

type wireV1Skill struct {
	SkillId                string `json:"skillId"`
	SkillName              string `json:"skillName"`
	UsePool                string `json:"usePool"`
	ResourceCost           int64  `json:"resourceCost"`
	CooldownMs             int64  `json:"cooldownMs"`
	CooldownReducePermille int64  `json:"cooldownReducePermille"`
	OrderWeight            int    `json:"orderWeight"`
}

package schema

import "skillsched/internal/expr"

// v3：PascalCase 字段名；时间单位 tick；冷却缩减万分比；表达式树；
// 含共享冷却组与冷却缩减上界字段，是信息最完整的版本。
type wireV3 struct {
	SchemaVer     int           `json:"SchemaVer"`
	Params        wireV3Params  `json:"Params"`
	ResourcePools []wireV3Pool  `json:"ResourcePools"`
	Abilities     []wireV3Skill `json:"Abilities"`
	CooldownLocks []wireV3Group `json:"CooldownLocks"`
}

type wireV3Params struct {
	ParamRevision int   `json:"ParamRevision"`
	TickSpanMs    int64 `json:"TickSpanMs"`
	TickHorizon   int   `json:"TickHorizon"`
	WindowTicks   int   `json:"WindowTicks"`
}

type wireV3Pool struct {
	PoolID      string     `json:"PoolID"`
	DisplayName string     `json:"DisplayName"`
	UpperBound  int64      `json:"UpperBound"`
	RegenExpr   *expr.Node `json:"RegenExpr"`
}

type wireV3Skill struct {
	AbilityID          string     `json:"AbilityID"`
	DisplayName        string     `json:"DisplayName"`
	BoundPool          string     `json:"BoundPool"`
	CastCost           int64      `json:"CastCost"`
	CooldownTicks      int64      `json:"CooldownTicks"`
	ReducePermyriad    int64      `json:"ReducePermyriad"`
	ReduceCapPermyriad int64      `json:"ReduceCapPermyriad"`
	ReduceExpr         *expr.Node `json:"ReduceExpr"`
	LockGroup          string     `json:"LockGroup"`
	PriorityRank       int        `json:"PriorityRank"`
}

type wireV3Group struct {
	LockID      string   `json:"LockID"`
	DisplayName string   `json:"DisplayName"`
	LockTicks   int64    `json:"LockTicks"`
	Members     []string `json:"Members"`
}

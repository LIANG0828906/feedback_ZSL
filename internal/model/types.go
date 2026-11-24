package model

// SkillKind 区分伤害技能与治疗技能。
type SkillKind string

const (
	SkillAttack SkillKind = "attack"
	SkillHeal   SkillKind = "heal"
)

// Skill 是技能静态定义。所有数值均为定点整型（毫单位，见 fixed.go）。
type Skill struct {
	ID       string    `json:"id"`
	Kind     SkillKind `json:"kind"`
	Power    int64     `json:"power_milli"`
	Cost     int64     `json:"cost_milli"`
	Cooldown int       `json:"cooldown"`
}

// UnitSpec 是参战单位的静态规格。
type UnitSpec struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Team    int      `json:"team"`
	MaxHP   int64    `json:"max_hp_milli"`
	MaxMP   int64    `json:"max_mp_milli"`
	StartHP int64    `json:"start_hp_milli"`
	StartMP int64    `json:"start_mp_milli"`
	MPRegen int64    `json:"mp_regen_milli"`
	Skills  []string `json:"skills"`
}

// Intent 是某单位在某回合的技能释放意图。
// 两个 Inject 字段仅用于异常注入链路，正常样本恒为 false。
type Intent struct {
	UnitID          string `json:"unit_id"`
	SkillID         string `json:"skill_id"`
	TargetID        string `json:"target_id,omitempty"`
	InjectConflict  bool   `json:"inject_conflict,omitempty"`
	InjectStageFail bool   `json:"inject_stage_fail,omitempty"`
}

// TurnRecord 是回放中的单个战斗回合原始记录。
type TurnRecord struct {
	TurnNo  int      `json:"turn_no"`
	Order   []string `json:"order"`
	Intents []Intent `json:"intents"`
}

// Replay 是一整局对战回放（不含真值，真值单独存放）。
type Replay struct {
	MatchID string       `json:"match_id"`
	Seed    int64        `json:"seed"`
	Skills  []Skill      `json:"skills"`
	Units   []UnitSpec   `json:"units"`
	Turns   []TurnRecord `json:"turns"`
}

// UnitState 是逐回合结束时单位状态。
type UnitState struct {
	UnitID    string         `json:"unit_id"`
	HP        int64          `json:"hp_milli"`
	MP        int64          `json:"mp_milli"`
	Alive     bool           `json:"alive"`
	Cooldowns map[string]int `json:"cooldowns"`
}

// TurnState 是单回合结束快照。
// Winner: -1 战斗进行中, 0 平局, >0 获胜队伍编号。
type TurnState struct {
	TurnNo int         `json:"turn_no"`
	Winner int         `json:"winner"`
	Units  []UnitState `json:"units"`
}

// MatchTruth 是生成器构造的逐回合真值清单。
type MatchTruth struct {
	MatchID string      `json:"match_id"`
	Turns   []TurnState `json:"turns"`
}

// Violation 是前置校验违规类型，按裁决优先级声明。
type Violation string

const (
	ViolationInsufficientResource Violation = "insufficient_resource"
	ViolationCooldownActive       Violation = "cooldown_active"
	ViolationTargetDead           Violation = "target_dead"
	// ViolationPhantomRejection 仅用于 E_CONFLICT 注入：无违规却被判失败。
	ViolationPhantomRejection Violation = "phantom_rejection"
)

// RulingPriority 是固定裁决优先级，与遍历顺序无关。
var RulingPriority = []Violation{
	ViolationInsufficientResource,
	ViolationCooldownActive,
	ViolationTargetDead,
}

// Ruling 是一条裁决明细。
type Ruling struct {
	TurnNo     int         `json:"turn_no"`
	UnitID     string      `json:"unit_id"`
	SkillID    string      `json:"skill_id"`
	Violations []Violation `json:"violations"`
	Reason     Violation   `json:"reason"`
	Allowed    bool        `json:"allowed"`
}

// WindowStatus 是窗口状态机四态。
type WindowStatus string

const (
	WindowPending   WindowStatus = "pending"
	WindowRunning   WindowStatus = "running"
	WindowCommitted WindowStatus = "committed"
	WindowFailed    WindowStatus = "failed"
)

// WindowReport 是一个回合窗口的结算报告。
type WindowReport struct {
	MatchID      string       `json:"match_id"`
	WindowNo     int          `json:"window_no"`
	FromTurn     int          `json:"from_turn"`
	ToTurn       int          `json:"to_turn"`
	Status       WindowStatus `json:"status"`
	Snapshots    []TurnState  `json:"snapshots"`
	Rulings      []Ruling     `json:"rulings"`
	ActionOffset int          `json:"action_offset"`
	RngDraws     int          `json:"rng_draws"`
	RngSeed      int64        `json:"rng_seed"`
	ContentHash  string       `json:"content_hash"`
	FailCode     string       `json:"fail_code,omitempty"`
}

// Algorithm 标识两条自适应结算路径。
type Algorithm string

const (
	AlgoFine      Algorithm = "fine_grained"
	AlgoAggregate Algorithm = "window_pre_aggregate"
)

// Decision 记录自适应取舍结论与依据。
type Decision struct {
	MatchID       string    `json:"match_id"`
	Algorithm     Algorithm `json:"algorithm"`
	WindowSize    int       `json:"window_size"`
	Turns         int       `json:"turns"`
	Units         int       `json:"units"`
	EstMemBytes   int64     `json:"est_mem_bytes"`
	MemBudget     int64     `json:"mem_budget_bytes"`
	EstTimeMicros int64     `json:"est_time_micros"`
	TimeBudget    int64     `json:"time_budget_micros"`
	Basis         string    `json:"basis"`
}

// RunParams 是全链路共享的参数（含版本指纹）。
type RunParams struct {
	WindowSize       int   `json:"window_size"`
	TimeBudgetMicros int64 `json:"time_budget_micros"`
	MemBudgetBytes   int64 `json:"mem_budget_bytes"`
	MaxParallel      int   `json:"max_parallel"`
	WorkerBufferCap  int   `json:"worker_buffer_cap"`
}

// 版本指纹：恢复时任一不匹配则整体作废快照。
const (
	ParamVersion = "param-1.0.0"
	AlgoVersion  = "algo-1.0.0"
)

// DefaultParams 返回演示管线使用的默认预算参数。
func DefaultParams() RunParams {
	return RunParams{
		WindowSize:       50,
		TimeBudgetMicros: 3_000_000,
		MemBudgetBytes:   64 << 20,
		MaxParallel:      4,
		WorkerBufferCap:  8,
	}
}

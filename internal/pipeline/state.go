package pipeline

import "mcapture/internal/session"

// 分段状态。
const (
	StatePending   = "pending"   // 待估计
	StateAligned   = "aligned"   // 已校正
	StateCommitted = "committed" // 已提交（冻结）
	StateFailed    = "failed"    // 失败（可重试）
)

// 合法迁移表。
var transitions = map[string]map[string]bool{
	StatePending:   {StateAligned: true, StateFailed: true},
	StateAligned:   {StateCommitted: true, StateFailed: true, StatePending: true},
	StateFailed:    {StatePending: true, StateAligned: true},
	StateCommitted: {}, // 终态：任何迁移都非法，保证已提交分段不被改写
}

// Segment 是某个通道在某个时间分段上的处理状态与产物。
type Segment struct {
	ChannelID  string
	Index      int
	Start      int64
	End        int64 // 半开区间 [Start, End)
	State      string
	Drift      DriftParams
	Output     []float64
	Hash       string
	Residuals  []ResidualStats
	FailCode   string
	FailReason string
}

// Transition 尝试状态迁移；非法迁移返回 E_STATE 且保持原状态。
func (g *Segment) Transition(to string) error {
	if !transitions[g.State][to] {
		return session.NewError(session.EState,
			"illegal transition "+g.State+" -> "+to).
			With(g.ChannelID, "state").WithSegment(g.Index)
	}
	g.State = to
	return nil
}

// IsFrozen 报告分段是否已提交冻结。
func (g *Segment) IsFrozen() bool { return g.State == StateCommitted }

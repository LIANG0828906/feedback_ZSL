package engine

import "skillsched/internal/errx"

// 技能状态机五态。
const (
	SkillPending   = "pending"   // 待装载
	SkillReady     = "ready"     // 待结算
	SkillSettling  = "settling"  // 结算中
	SkillCommitted = "committed" // 已提交
	SkillFailed    = "failed"    // 失败（可重试，不污染已提交窗口）
)

// 窗口状态机四态。
const (
	WinPending    = "pending"    // 待排程
	WinScheduling = "scheduling" // 排程中
	WinCommitted  = "committed"  // 已提交
	WinFailed     = "failed"     // 失败
)

var skillTransitions = map[string]map[string]bool{
	SkillPending:   {SkillReady: true, SkillFailed: true},
	SkillReady:     {SkillSettling: true, SkillFailed: true},
	SkillSettling:  {SkillCommitted: true, SkillFailed: true},
	SkillCommitted: {},
	SkillFailed:    {SkillReady: true}, // 允许重试
}

var winTransitions = map[string]map[string]bool{
	WinPending:    {WinScheduling: true, WinFailed: true},
	WinScheduling: {WinCommitted: true, WinFailed: true},
	WinCommitted:  {},
	WinFailed:     {WinPending: true}, // 整窗重排
}

// transitionSkill 在非法迁移时返回 E_STATE 并保持原状态。
func transitionSkill(cur, next string) error {
	if !skillTransitions[cur][next] {
		return errx.New(errx.EState, "illegal skill state transition").
			With("from", cur, "to", next)
	}
	return nil
}

// transitionWindow 在非法迁移时返回 E_STATE 并保持原状态。
func transitionWindow(cur, next string) error {
	if !winTransitions[cur][next] {
		return errx.New(errx.EState, "illegal window state transition").
			With("from", cur, "to", next)
	}
	return nil
}

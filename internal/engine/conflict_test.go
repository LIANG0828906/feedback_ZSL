package engine

import (
	"testing"

	"skillsched/internal/errx"
	"skillsched/internal/gen"
	"skillsched/internal/model"
)

func TestEffectiveCooldownClamped(t *testing.T) {
	// 缩减上界生效，且结果永不为负。
	s := &model.Skill{Cooldown: 10, CDR: 9000, CDRCap: 3000}
	if got := EffectiveCooldown(s); got != 7 {
		t.Fatalf("eff=%d want 7", got)
	}
	s2 := &model.Skill{Cooldown: 1, CDR: 10000, CDRCap: 10000}
	if got := EffectiveCooldown(s2); got != 0 {
		t.Fatalf("eff=%d want 0 (no negative)", got)
	}
}

func TestResumeAndRealtimeEqualFull(t *testing.T) {
	sc := gen.Generate("paths", gen.DefaultConfig())
	full, err := RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := RunRealtime(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	if tick, sid, field, ok := Compare(full, rt); ok {
		t.Fatalf("realtime mismatch tick=%d skill=%s field=%s", tick, sid, field)
	}
	// 从初始态恢复等价全量。
	resumed, err := RunResume(sc.Rules, InitialRuntime(sc.Rules), sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	if tick, sid, field, ok := Compare(full, resumed); ok {
		t.Fatalf("resume mismatch tick=%d skill=%s field=%s", tick, sid, field)
	}
}

func TestCompareReturnsFieldName(t *testing.T) {
	a := []model.TickTruth{{Tick: 0, Skills: []model.SkillTick{{SkillID: "x", Available: true}}}}
	b := []model.TickTruth{{Tick: 0, Skills: []model.SkillTick{{SkillID: "x", Available: false}}}}
	tick, sid, field, ok := Compare(a, b)
	if !ok || tick != 0 || sid != "x" || field != "available" {
		t.Fatalf("got tick=%d sid=%s field=%s ok=%v", tick, sid, field, ok)
	}
	if errx.CodeOf(transitionWindow(WinPending, WinCommitted)) != errx.EState {
		t.Fatal("illegal window transition must be E_STATE")
	}
}

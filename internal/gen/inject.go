package gen

import (
	"encoding/json"
	"os"
	"path/filepath"

	"replaykernel/internal/model"
)

// InjectSpec 描述一个异常注入样本。
type InjectSpec struct {
	MatchID string
	Kind    string // decode | order | conflict | stage
	Turn    int    // 注入回合（1-based）
	Window  int    // 关联窗口（由 demo 按窗口大小换算并校验）
}

// BuildInjectSamples 基于一局正常回放派生四类异常样本，写入 samples/faults。
func BuildInjectSamples(base *model.Replay, outDir string, specs []InjectSpec) error {
	dir := filepath.Join(outDir, "faults")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, sp := range specs {
		clone := cloneReplay(base)
		clone.MatchID = sp.MatchID
		var raw []byte
		switch sp.Kind {
		case "decode":
			raw = []byte(`{"match_id":"` + sp.MatchID + `","turns":[BROKEN_PAYLOAD`)
		case "order":
			injectOrder(clone, sp.Turn)
			raw = mustJSON(clone)
		case "conflict":
			injectConflict(clone, sp.Turn)
			raw = mustJSON(clone)
		case "stage":
			injectStage(clone, sp.Turn)
			raw = mustJSON(clone)
		default:
			return model.NewError(model.ESchema, "unknown injection kind %q", sp.Kind)
		}
		if err := os.WriteFile(filepath.Join(dir, sp.MatchID+".json"), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func cloneReplay(r *model.Replay) *model.Replay {
	b, _ := json.Marshal(r)
	var cp model.Replay
	_ = json.Unmarshal(b, &cp)
	return &cp
}

func mustJSON(r *model.Replay) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return b
}

// injectOrder 把指定回合的编号改成 +2，制造跳号（期望 N 实测 N+2）。
func injectOrder(r *model.Replay, turn int) {
	for i := range r.Turns {
		if r.Turns[i].TurnNo == turn {
			r.Turns[i].TurnNo = turn + 2
			return
		}
	}
}

// injectConflict 在指定回合挑选一个意图，将其改写成必然合法的形态
// （零消耗零冷却技能 + 存活敌方目标）后再打 phantom 标记，
// 保证裁决时违规集合严格为空。
func injectConflict(r *model.Replay, turn int) {
	unitByID := map[string]model.UnitSpec{}
	for _, u := range r.Units {
		unitByID[u.ID] = u
	}
	// 注入专用技能：零消耗、零冷却，保证 phantom 意图不存在任何真实违规。
	const phantomSkill = "phantom_strike"
	hasSkill := false
	for _, s := range r.Skills {
		if s.ID == phantomSkill {
			hasSkill = true
		}
	}
	if !hasSkill {
		r.Skills = append(r.Skills, model.Skill{
			ID: phantomSkill, Kind: model.SkillAttack, Power: 1_000, Cost: 0, Cooldown: 0,
		})
	}
	for ui := range r.Units {
		known := false
		for _, sid := range r.Units[ui].Skills {
			if sid == phantomSkill {
				known = true
			}
		}
		if !known {
			r.Units[ui].Skills = append(r.Units[ui].Skills, phantomSkill)
		}
	}
	for i := range r.Turns {
		if r.Turns[i].TurnNo != turn || len(r.Turns[i].Intents) == 0 {
			continue
		}
		t := r.Turns[i]
		j := 0
		actor := unitByID[t.Intents[j].UnitID]
		var enemyID string
		for _, u := range r.Units {
			if u.Team != actor.Team {
				enemyID = u.ID
				break
			}
		}
		t.Intents[j].SkillID = phantomSkill
		t.Intents[j].TargetID = enemyID
		t.Intents[j].InjectConflict = true
		return
	}
}

// injectStage 在指定回合注入单窗口内部失败标记。
func injectStage(r *model.Replay, turn int) {
	for i := range r.Turns {
		if r.Turns[i].TurnNo != turn || len(r.Turns[i].Intents) == 0 {
			continue
		}
		r.Turns[i].Intents[0].InjectStageFail = true
		return
	}
}

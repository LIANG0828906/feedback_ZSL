// Package gen 是本地确定性样本生成器：按固定种子产出回放与解析真值。
// 真值由本包独立实现规则构造，是全链路唯一观测来源；不依赖 engine 结算路径。
package gen

import (
	"encoding/json"
	"os"
	"path/filepath"

	"replaykernel/internal/model"
	"replaykernel/internal/prng"
)

// Spec 描述一批样本的规模参数。
type Spec struct {
	OutDir     string
	Seed       int64
	MatchSpecs []MatchSpec
}

// MatchSpec 描述单局规模。
type MatchSpec struct {
	MatchID string
	Turns   int
	Units   int // 必须为偶数，两队均分
}

// skill 模板（毫单位定点值）。
var skillCatalog = []model.Skill{
	{ID: "strike", Kind: model.SkillAttack, Power: 12_000, Cost: 8_000, Cooldown: 2},
	{ID: "heavy", Kind: model.SkillAttack, Power: 22_000, Cost: 18_000, Cooldown: 4},
	{ID: "mend", Kind: model.SkillHeal, Power: 10_000, Cost: 6_000, Cooldown: 2},
}

// genUnit 是生成器内部的单位运行状态（独立于 engine）。
type genUnit struct {
	spec      model.UnitSpec
	hp        int64
	mp        int64
	alive     bool
	cooldowns map[string]int
}

// Generate 按规格生成全部正常样本文件，并返回回放哈希映射。
func Generate(spec Spec) (map[string]string, error) {
	if err := os.MkdirAll(filepath.Join(spec.OutDir, "replays"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(spec.OutDir, "truth"), 0o755); err != nil {
		return nil, err
	}

	hashes := map[string]string{}
	for _, ms := range spec.MatchSpecs {
		replay, truth, raw, err := generateMatch(spec.Seed, ms)
		if err != nil {
			return nil, err
		}
		hashes[ms.MatchID] = sha256Hex(raw)

		if err := writeJSON(filepath.Join(spec.OutDir, "replays", ms.MatchID+".json"), replay); err != nil {
			return nil, err
		}
		if err := writeJSON(filepath.Join(spec.OutDir, "truth", ms.MatchID+".truth.json"), truth); err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

func generateMatch(masterSeed int64, ms MatchSpec) (*model.Replay, *model.MatchTruth, []byte, error) {
	if ms.Units%2 != 0 || ms.Units < 2 {
		return nil, nil, nil, model.NewError(model.ESchema, "unit count must be even and >= 2")
	}
	matchSeed := masterSeed + int64(hashStr(ms.MatchID)%1_000_000)

	units := buildUnits(ms)
	replay := &model.Replay{
		MatchID: ms.MatchID,
		Seed:    matchSeed,
		Skills:  append([]model.Skill(nil), skillCatalog...),
		Units:   units,
	}

	runtime := map[string]*genUnit{}
	order := make([]string, 0, len(units))
	for _, u := range units {
		cd := map[string]int{}
		for _, sid := range u.Skills {
			cd[sid] = 0
		}
		runtime[u.ID] = &genUnit{
			spec: u, hp: u.StartHP, mp: u.StartMP, alive: true, cooldowns: cd,
		}
		order = append(order, u.ID)
	}

	truth := &model.MatchTruth{MatchID: ms.MatchID}
	for turnNo := 1; turnNo <= ms.Turns; turnNo++ {
		turn := buildTurn(matchSeed, turnNo, order, runtime)
		replay.Turns = append(replay.Turns, turn)

		// 生成器独立结算真值，规则与 engine 保持同一套确定语义。
		simulateTurn(replay, runtime, turn)
		truth.Turns = append(truth.Turns, snapshotTruth(turnNo, order, runtime, winnerOf(runtime)))
	}

	raw, err := json.Marshal(replay)
	if err != nil {
		return nil, nil, nil, err
	}
	return replay, truth, raw, nil
}

func buildUnits(ms MatchSpec) []model.UnitSpec {
	out := make([]model.UnitSpec, 0, ms.Units)
	perTeam := ms.Units / 2
	for i := 0; i < ms.Units; i++ {
		team := 1
		if i >= perTeam {
			team = 2
		}
		out = append(out, model.UnitSpec{
			ID:      unitName(i),
			Name:    unitName(i),
			Team:    team,
			MaxHP:   100_000,
			MaxMP:   60_000,
			StartHP: 100_000,
			StartMP: 40_000,
			MPRegen: 5_000,
			Skills:  []string{"strike", "heavy", "mend"},
		})
	}
	return out
}

func buildTurn(matchSeed int64, turnNo int, order []string, rt map[string]*genUnit) model.TurnRecord {
	// 行动顺序：用回合派生随机源做确定性轮转排列，保证仍是合法排列。
	rng := prng.New(prng.TurnSeed(matchSeed, turnNo))
	shuffled := append([]string(nil), order...)
	for i := len(shuffled) - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	turn := model.TurnRecord{TurnNo: turnNo, Order: shuffled}
	for _, uid := range shuffled {
		u := rt[uid]
		if !u.alive {
			continue
		}
		skill := pickSkill(rng, u)
		target := pickTarget(rng, uid, u.spec.Team, rt)
		turn.Intents = append(turn.Intents, model.Intent{
			UnitID:   uid,
			SkillID:  skill,
			TargetID: target,
		})
	}
	return turn
}

func pickSkill(rng *prng.Source, u *genUnit) string {
	// 偏向能立即释放的技能，保证样本里同时存在成功释放与违规裁决。
	affordable := make([]string, 0, 3)
	for _, sid := range u.spec.Skills {
		if u.cooldowns[sid] == 0 {
			affordable = append(affordable, sid)
		}
	}
	if len(affordable) > 0 && rng.Chance(750) {
		return affordable[rng.Intn(len(affordable))]
	}
	return u.spec.Skills[rng.Intn(len(u.spec.Skills))]
}

func pickTarget(rng *prng.Source, selfID string, team int, rt map[string]*genUnit) string {
	enemies := make([]string, 0)
	friends := make([]string, 0)
	for id, u := range rt {
		if !u.alive || id == selfID {
			continue
		}
		if u.spec.Team == team {
			friends = append(friends, id)
		} else {
			enemies = append(enemies, id)
		}
	}
	// map 遍历不确定，但最终选择交给 rng；为跨进程确定，先排序。
	sortStrings(enemies)
	sortStrings(friends)
	if len(enemies) == 0 {
		if len(friends) > 0 {
			return friends[rng.Intn(len(friends))]
		}
		return selfID
	}
	return enemies[rng.Intn(len(enemies))]
}

// simulateTurn 是生成器侧的独立确定性结算（与 engine.resolveTurn 同语义、不同实现）。
func simulateTurn(r *model.Replay, rt map[string]*genUnit, turn model.TurnRecord) {
	skillByID := map[string]model.Skill{}
	for _, s := range r.Skills {
		skillByID[s.ID] = s
	}
	for _, u := range rt {
		for sid := range u.cooldowns {
			if u.cooldowns[sid] > 0 {
				u.cooldowns[sid]--
			}
		}
	}

	intentByUnit := map[string]model.Intent{}
	for _, in := range turn.Intents {
		intentByUnit[in.UnitID] = in
	}
	rng := prng.New(prng.TurnSeed(r.Seed, turn.TurnNo))
	for _, uid := range turn.Order {
		intent, ok := intentByUnit[uid]
		if !ok {
			continue
		}
		actor := rt[uid]
		if !actor.alive {
			continue
		}
		skill := skillByID[intent.SkillID]
		if actor.mp < skill.Cost || actor.cooldowns[skill.ID] > 0 {
			continue
		}
		target := rt[intent.TargetID]
		if target == nil || !target.alive {
			continue
		}
		actor.mp -= skill.Cost
		if actor.mp < 0 {
			actor.mp = 0
		}
		actor.cooldowns[skill.ID] = skill.Cooldown
		switch skill.Kind {
		case model.SkillAttack:
			dmg := skill.Power
			if rng.Chance(150) {
				dmg = dmg * 1500 / 1000
			}
			target.hp -= dmg
			if target.hp <= 0 {
				target.hp = 0
				target.alive = false
			}
		case model.SkillHeal:
			target.hp += skill.Power
			if target.hp > target.spec.MaxHP {
				target.hp = target.spec.MaxHP
			}
			target.alive = true
		}
	}

	for _, u := range rt {
		if !u.alive {
			u.mp = 0
			continue
		}
		u.mp += u.spec.MPRegen
		if u.mp > u.spec.MaxMP {
			u.mp = u.spec.MaxMP
		}
	}
}

func snapshotTruth(turnNo int, order []string, rt map[string]*genUnit, winner int) model.TurnState {
	out := model.TurnState{TurnNo: turnNo, Winner: winner}
	for _, id := range order {
		u := rt[id]
		cd := map[string]int{}
		for k, v := range u.cooldowns {
			cd[k] = v
		}
		out.Units = append(out.Units, model.UnitState{
			UnitID: id, HP: u.hp, MP: u.mp, Alive: u.alive, Cooldowns: cd,
		})
	}
	return out
}

func winnerOf(rt map[string]*genUnit) int {
	teams := map[int]bool{}
	for _, u := range rt {
		if u.alive {
			teams[u.spec.Team] = true
		}
	}
	if len(teams) == 0 {
		return 0
	}
	if len(teams) == 1 {
		for t := range teams {
			return t
		}
	}
	return -1
}

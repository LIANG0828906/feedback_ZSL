// Package gen 用固定种子的确定性伪随机源产出规则工程、施放意图与逐 tick 真值。
// 不访问任何外部战斗服或账号；真值仅来自本包内独立的参考模拟（解析构造）。
package gen

import (
	"strconv"

	"skillsched/internal/expr"
	"skillsched/internal/fixed"
	"skillsched/internal/model"
)

// lcg 是确定性线性同余生成器，保证跨平台同种子结果一致（不依赖 map 遍历）。
type lcg struct {
	state uint64
}

func newLCG(seed uint64) *lcg { return &lcg{state: seed} }

func (r *lcg) next() uint64 {
	// Knuth MMIX 常数。
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

// intn 返回 [0,n)。
func (r *lcg) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// rangeInt 返回 [lo,hi]。
func (r *lcg) rangeInt(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + r.intn(hi-lo+1)
}

// Scenario 是一次确定性生成的完整场景。
type Scenario struct {
	Name    string
	Seed    int64
	Rules   *model.RuleSet
	Intents []model.Intent
	Truth   *model.Truth
}

// Config 控制场景规模。
type Config struct {
	Seed       int64
	Horizon    int
	WindowSize int
	Pools      int
	Skills     int
	Groups     int
}

// DefaultConfig 是验收用固定种子配置。
func DefaultConfig() Config {
	return Config{Seed: 20240517, Horizon: 40, WindowSize: 8, Pools: 3, Skills: 6, Groups: 2}
}

// Generate 构造一个确定性场景。
func Generate(name string, cfg Config) *Scenario {
	rng := newLCG(uint64(cfg.Seed))
	rules := &model.RuleSet{
		SourceVersion: model.V3,
		Params: model.Params{
			ParamVersion: 1, TickMillis: fixed.TickMillis,
			Horizon: cfg.Horizon, WindowSize: cfg.WindowSize,
		},
	}

	// 资源池：回复使用线性常量表 + 等价树，回复系数取 10 的倍数保证刻度往返精确。
	for i := 0; i < cfg.Pools; i++ {
		id := "pool_" + strconv.Itoa(i+1)
		capV := int64(500 + rng.rangeInt(0, 5)*100)
		regenCoef := int64(rng.rangeInt(2, 9) * 10) // 万分比系数，10 的倍数
		flat := &expr.Flat{
			Const: int64(rng.rangeInt(5, 20)),
			Terms: []expr.Term{
				{Var: "base_regen", Coef: 1},
				{Var: "pool_pressure", Coef: regenCoef},
			},
		}
		tree := expr.TreeFromFlat("regen."+id, flat)
		rules.Pools = append(rules.Pools, &model.Pool{
			ID: id, Name: "资源池" + strconv.Itoa(i+1), Capacity: capV,
			Regen: flat, RegenTree: tree, RegenScale: fixed.InternalScale,
		})
	}

	// 共享冷却组。
	for i := 0; i < cfg.Groups; i++ {
		id := "group_" + strconv.Itoa(i+1)
		rules.Groups = append(rules.Groups, &model.SharedGroup{
			ID: id, Name: "共享冷却组" + strconv.Itoa(i+1),
			GroupCooldown: int64(rng.rangeInt(2, 5)),
			Members:       []string{},
		})
	}

	// 技能：CDR 取 10 的倍数，保证千分比<->万分比往返无损。
	for i := 0; i < cfg.Skills; i++ {
		id := "skill_" + strconv.Itoa(i+1)
		pool := rules.Pools[rng.intn(cfg.Pools)].ID
		cdr := int64(rng.rangeInt(1, 4) * 10) // 万分比 10..40%
		cdrCap := int64(rng.rangeInt(5, 8) * 10)
		if cdrCap < cdr {
			cdrCap = cdr
		}
		group := ""
		if i < cfg.Groups*2 { // 前若干技能加入共享组，制造争用
			group = rules.Groups[i%cfg.Groups].ID
		}
		cdrTree := &expr.Node{ID: "cdr." + id, Op: expr.OpNum, Value: cdr}
		skill := &model.Skill{
			ID: id, Name: "技能" + strconv.Itoa(i+1), PoolID: pool,
			Cost:     int64(rng.rangeInt(20, 90)),
			Cooldown: int64(rng.rangeInt(2, 7)),
			CDR:      cdr, CDRCap: cdrCap, CDRTree: cdrTree,
			SharedGroup: group, Priority: i + 1,
		}
		rules.Skills = append(rules.Skills, skill)
		if group != "" {
			for _, g := range rules.Groups {
				if g.ID == group {
					g.Members = append(g.Members, id)
				}
			}
		}
	}

	// 意图：在若干 tick 上为同池/同组技能制造重叠争用，确定性生成且按 tick/seq 排序。
	intents := buildIntents(rng, rules, cfg.Horizon)

	// 参考模拟独立构造真值。
	truth := referenceSimulate(name, cfg.Seed, rules, intents)

	return &Scenario{Name: name, Seed: cfg.Seed, Rules: rules, Intents: intents, Truth: truth}
}

func buildIntents(rng *lcg, rules *model.RuleSet, horizon int) []model.Intent {
	var out []model.Intent
	seq := 0
	for tick := 0; tick < horizon; tick++ {
		// 约 60% 的 tick 产生施放意图，部分 tick 同时施放多个技能以制造争用。
		if rng.intn(100) >= 60 {
			continue
		}
		n := 1
		if rng.intn(100) < 45 {
			n = 2
		}
		if rng.intn(100) < 15 {
			n = 3
		}
		chosen := map[string]bool{}
		for k := 0; k < n; k++ {
			idx := rng.intn(len(rules.Skills))
			sid := rules.Skills[idx].ID
			if chosen[sid] {
				continue
			}
			chosen[sid] = true
			out = append(out, model.Intent{Tick: tick, SkillID: sid, Seq: seq})
			seq++
		}
	}
	return out
}

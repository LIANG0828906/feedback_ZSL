package depgraph

import (
	"testing"

	"skillsched/internal/gen"
	"skillsched/internal/model"
)

func TestBuildAndCacheKey(t *testing.T) {
	sc := gen.Generate("dg", gen.DefaultConfig())
	g := Build(sc.Rules)
	sid := sc.Rules.Skills[0].ID
	key1 := g.Nodes[tickNodeID(sid, 0)].CacheKey(g.paramVersion, g.keys)
	key2 := g.Nodes[tickNodeID(sid, 0)].CacheKey(g.paramVersion, g.keys)
	if key1 != key2 {
		t.Fatal("cache key not stable")
	}
	if len(g.Nodes) == 0 {
		t.Fatal("graph empty")
	}
}

func TestInvalidateOnlyAffectedRange(t *testing.T) {
	sc := gen.Generate("inv", gen.DefaultConfig())
	g := Build(sc.Rules)
	// 填充全部 tick 缓存。
	for tick := 0; tick < sc.Rules.Params.Horizon; tick++ {
		for _, s := range sc.Rules.Skills {
			g.Put(tick, s.ID, model.SkillTick{SkillID: s.ID})
		}
	}
	sid := sc.Rules.Skills[0].ID
	items := g.Invalidate(Edit{
		Kind: EditCooldown, NodeID: "cdr:" + sid,
		TickFrom: 10, TickTo: 19, Reason: "cooldown edited",
		NewContent: map[string]int64{"cooldown": 9},
	}, sc.Rules.Params.Horizon)
	if len(items) == 0 {
		t.Fatal("expected invalidations")
	}
	for _, it := range items {
		if it.TickFrom != 10 || it.TickTo != 19 {
			t.Fatalf("invalidation range wrong: %+v", it)
		}
	}
	// 区间外缓存必须保留，区间内该技能缓存必须失效。
	if _, ok := g.Get(0, sid); !ok {
		t.Fatal("unaffected tick cache evicted")
	}
	if _, ok := g.Get(15, sid); ok {
		t.Fatal("affected tick cache retained")
	}
	// 未受影响的其他技能缓存仍在。
	other := sc.Rules.Skills[1].ID
	if _, ok := g.Get(15, other); !ok {
		t.Fatal("unrelated skill cache evicted")
	}
}

func TestGroupEditPropagatesToMembers(t *testing.T) {
	sc := gen.Generate("grp", gen.DefaultConfig())
	g := Build(sc.Rules)
	var groupID string
	for _, grp := range sc.Rules.Groups {
		if len(grp.Members) > 0 {
			groupID = grp.ID
			break
		}
	}
	if groupID == "" {
		t.Skip("no group with members")
	}
	items := g.Invalidate(Edit{
		Kind: EditGroup, NodeID: "group:" + groupID,
		TickFrom: 0, TickTo: sc.Rules.Params.Horizon - 1, Reason: "group added",
	}, sc.Rules.Params.Horizon)
	if len(items) == 0 {
		t.Fatal("group edit must propagate to member tick nodes")
	}
}

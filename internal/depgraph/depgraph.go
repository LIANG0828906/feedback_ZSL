// Package depgraph 建立技能/资源节点与逐 tick 结算结果之间的依赖图，
// 支持编辑后的精确失效传播：仅失效实际受影响的 tick 区间及其下游，
// 未失效区间复用持久化缓存。缓存键由节点内容哈希、节点标识、参数版本与上游缓存键共同决定。
package depgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"skillsched/internal/model"
)

// NodeKind 区分节点类型。
type NodeKind string

const (
	KindCDR       NodeKind = "cdr"   // 冷却缩减系数
	KindRegen     NodeKind = "regen" // 资源回复表达式
	KindGroup     NodeKind = "shared_group"
	KindSkillTick NodeKind = "skill_tick" // 逐 tick 结算结果
)

// Node 是依赖图节点。
type Node struct {
	ID       string   `json:"id"`
	Kind     NodeKind `json:"kind"`
	SkillID  string   `json:"skill_id,omitempty"`
	PoolID   string   `json:"pool_id,omitempty"`
	GroupID  string   `json:"group_id,omitempty"`
	Tick     int      `json:"tick,omitempty"` // 仅 skill_tick
	Content  any      `json:"content"`
	Upstream []string `json:"upstream"`
}

// contentHash 规范化内容哈希，消除遍历顺序。
func contentHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// CacheKey 按“节点标识 + 内容哈希 + 参数版本 + 上游缓存键”组合。
func (n *Node) CacheKey(paramVersion int, upstreamKeys map[string]string) string {
	ups := append([]string{}, n.Upstream...)
	sort.Strings(ups)
	parts := make([]string, 0, len(ups)+1)
	parts = append(parts, n.ID+"|"+contentHash(n.Content)+"|pv"+itoa(paramVersion))
	for _, u := range ups {
		parts = append(parts, u+"="+upstreamKeys[u])
	}
	sum := sha256.Sum256([]byte(join(parts, ";")))
	return hex.EncodeToString(sum[:])
}

// Graph 持有节点、边与 tick 缓存。
type Graph struct {
	Nodes        map[string]*Node `json:"nodes"`
	downstream   map[string][]string
	keys         map[string]string
	paramVersion int
	// cache 保存未失效的 tick 缓存（tick -> skillID -> 缓存条目）。
	cache map[int]map[string]CacheEntry
}

// CacheEntry 是一个逐 tick 结果缓存条目。
type CacheEntry struct {
	Key     string          `json:"key"`
	Tick    int             `json:"tick"`
	SkillID string          `json:"skill_id"`
	Value   model.SkillTick `json:"value"`
}

// InvalidItem 是一条失效记录（节点标识 + tick 区间）。
type InvalidItem struct {
	NodeID   string   `json:"node_id"`
	Kind     NodeKind `json:"kind"`
	TickFrom int      `json:"tick_from"`
	TickTo   int      `json:"tick_to"`
	Reason   string   `json:"reason"`
}

// Build 依据规则工程构造依赖图，并把 CDR/回复/共享组连到逐 tick 结果节点。
func Build(rules *model.RuleSet) *Graph {
	g := &Graph{
		Nodes:        map[string]*Node{},
		downstream:   map[string][]string{},
		keys:         map[string]string{},
		paramVersion: rules.Params.ParamVersion,
		cache:        map[int]map[string]CacheEntry{},
	}
	for _, p := range rules.Pools {
		n := &Node{ID: "regen:" + p.ID, Kind: KindRegen, PoolID: p.ID,
			Content: map[string]any{"regen": p.Regen, "tree": p.RegenTree}}
		g.add(n)
	}
	for _, grp := range rules.Groups {
		n := &Node{ID: "group:" + grp.ID, Kind: KindGroup, GroupID: grp.ID, Content: grp}
		g.add(n)
	}
	for _, s := range rules.Skills {
		cdr := &Node{ID: "cdr:" + s.ID, Kind: KindCDR, SkillID: s.ID,
			Content: map[string]int64{"cdr": s.CDR, "cdr_cap": s.CDRCap, "cooldown": s.Cooldown}}
		g.add(cdr)
		var ups []string
		ups = append(ups, "cdr:"+s.ID, "regen:"+s.PoolID)
		if s.SharedGroup != "" {
			ups = append(ups, "group:"+s.SharedGroup)
		}
		for t := 0; t < rules.Params.Horizon; t++ {
			tn := &Node{
				ID: tickNodeID(s.ID, t), Kind: KindSkillTick, SkillID: s.ID, Tick: t,
				Content: map[string]any{"tick": t, "skill": s.ID}, Upstream: ups,
			}
			g.add(tn)
		}
	}
	g.recomputeKeys()
	return g
}

func tickNodeID(skillID string, tick int) string {
	return "tick:" + skillID + ":" + itoa(tick)
}

func (g *Graph) add(n *Node) {
	g.Nodes[n.ID] = n
	for _, u := range n.Upstream {
		g.downstream[u] = append(g.downstream[u], n.ID)
	}
}

func (g *Graph) recomputeKeys() {
	// 拓扑顺序：先非 tick 节点，再 tick 节点。上游键先就绪。
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return rank(g.Nodes[ids[i]]) < rank(g.Nodes[ids[j]]) ||
			(rank(g.Nodes[ids[i]]) == rank(g.Nodes[ids[j]]) && ids[i] < ids[j])
	})
	for _, id := range ids {
		g.keys[id] = g.Nodes[id].CacheKey(g.paramVersion, g.keys)
	}
}

func rank(n *Node) int {
	switch n.Kind {
	case KindSkillTick:
		return 2
	default:
		return 1
	}
}

// EditKind 标识编辑类型，决定受影响 tick 区间。
type EditKind string

const (
	EditCooldown EditKind = "edit_cooldown"
	EditGroup    EditKind = "edit_group"
	EditScale    EditKind = "edit_scale"
)

// Edit 描述一次规则编辑。TickFrom/TickTo 为受影响区间（含边界）。
type Edit struct {
	Kind       EditKind
	NodeID     string
	TickFrom   int
	TickTo     int
	NewContent any
	Reason     string
}

// Invalidate 应用编辑并返回精确失效清单；只失效受影响区间及其下游，未失效缓存保留。
func (g *Graph) Invalidate(ed Edit, horizon int) []InvalidItem {
	if ed.TickFrom < 0 {
		ed.TickFrom = 0
	}
	if ed.TickTo >= horizon || ed.TickTo == 0 {
		ed.TickTo = horizon - 1
	}
	items := []InvalidItem{}
	seen := map[string]bool{}

	// 更新被编辑节点内容并失效其键。
	if n, ok := g.Nodes[ed.NodeID]; ok {
		n.Content = ed.NewContent
	}

	// 找到所有下游 tick 节点，按节点声明的受影响区间裁剪。
	root := ed.NodeID
	var walk func(id string)
	walk = func(id string) {
		for _, d := range g.downstream[id] {
			n := g.Nodes[d]
			if n == nil || n.Kind != KindSkillTick {
				walk(d)
				continue
			}
			if n.Tick < ed.TickFrom || n.Tick > ed.TickTo {
				continue
			}
			if !seen[d] {
				seen[d] = true
				items = append(items, InvalidItem{
					NodeID: d, Kind: KindSkillTick, TickFrom: ed.TickFrom, TickTo: ed.TickTo,
					Reason: ed.Reason,
				})
				g.evict(n.Tick, n.SkillID)
			}
		}
	}
	walk(root)

	sort.Slice(items, func(i, j int) bool {
		if items[i].NodeID != items[j].NodeID {
			return items[i].NodeID < items[j].NodeID
		}
		return items[i].TickFrom < items[j].TickFrom
	})
	g.recomputeKeys()
	return items
}

func (g *Graph) evict(tick int, skillID string) {
	if m, ok := g.cache[tick]; ok {
		delete(m, skillID)
		if len(m) == 0 {
			delete(g.cache, tick)
		}
	}
}

// Put 写入一个 tick 缓存条目（仅当键匹配当前节点键）。
func (g *Graph) Put(tick int, skillID string, v model.SkillTick) {
	key := g.keys[tickNodeID(skillID, tick)]
	if g.cache[tick] == nil {
		g.cache[tick] = map[string]CacheEntry{}
	}
	g.cache[tick][skillID] = CacheEntry{Key: key, Tick: tick, SkillID: skillID, Value: v}
}

// Get 命中未被显式失效的条目即返回缓存。失效只通过 Invalidate->evict 精确发生，
// 未失效区间即使其上游节点重算键变化也继续复用持久化结果（由编辑区间裁剪保证安全）。
func (g *Graph) Get(tick int, skillID string) (CacheEntry, bool) {
	m, ok := g.cache[tick]
	if !ok {
		return CacheEntry{}, false
	}
	e, ok := m[skillID]
	if !ok {
		return CacheEntry{}, false
	}
	return e, true
}

// CachedTicks 返回仍有缓存的 tick（升序），用于验证未失效区间复用。
func (g *Graph) CachedTicks() []int {
	out := make([]int, 0, len(g.cache))
	for t := range g.cache {
		if len(g.cache[t]) > 0 {
			out = append(out, t)
		}
	}
	sort.Ints(out)
	return out
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func itoa(n int) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

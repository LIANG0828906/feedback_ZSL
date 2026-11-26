package gen

import (
	"fmt"
	"sort"
)

const giB int64 = 1 << 30

// RenderV1 renders the snake_case / flat / cores / permille / tick schema.
func (s *canonSpec) RenderV1() *V1Project {
	out := &V1Project{
		SchemaVersion: "v1",
		ProjectID:     s.projectID,
		ReferenceCPU: V1Resources{
			CPUCores: s.refCpuMilli / 1000,
			MemBytes: s.refMemGiB * giB,
		},
		TopologySemantics: v1TopoSem(s.topoSem),
	}
	for _, n := range s.nodes {
		out.Nodes = append(out.Nodes, V1Node{
			NodeID:      n.id,
			Allocatable: V1Resources{CPUCores: n.cpuCores, MemBytes: n.memGiB * giB},
			Labels:      cloneMap(n.labels),
			Topology:    cloneMap(n.topology),
			Taints:      v1Taints(n.taints),
		})
	}
	for _, p := range s.pods {
		out.Pods = append(out.Pods, V1Pod{
			PodID:        p.id,
			Request:      V1Resources{CPUCores: p.cpuMilli / 1000, MemBytes: p.memGiB * giB},
			NodeSelector: cloneMap(p.nodeSelector),
			Tolerations:  v1Tolerations(p.tolerations),
			MustRules:    v1Rules(p.mustRules),
			PreferRules:  v1Rules(p.preferRules),
			// v1 only carries whole cores; generator keeps requests integral.
			SoftTimeoutTick: p.softTimeoutMs / 100,
		})
	}
	return out
}

func v1TopoSem(m map[string]string) []V1TopoSem {
	keys := sortedKeys(m)
	out := make([]V1TopoSem, 0, len(keys))
	for _, k := range keys {
		out = append(out, V1TopoSem{Key: k, Semantic: m[k]})
	}
	return out
}

func v1Taints(ts []canonTaint) []V1Taint {
	out := make([]V1Taint, 0, len(ts))
	for _, t := range ts {
		out = append(out, V1Taint{Key: t.key, Value: t.value, Effect: t.effect})
	}
	return out
}

func v1Tolerations(ts []canonTaint) []V1Toleration {
	out := make([]V1Toleration, 0, len(ts))
	for _, t := range ts {
		out = append(out, V1Toleration{Key: t.key, Operator: t.operator, Value: t.value, Effect: t.effect})
	}
	return out
}

func v1Rules(rs []canonRule) []V1Rule {
	out := make([]V1Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, V1Rule{
			RuleID:         r.id,
			Mode:           r.mode,
			MatchLabels:    cloneMap(r.matchLabels),
			TopologyKey:    r.topologyKey,
			WeightPermille: r.weightBps / 10,
			MaxColocated:   r.maxColocated,
		})
	}
	return out
}

// RenderV2 renders the camelCase / nested topology tree / string cores / bps /
// millisecond schema. Nodes are grouped zone -> rack while preserving the
// canonical node order inside each rack.
func (s *canonSpec) RenderV2() *V2Project {
	out := &V2Project{
		SchemaVersion: "v2",
		ProjectID:     s.projectID,
		ReferenceCPU: V2Resources{
			CPU: fmt.Sprintf("%d", s.refCpuMilli/1000),
			Mem: fmt.Sprintf("%dMi", s.refMemGiB*1024),
		},
		TopologySemantics: v2TopoSem(s.topoSem),
	}
	zoneOrder, rackOrder, byRack := groupTree(s.nodes)
	globalIndex := map[string]int{}
	for i, n := range s.nodes {
		globalIndex[n.id] = i
	}
	for _, z := range zoneOrder {
		zone := V2Zone{ZoneName: z}
		for _, rack := range rackOrder[z] {
			rackDTO := V2Rack{RackName: rack}
			nodes := byRack[z+"|"+rack]
			// The nested tree groups nodes by topology, but canonical
			// traversal order is by node id; sort inside each rack so that
			// flattening reproduces the same order as the flat schemas.
			sort.Slice(nodes, func(i, j int) bool { return globalIndex[nodes[i].id] < globalIndex[nodes[j].id] })
			for _, n := range nodes {
				rackDTO.Nodes = append(rackDTO.Nodes, V2Node{
					NodeID: n.id,
					Allocatable: V2Resources{
						CPU: fmt.Sprintf("%d", n.cpuCores),
						Mem: fmt.Sprintf("%dMi", n.memGiB*1024),
					},
					Labels:   cloneMap(n.labels),
					Topology: cloneMap(n.topology),
					Taints:   v2Taints(n.taints),
				})
			}
			zone.Racks = append(zone.Racks, rackDTO)
		}
		out.ZoneTree = append(out.ZoneTree, zone)
	}
	for _, p := range s.pods {
		out.Pods = append(out.Pods, V2Pod{
			PodID: p.id,
			Request: V2Resources{
				CPU: fmt.Sprintf("%d", p.cpuMilli/1000),
				Mem: fmt.Sprintf("%dMi", p.memGiB*1024),
			},
			NodeSelector:  cloneMap(p.nodeSelector),
			Tolerations:   v2Tolerations(p.tolerations),
			MustRules:     v2Rules(p.mustRules),
			PreferRules:   v2Rules(p.preferRules),
			SoftTimeoutMs: p.softTimeoutMs,
		})
	}
	return out
}

func groupTree(nodes []canonNode) ([]string, map[string][]string, map[string][]canonNode) {
	zoneSeen := map[string]bool{}
	rackSeen := map[string]map[string]bool{}
	byRack := map[string][]canonNode{}
	var zoneOrder []string
	rackOrder := map[string][]string{}
	// Iterate in canonical node order and append into topology buckets. Each
	// bucket therefore keeps global node order even though nodes are grouped.
	for _, n := range nodes {
		z := n.topology[zoneKey]
		rack := n.topology[rackKey]
		key := z + "|" + rack
		if !zoneSeen[z] {
			zoneSeen[z] = true
			zoneOrder = append(zoneOrder, z)
			rackSeen[z] = map[string]bool{}
		}
		if !rackSeen[z][rack] {
			rackSeen[z][rack] = true
			rackOrder[z] = append(rackOrder[z], rack)
		}
		byRack[key] = append(byRack[key], n)
	}
	return zoneOrder, rackOrder, byRack
}

func v2TopoSem(m map[string]string) []V2TopoSem {
	out := make([]V2TopoSem, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, V2TopoSem{TopologyKey: k, Semantic: m[k]})
	}
	return out
}

func v2Taints(ts []canonTaint) []V2Taint {
	out := make([]V2Taint, 0, len(ts))
	for _, t := range ts {
		out = append(out, V2Taint{Key: t.key, Value: t.value, Effect: t.effect})
	}
	return out
}

func v2Tolerations(ts []canonTaint) []V2Toleration {
	out := make([]V2Toleration, 0, len(ts))
	for _, t := range ts {
		out = append(out, V2Toleration{Key: t.key, Operator: t.operator, Value: t.value, Effect: t.effect})
	}
	return out
}

func v2Rules(rs []canonRule) []V2Rule {
	out := make([]V2Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, V2Rule{
			RuleID:       r.id,
			Mode:         r.mode,
			MatchLabels:  cloneMap(r.matchLabels),
			TopologyKey:  r.topologyKey,
			WeightBps:    r.weightBps,
			MaxColocated: r.maxColocated,
		})
	}
	return out
}

// RenderV3 renders the kebab-case / flat / milli tokens + exact ratio strings
// / bps / millisecond schema with no soft-timeout field.
func (s *canonSpec) RenderV3() *V3Project {
	out := &V3Project{
		SchemaVersion: "v3",
		ProjectID:     s.projectID,
		ReferenceCPU: V3Resources{
			CPU: fmt.Sprintf("%dm", s.refCpuMilli),
			Mem: fmt.Sprintf("%dGi", s.refMemGiB),
		},
		TopologySemantics: v3TopoSem(s.topoSem),
	}
	for _, n := range s.nodes {
		out.Nodes = append(out.Nodes, V3Node{
			NodeID: n.id,
			Allocatable: V3Resources{
				CPU: fmt.Sprintf("%dm", n.cpuCores*1000),
				Mem: fmt.Sprintf("%dGi", n.memGiB),
			},
			Labels:   cloneMap(n.labels),
			Topology: cloneMap(n.topology),
			Taints:   v3Taints(n.taints),
		})
	}
	for _, p := range s.pods {
		// Exact percentage at the 8000 mCPU reference: pct = milli/80. Whole
		// cores yield a terminating decimal (1000m -> "12.5%"), always exact.
		out.Pods = append(out.Pods, V3Pod{
			PodID: p.id,
			Request: V3Resources{
				CPU: fmt.Sprintf("%s%%", exactPercent(p.cpuMilli, 8000)),
				Mem: fmt.Sprintf("%dGi", p.memGiB),
			},
			NodeSelector: cloneMap(p.nodeSelector),
			Tolerations:  v3Tolerations(p.tolerations),
			MustRules:    v3Rules(p.mustRules),
			PreferRules:  v3Rules(p.preferRules),
		})
	}
	return out
}

func v3TopoSem(m map[string]string) []V3TopoSem {
	out := make([]V3TopoSem, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, V3TopoSem{TopologyKey: k, Semantic: m[k]})
	}
	return out
}

func v3Taints(ts []canonTaint) []V3Taint {
	out := make([]V3Taint, 0, len(ts))
	for _, t := range ts {
		out = append(out, V3Taint{Key: t.key, Value: t.value, Effect: t.effect})
	}
	return out
}

func v3Tolerations(ts []canonTaint) []V3Toleration {
	out := make([]V3Toleration, 0, len(ts))
	for _, t := range ts {
		out = append(out, V3Toleration{Key: t.key, Operator: t.operator, Value: t.value, Effect: t.effect})
	}
	return out
}

func v3Rules(rs []canonRule) []V3Rule {
	out := make([]V3Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, V3Rule{
			RuleID:       r.id,
			Mode:         r.mode,
			MatchLabels:  cloneMap(r.matchLabels),
			TopologyKey:  r.topologyKey,
			WeightBps:    r.weightBps,
			MaxColocated: r.maxColocated,
		})
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// exactPercent renders 100*num/den as an exact decimal percentage string
// without floating point. Whole values have no trailing ".0".
func exactPercent(num, den int64) string {
	scaled := num * 100
	whole := scaled / den
	rem := scaled % den
	if rem == 0 {
		return fmt.Sprintf("%d", whole)
	}
	// At most three fractional digits at the generator's fixed ratios.
	var frac []byte
	for rem != 0 && len(frac) < 6 {
		rem *= 10
		frac = append(frac, byte('0'+rem/den))
		rem %= den
	}
	return fmt.Sprintf("%d.%s", whole, string(frac))
}

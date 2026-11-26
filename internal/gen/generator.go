package gen

import "fmt"

// FixedSeed is the single seed used for all deterministic samples.
const FixedSeed uint64 = 20261127

const (
	zoneKey = "zone"
	rackKey = "rack"
	hostKey = "host"

	labelApp  = "app"
	labelTier = "tier"
	labelDisk = "disk"
)

var (
	appValues  = []string{"billing", "search", "cache", "gateway", "batch"}
	tierValues = []string{"front", "mid", "data"}
	diskValues = []string{"ssd", "hdd"}
	taintKeys  = []string{"dedicated", "gpu", "edge"}
)

// Spec sizes. The standard project stays under the engine threshold while the
// large project crosses it, exercising both scoring paths deterministically.
const (
	StandardNodes = 12
	StandardPods  = 8
	LargeNodes    = 20
	LargePods     = 12
)

// Generate builds the canonical spec for a named profile with the fixed seed.
func Generate(profile string) *canonSpec {
	nodeCount, podCount := StandardNodes, StandardPods
	if profile == "large" {
		nodeCount, podCount = LargeNodes, LargePods
	}
	r := newRNG(FixedSeed)
	if profile == "large" {
		// Distinct stream while staying deterministic.
		r = newRNG(FixedSeed ^ 0x5a5a5a5a)
	}

	spec := &canonSpec{
		projectID:   "proj-" + profile,
		refCpuMilli: 8000,
		refMemGiB:   32,
		topoSem: map[string]string{
			zoneKey: "failure-domain/zone",
			rackKey: "failure-domain/rack",
			hostKey: "node/host",
		},
	}

	spec.nodes = generateNodes(r, nodeCount)
	spec.pods = generatePods(r, spec, podCount)
	return spec
}

func generateNodes(r *rng, n int) []canonNode {
	nodes := make([]canonNode, n)
	zoneCount := 3
	rackPerZone := 2
	for i := 0; i < n; i++ {
		zone := fmt.Sprintf("z%d", i%zoneCount)
		rack := fmt.Sprintf("%s-r%d", zone, (i/zoneCount)%rackPerZone)
		host := fmt.Sprintf("node-%02d", i+1)
		labels := map[string]string{
			labelApp:  appValues[r.intn(len(appValues))],
			labelTier: tierValues[r.intn(len(tierValues))],
			labelDisk: diskValues[r.intn(len(diskValues))],
		}
		// CPU in whole cores; some small nodes guarantee feasibility pruning.
		cpuCores := int64(4 + r.intn(13))    // 4..16 cores
		memGiB := int64(8 * (1 + r.intn(7))) // 8..56 GiB in 8 GiB steps
		var taints []canonTaint
		if r.chance(25) {
			effect := "NoSchedule"
			if r.chance(30) {
				effect = "NoExecute"
			}
			taints = append(taints, canonTaint{
				key:    r.pick(taintKeys),
				value:  "true",
				effect: effect,
			})
		}
		nodes[i] = canonNode{
			id:       host,
			cpuCores: cpuCores,
			memGiB:   memGiB,
			labels:   labels,
			topology: map[string]string{zoneKey: zone, rackKey: rack, hostKey: host},
			taints:   taints,
		}
	}
	return nodes
}

func generatePods(r *rng, spec *canonSpec, n int) []canonPod {
	pods := make([]canonPod, n)
	for i := 0; i < n; i++ {
		// CPU in whole cores (1..4). This stays lossless in the integer-core
		// v1/v2 schemas while the v3 percentage at the 8000 mCPU reference is
		// still exact (1 core = 12.5%), and the range prunes small nodes.
		cpuMilli := int64(1000 * (1 + r.intn(4)))
		memGiB := int64(2 * (1 + r.intn(8))) // 2..16 GiB

		selector := map[string]string{}
		if r.chance(70) {
			selector[labelApp] = r.pick(appValues)
		}
		if r.chance(45) {
			selector[labelTier] = r.pick(tierValues)
		}

		var tolerations []canonTaint
		if r.chance(35) {
			t := r.pick(taintKeys)
			op := "Equal"
			if r.chance(40) {
				op = "Exists"
			}
			tolerations = append(tolerations, canonTaint{key: t, value: "true", effect: "", operator: op})
		}

		p := canonPod{
			id:           fmt.Sprintf("pod-%02d", i+1),
			cpuMilli:     cpuMilli,
			memGiB:       memGiB,
			nodeSelector: selector,
			tolerations:  tolerations,
		}

		// Must rules (0..2).
		mustCount := r.intn(3)
		for j := 0; j < mustCount; j++ {
			p.mustRules = append(p.mustRules, canonRule{
				id:          fmt.Sprintf("%s-must-%d", p.id, j+1),
				phase:       "must",
				mode:        pickMode(r, 55),
				matchLabels: randomSelector(r),
				topologyKey: r.pick([]string{zoneKey, rackKey}),
			})
		}

		// Prefer rules (2..4), always present so scoring is non-trivial.
		preferCount := 2 + r.intn(3)
		for j := 0; j < preferCount; j++ {
			p.preferRules = append(p.preferRules, canonRule{
				id:           fmt.Sprintf("%s-pref-%d", p.id, j+1),
				phase:        "prefer",
				mode:         pickMode(r, 50),
				matchLabels:  randomSelector(r),
				topologyKey:  r.pick([]string{zoneKey, rackKey, hostKey}),
				weightBps:    int64(1000 + 500*r.intn(9)), // 1000..5000 bps, step 500
				maxColocated: int64(1 + r.intn(4)),        // 1..4 cap, 0 never used here
			})
		}

		// Optional soft timeout in ticks; absence is represented per-pod by 0.
		if r.chance(60) {
			p.softTimeoutMs = int64(100 * (2 + r.intn(19))) // 200..2000 ms
		}
		pods[i] = p
	}
	return pods
}

func pickMode(r *rng, affinityPct int) string {
	if r.chance(affinityPct) {
		return "affinity"
	}
	return "anti_affinity"
}

func randomSelector(r *rng) map[string]string {
	sel := map[string]string{}
	sel[labelApp] = r.pick(appValues)
	if r.chance(50) {
		sel[labelTier] = r.pick(tierValues)
	}
	if r.chance(30) {
		sel[labelDisk] = r.pick(diskValues)
	}
	return sel
}

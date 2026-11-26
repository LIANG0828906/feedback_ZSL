package gen

// canonicalSpec is the version-independent description of one generated
// project. The three schema renderers and the domain ground truth are both
// derived from this single source, so cross-version equality is structural.

type canonRule struct {
	id           string
	phase        string // "must" | "prefer"
	mode         string // "affinity" | "anti_affinity"
	matchLabels  map[string]string
	topologyKey  string
	weightBps    int64
	maxColocated int64
}

type canonTaint struct {
	key, value, effect, operator string
}

type canonNode struct {
	id       string
	cpuCores int64
	memGiB   int64
	labels   map[string]string
	topology map[string]string
	taints   []canonTaint
}

type canonPod struct {
	id            string
	cpuMilli      int64
	memGiB        int64
	nodeSelector  map[string]string
	tolerations   []canonTaint
	mustRules     []canonRule
	preferRules   []canonRule
	softTimeoutMs int64
}

type canonSpec struct {
	projectID string
	// reference CPU in milli-cores, used to render v3 ratio strings.
	refCpuMilli int64
	refMemGiB   int64
	topoSem     map[string]string // topology key -> semantic token
	nodes       []canonNode
	pods        []canonPod
}

// topoSemExport exposes the authoritative topology semantics to loader
// options. It is internal-package data rendered through a narrow method.
func (s *canonSpec) topoSemExport() map[string]string {
	out := make(map[string]string, len(s.topoSem))
	for k, v := range s.topoSem {
		out[k] = v
	}
	return out
}

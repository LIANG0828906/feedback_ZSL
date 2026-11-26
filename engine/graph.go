package engine

import (
	"dialoguefit/dag"
	"dialoguefit/model"
)

// DependencyGraph exposes the explicit envelope -> stress -> alignment and
// coefficient -> range -> alignment edges for one dialogue result.
func DependencyGraph(result *model.DialogueResult) *dag.Graph {
	g := dag.New()
	if result == nil {
		return g
	}
	previousRange := ""
	for _, pr := range result.PhonemeResults {
		nodes := pr.Nodes
		if nodes == nil {
			nodes = map[string]string{"phoneme_result": pr.CacheKey}
		}
		add := func(kind string, deps ...string) {
			key := nodes[kind]
			if key == "" {
				return
			}
			g.Add(dag.Node{
				Key: key, Kind: kind, DialogueID: result.DialogueID,
				PhonemeIndex: pr.Index, DependsOn: deps,
			})
		}
		envKey := nodes["envelope"]
		coefKey := nodes["stretch_coefficient"]
		rangeKey := nodes["stretch_range"]
		stressKey := nodes["stress"]
		alignedKey := nodes["aligned_stress"]

		add("envelope")
		add("stretch_coefficient")
		rangeDeps := []string{}
		if coefKey != "" {
			rangeDeps = append(rangeDeps, coefKey)
		}
		if previousRange != "" {
			rangeDeps = append(rangeDeps, previousRange)
		}
		add("stretch_range", rangeDeps...)
		if stressKey != "" {
			add("stress", envKey)
		}
		if alignedKey != "" {
			add("aligned_stress", stressKey, rangeKey)
		}
		compositeKey := nodes["phoneme_result"]
		if compositeKey == "" {
			compositeKey = pr.CacheKey
		}
		if compositeKey != "" {
			g.Add(dag.Node{
				Key: compositeKey, Kind: "phoneme_result", DialogueID: result.DialogueID,
				PhonemeIndex: pr.Index, DependsOn: pr.Upstream,
			})
		}
		if rangeKey != "" {
			previousRange = rangeKey
		}
	}
	return g
}

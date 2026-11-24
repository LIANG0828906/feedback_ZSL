package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ContentHash hashes only semantic project content, not the source schema or
// conversion metadata. The same economy expressed as v1 or v3 therefore has
// the same business-content hash while source-version compatibility remains a
// separate checkpoint check.
func (m *Model) ContentHash() (string, error) {
	if m == nil {
		return "", NewError(ESchema, "", "nil model")
	}
	normalized := m.Clone()
	normalized.Normalize()
	nodes := make([]*Node, len(normalized.Nodes))
	for i, node := range normalized.Nodes {
		clone := node.Clone()
		sort.Slice(clone.Recipes, func(a, b int) bool {
			return clone.Recipes[a].ID < clone.Recipes[b].ID
		})
		if !clone.RateExpr.HasVariable() {
			value, err := clone.RateExpr.Eval(nil)
			if err == nil {
				clone.RateExpr = ConstExpr(value)
			}
		}
		nodes[i] = clone
	}
	initialStocks := make(map[string]int64, len(normalized.Resources))
	capacities := make(map[string]int64, len(normalized.Resources))
	for _, resource := range normalized.Resources {
		initialStocks[resource.ID] = normalized.InitialStocks[resource.ID]
		capacities[resource.ID] = normalized.Capacities[resource.ID]
	}
	payload := struct {
		ID                         string           `json:"id"`
		Name                       string           `json:"name"`
		CycleMs                    int64            `json:"cycleMs"`
		WindowCycles               int              `json:"windowCycles"`
		WindowCount                int              `json:"windowCount"`
		RateScale                  int64            `json:"rateScale"`
		WorkerScale                int64            `json:"workerScale"`
		WorkersTotal               int64            `json:"workersTotal"`
		WorkersMaximum             int64            `json:"workersMaximum"`
		Resources                  []Resource       `json:"resources"`
		InitialStocks              map[string]int64 `json:"initialStocks"`
		Capacities                 map[string]int64 `json:"capacities"`
		OverflowPolicy             string           `json:"overflowPolicy"`
		UpgradeDiscountBasisPoints int64            `json:"upgradeDiscountBasisPoints"`
		Nodes                      []*Node          `json:"nodes"`
		Intents                    []Intent         `json:"intents"`
	}{
		ID:                         normalized.ID,
		Name:                       normalized.Name,
		CycleMs:                    normalized.CycleMs,
		WindowCycles:               normalized.WindowCycles,
		WindowCount:                normalized.WindowCount,
		RateScale:                  normalized.RateScale,
		WorkerScale:                normalized.WorkerScale,
		WorkersTotal:               normalized.WorkersTotal,
		WorkersMaximum:             normalized.WorkersMaximum,
		Resources:                  normalized.Resources,
		InitialStocks:              initialStocks,
		Capacities:                 capacities,
		OverflowPolicy:             normalized.OverflowPolicy,
		UpgradeDiscountBasisPoints: normalized.UpgradeDiscountBasisPoints,
		Nodes:                      nodes,
		Intents:                    normalized.Intents,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Fingerprint creates the all-or-nothing identity used by SQLite checkpoints.
func (m *Model) Fingerprint(algorithmVersion string) (Fingerprint, error) {
	if m == nil {
		return Fingerprint{}, NewError(ESchema, "", "nil model")
	}
	normalized := m.Clone()
	normalized.Normalize()
	hash, err := m.ContentHash()
	if err != nil {
		return Fingerprint{}, err
	}
	if algorithmVersion == "" {
		algorithmVersion = DefaultAlgorithmVersion
	}
	conversionVersion := normalized.ConversionVersion
	if conversionVersion == "" {
		conversionVersion = DefaultConversionVersion
	}
	return Fingerprint{
		ContentHash:       hash,
		SourceVersion:     normalized.SourceVersion,
		ConversionVersion: conversionVersion,
		AlgorithmVersion:  algorithmVersion,
	}, nil
}

package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"driftconverge/internal/model"
)

type ClusterValues struct {
	Expected           int
	Differences        int
	PolicyBlocks       int
	Blocked            int
	ConvergedResources int
	Unobservable       bool
}

type Registry struct {
	mu     sync.RWMutex
	values map[string]ClusterValues
}

func New(clusters []string) *Registry {
	values := make(map[string]ClusterValues, len(clusters))
	for _, cluster := range clusters {
		values[cluster] = ClusterValues{}
	}
	return &Registry{values: values}
}

func (r *Registry) Update(status model.ClusterStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[status.Cluster] = ClusterValues{
		Expected:           status.ExpectedCount,
		Differences:        status.DiffCount,
		PolicyBlocks:       status.PolicyBlockCount,
		Blocked:            status.BlockedCount,
		ConvergedResources: status.ConvergedResources,
		Unobservable:       !status.Observable,
	}
}

func (r *Registry) UpdateReport(cluster model.ClusterReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[cluster.Cluster] = ClusterValues{
		Expected:           cluster.ExpectedCount,
		Differences:        cluster.DiffCount,
		PolicyBlocks:       cluster.PolicyBlockCount,
		Blocked:            cluster.BlockedCount,
		ConvergedResources: cluster.ConvergedResources,
		Unobservable:       !cluster.Observable,
	}
}

func (r *Registry) Render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clusters := make([]string, 0, len(r.values))
	for cluster := range r.values {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)

	var builder strings.Builder
	definitions := []struct {
		name string
		help string
		get  func(ClusterValues) int
	}{
		{"drift_expected_resources", "Expected Deployment resources from the Git snapshot.", func(v ClusterValues) int { return v.Expected }},
		{"drift_differences", "Differences in the latest committed comparison or preserved last observable comparison.", func(v ClusterValues) int { return v.Differences }},
		{"drift_policy_blocks", "Policy violations rejected in the latest round.", func(v ClusterValues) int { return v.PolicyBlocks }},
		{"drift_blocked_items", "Differences blocked by admission timeout in the latest round.", func(v ClusterValues) int { return v.Blocked }},
		{"drift_converged_resources", "Expected resources with no remaining differences after apply.", func(v ClusterValues) int { return v.ConvergedResources }},
		{"drift_unobservable", "1 when the cluster API could not be observed, otherwise 0.", func(v ClusterValues) int {
			if v.Unobservable {
				return 1
			}
			return 0
		}},
	}
	for _, definition := range definitions {
		fmt.Fprintf(&builder, "# HELP %s %s\n", definition.name, definition.help)
		fmt.Fprintf(&builder, "# TYPE %s gauge\n", definition.name)
		for _, cluster := range clusters {
			value := definition.get(r.values[cluster])
			fmt.Fprintf(&builder, "%s{cluster=%s} %s\n", definition.name, quoteLabel(cluster), strconv.Itoa(value))
		}
	}
	return builder.String()
}

func quoteLabel(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

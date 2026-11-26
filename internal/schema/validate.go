package schema

import (
	"fmt"
	"sort"
	"strings"

	"meshpolicy/internal/model"
	"meshpolicy/internal/xerr"
)

func ValidateProject(p *model.Project) error {
	workloadIDs := map[string]model.Workload{}
	for i, w := range p.Workloads {
		if w.ID == "" {
			return xerr.New(xerr.EFieldConflict, "workload id is required", map[string]any{"path": fmt.Sprintf("$.workloads[%d].id", i)})
		}
		if _, exists := workloadIDs[w.ID]; exists {
			return xerr.New(xerr.EFieldConflict, "duplicate workload id", map[string]any{"path": fmt.Sprintf("$.workloads[%d].id", i), "workload_id": w.ID})
		}
		workloadIDs[w.ID] = w
	}

	policyIDs := map[string]bool{}
	for i, pol := range p.Policies {
		if pol.ID == "" {
			return xerr.New(xerr.EFieldConflict, "policy id is required", map[string]any{"path": fmt.Sprintf("$.policies[%d].id", i)})
		}
		if policyIDs[pol.ID] {
			return xerr.New(xerr.EFieldConflict, "duplicate policy id", map[string]any{"path": fmt.Sprintf("$.policies[%d].id", i), "policy_id": pol.ID})
		}
		policyIDs[pol.ID] = true
		if err := validateSelectorRefs(pol.Source, workloadIDs, fmt.Sprintf("$.policies[%d].source", i), pol.ID); err != nil {
			return err
		}
	}

	for i, w := range p.Workloads {
		for _, dep := range w.DependsOn {
			dw, ok := workloadIDs[dep]
			if !ok {
				return xerr.New(xerr.EWorkloadMissing, "dependency points to a workload that is not present", map[string]any{
					"path": fmt.Sprintf("$.workloads[%d].depends_on", i), "workload_id": w.ID, "missing_ref": dep,
				})
			}
			if dw.Exited {
				return xerr.New(xerr.EWorkloadExited, "dependency points to an exited workload", map[string]any{
					"path": fmt.Sprintf("$.workloads[%d].depends_on", i), "workload_id": w.ID, "exited_ref": dep,
				})
			}
		}
	}

	if cycle := dependencyCycle(p.Workloads); len(cycle) > 0 {
		return xerr.New(xerr.ESelectorCycle, "selector/dependency propagation graph contains a cycle", map[string]any{
			"path":  "$.workloads[*].depends_on",
			"cycle": strings.Join(cycle, " -> "),
		})
	}

	for _, ev := range p.Timeline {
		if ev.Attempt != nil {
			a := ev.Attempt
			if _, ok := workloadIDs[a.SourceWorkloadID]; !ok {
				return xerr.New(xerr.EWorkloadMissing, "connection attempt source workload is missing", map[string]any{
					"path": "$.timeline.attempt.source_workload_id", "attempt_id": a.ID, "missing_ref": a.SourceWorkloadID,
				})
			}
			if _, ok := workloadIDs[a.TargetWorkloadID]; !ok {
				return xerr.New(xerr.EWorkloadMissing, "connection attempt target workload is missing", map[string]any{
					"path": "$.timeline.attempt.target_workload_id", "attempt_id": a.ID, "missing_ref": a.TargetWorkloadID,
				})
			}
		}
		for _, ref := range changePolicyRefs(ev.Change) {
			if !policyIDs[ref] {
				return xerr.New(xerr.EPolicyNotFound, "timeline change references missing policy", map[string]any{
					"change_id": valueOr(ev.Change, func(c *model.PolicyChange) string { return c.ID }),
					"policy_id": ref,
				})
			}
		}
	}
	return nil
}

func validateSelectorRefs(sel model.Selector, ids map[string]model.Workload, path, policyID string) error {
	for _, ref := range sel.WorkloadRefs {
		w, ok := ids[ref]
		if !ok {
			return xerr.New(xerr.EWorkloadMissing, "policy selector references a missing workload", map[string]any{
				"path": path + ".workload_refs", "policy_id": policyID, "missing_ref": ref,
			})
		}
		if w.Exited {
			return xerr.New(xerr.EWorkloadExited, "policy selector references an exited workload", map[string]any{
				"path": path + ".workload_refs", "policy_id": policyID, "exited_ref": ref,
			})
		}
	}
	return nil
}

func dependencyCycle(workloads []model.Workload) []string {
	adj := map[string][]string{}
	for _, w := range workloads {
		adj[w.ID] = append([]string(nil), w.DependsOn...)
		sort.Strings(adj[w.ID])
	}
	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := map[string]int{}
	stack := []string{}
	var cycle []string

	var dfs func(string) bool
	dfs = func(id string) bool {
		state[id] = active
		stack = append(stack, id)
		for _, next := range adj[id] {
			if _, ok := adj[next]; !ok {
				continue
			}
			if state[next] == unvisited {
				if dfs(next) {
					return true
				}
			} else if state[next] == active {
				for i, item := range stack {
					if item == next {
						cycle = append(append([]string(nil), stack[i:]...), next)
						return true
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return false
	}

	ids := make([]string, 0, len(workloads))
	for _, w := range workloads {
		ids = append(ids, w.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if state[id] == unvisited && dfs(id) {
			return cycle
		}
	}
	return nil
}

func changePolicyRefs(c *model.PolicyChange) []string {
	if c == nil {
		return nil
	}
	refs := []string{c.PolicyID}
	if c.Before != nil {
		refs = append(refs, c.Before.ID)
	}
	if c.After != nil {
		refs = append(refs, c.After.ID)
	}
	var out []string
	for _, ref := range refs {
		if ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

func valueOr(c *model.PolicyChange, f func(*model.PolicyChange) string) string {
	if c == nil {
		return ""
	}
	return f(c)
}

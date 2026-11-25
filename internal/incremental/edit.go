package incremental

import (
	"sort"

	"grayrelease/internal/loader"
	"grayrelease/internal/model"
)

func ApplyEdit(base *model.Project, edit Edit) (*model.Project, *model.Rule, error) {
	project, err := CloneProject(base)
	if err != nil {
		return nil, nil, model.NewError("E_EDIT_CLONE", "clone project failed").With("cause", err.Error())
	}
	if edit.Service == "" {
		return nil, nil, model.NewError("E_FIELD_MISSING", "edit.service is required").With("edit_id", edit.ID)
	}
	if _, ok := service(project, edit.Service); !ok {
		return nil, nil, model.NewError(loader.CodeServiceMissing, "edit targets missing service").
			With("edit_id", edit.ID).With("missing_service", edit.Service)
	}

	var oldRule *model.Rule
	switch edit.Kind {
	case "rule_weight", "scale":
		if edit.RuleID == "" || edit.WeightBPS == nil {
			return nil, nil, model.NewError("E_FIELD_AMBIGUOUS", "rule_weight edit requires rule_id and weight_bps").With("edit_id", edit.ID)
		}
		index, rule, err := ruleIndex(project, edit.RuleID, edit.Service)
		if err != nil {
			return nil, nil, err
		}
		if *edit.WeightBPS < 0 || *edit.WeightBPS > model.CanonicalBasis {
			return nil, nil, model.NewError(loader.CodeScaleRange, "edited weight is outside [0,10000]").
				With("rule_id", edit.RuleID).With("value", *edit.WeightBPS)
		}
		oldCopy := rule
		oldRule = &oldCopy
		project.Rules[index].WeightBPS = *edit.WeightBPS

	case "rule_match":
		if edit.RuleID == "" || edit.MatchLabelKey == "" {
			return nil, nil, model.NewError("E_FIELD_AMBIGUOUS", "rule_match edit requires rule_id and match label").With("edit_id", edit.ID)
		}
		index, rule, err := ruleIndex(project, edit.RuleID, edit.Service)
		if err != nil {
			return nil, nil, err
		}
		oldCopy := rule
		oldRule = &oldCopy
		if project.Rules[index].Match.Labels == nil {
			project.Rules[index].Match.Labels = map[string]string{}
		}
		project.Rules[index].Match.Labels[edit.MatchLabelKey] = edit.MatchLabelValue

	case "mirror_ratio":
		if edit.MirrorRatioBPS == nil {
			return nil, nil, model.NewError("E_FIELD_MISSING", "mirror_ratio edit requires mirror_ratio_bps").With("edit_id", edit.ID)
		}
		if *edit.MirrorRatioBPS < 0 || *edit.MirrorRatioBPS > model.CanonicalBasis {
			return nil, nil, model.NewError(loader.CodeScaleRange, "edited mirror ratio is outside [0,10000]").With("value", *edit.MirrorRatioBPS)
		}
		index, err := mirrorIndex(project, edit.Service, edit.RuleID)
		if err != nil {
			return nil, nil, err
		}
		project.Mirrors[index].RatioBPS = *edit.MirrorRatioBPS

	case "mirror_sample":
		index, err := mirrorIndex(project, edit.Service, edit.RuleID)
		if err != nil {
			return nil, nil, err
		}
		if edit.RemoveSample {
			project.Mirrors[index].Sample = nil
		} else {
			if edit.SampleModulus == nil || *edit.SampleModulus == 0 {
				return nil, nil, model.NewError(loader.CodeScaleRange, "mirror_sample edit requires positive sample_modulus").With("edit_id", edit.ID)
			}
			project.Mirrors[index].Sample = &model.SamplePolicy{
				Present: true, Modulus: *edit.SampleModulus,
			}
		}

	case "topology_add_dependency":
		if edit.AddDependency == "" {
			return nil, nil, model.NewError("E_FIELD_MISSING", "topology edit requires add_dependency").With("edit_id", edit.ID)
		}
		if _, ok := service(project, edit.AddDependency); !ok {
			return nil, nil, model.NewError(loader.CodeServiceMissing, "topology edit points to missing service").With("missing_service", edit.AddDependency)
		}
		for i := range project.Services {
			if project.Services[i].Name != edit.Service {
				continue
			}
			exists := false
			for _, dep := range project.Services[i].Dependencies {
				if dep == edit.AddDependency {
					exists = true
				}
			}
			if !exists {
				project.Services[i].Dependencies = append(project.Services[i].Dependencies, edit.AddDependency)
			}
		}

	default:
		return nil, nil, model.NewError("E_EDIT_KIND_UNSUPPORTED", "unsupported incremental edit kind").With("kind", edit.Kind)
	}

	if err := loader.ValidateProject(project, false); err != nil {
		return nil, oldRule, err
	}
	return project, oldRule, nil
}

func service(project *model.Project, name string) (model.Service, bool) {
	for _, svc := range project.Services {
		if svc.Name == name {
			return svc, true
		}
	}
	return model.Service{}, false
}

func ruleIndex(project *model.Project, ruleID, serviceName string) (int, model.Rule, error) {
	for i, rule := range project.Rules {
		if rule.ID == ruleID {
			if rule.Service != serviceName {
				return 0, model.Rule{}, model.NewError(loader.CodeFieldAmbiguous, "rule belongs to another service").
					With("rule_id", ruleID).With("service", serviceName).With("rule_service", rule.Service)
			}
			return i, rule, nil
		}
	}
	return 0, model.Rule{}, model.NewError("E_RULE_NOT_FOUND", "edit rule not found").With("rule_id", ruleID).With("service", serviceName)
}

func mirrorIndex(project *model.Project, serviceName, ruleID string) (int, error) {
	for i, mirror := range project.Mirrors {
		if mirror.Service == serviceName && mirror.RuleID == ruleID {
			return i, nil
		}
	}
	return 0, model.NewError("E_MIRROR_POLICY_NOT_FOUND", "mirror policy not found").
		With("service", serviceName).With("rule_id", ruleID)
}

func (m *Manager) buildClosure(edit Edit, oldRule *model.Rule) Closure {
	services := dependencySubtree(m.project, edit.Service)
	ticks := make([]int, 0, m.endTick-edit.AtTick+1)
	for tick := edit.AtTick; tick <= m.endTick; tick++ {
		ticks = append(ticks, tick)
	}
	closure := Closure{
		EditID: edit.ID, Kind: edit.Kind, Services: services, StartTick: edit.AtTick,
		EndTick: m.endTick, Ticks: ticks,
		Components: []string{
			"field:" + edit.Kind + ":" + edit.Service,
			"topology:dependency-subtree",
		},
	}

	keys := keysForService(m.requests, edit.Service)
	switch edit.Kind {
	case "rule_weight", "scale":
		closure.Components = append(closure.Components,
			"stable-split:rule="+edit.RuleID,
			"cross-time:mapping-persists-until-next-edit",
			"checkpoint:route_mapping")
		if oldRule != nil && edit.WeightBPS != nil {
			newRule := *oldRule
			newRule.WeightBPS = *edit.WeightBPS
			migrations := migrationKeys(oldRule, &newRule, keys)
			closure.StableKeys = migrations
			closure.Components = append(closure.Components, "migration-set:enumerated-keys")
		}
	case "rule_match":
		closure.Components = append(closure.Components,
			"stable-split:rule-match="+edit.RuleID,
			"cross-time:label-assignment-persists",
			"checkpoint:route_mapping")
		closure.StableKeys = keys
	case "mirror_ratio", "mirror_sample":
		closure.Components = append(closure.Components,
			"mirror-selection:"+edit.Service+":"+edit.RuleID,
			"cross-time:shadow-queue-occupancy-propagates",
			"resource:buffer-and-concurrency-state",
			"checkpoint:mirror_queue_snapshot")
		closure.StableKeys = keys
	case "topology_add_dependency":
		closure.Components = append(closure.Components, "topology:new-edge-propagates-dependency-closure")
	}
	closure.Explanation = "Recompute closure services from at_tick because stable mappings and shadow queue state can conduct earlier edits into later moments; all other service-tick entries are reused."
	return closure
}

func dependencySubtree(project *model.Project, root string) []string {
	adj := map[string][]string{}
	for _, svc := range project.Services {
		adj[svc.Name] = append(adj[svc.Name], svc.Dependencies...)
	}
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adj[current] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func keysForService(requests []model.Request, service string) []string {
	seen := map[string]bool{}
	for _, req := range requests {
		if req.Service == service {
			seen[req.Key] = true
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func migrationKeys(oldRule, newRule *model.Rule, keys []string) []string {
	out := make([]string, 0)
	for _, key := range keys {
		bucket := model.StableBucket("grayrule:"+oldRule.ID, key)
		oldCanary := bucket < oldRule.WeightBPS
		newCanary := bucket < newRule.WeightBPS
		if oldCanary != newCanary {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

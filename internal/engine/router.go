package engine

import (
	"fmt"
	"sort"

	"grayrelease/internal/model"
)

func Route(project *model.Project, req model.Request) (Decision, error) {
	service, err := serviceByName(project, req.Service)
	if err != nil {
		return Decision{}, err
	}

	decision := Decision{
		Tick: req.Tick, RequestID: req.RequestID, Service: req.Service, Key: req.Key, Labels: req.Labels,
		CacheState: "fresh",
	}

	candidates := MatchingRules(project.Rules, req)
	decision.CandidateRuleIDs = make([]string, 0, len(candidates))
	for _, rule := range candidates {
		decision.CandidateRuleIDs = append(decision.CandidateRuleIDs, rule.ID)
	}

	if len(candidates) == 0 {
		decision.AdjudicationBasis = "no-label-match:default-baseline"
		decision.Side = model.SideBaseline
		decision.MainTarget = service.Baseline
		decision.Bucket = -1
		decision.MainAllowed = true
		decision.MainConclusion = "allow:baseline"
	} else {
		rule := candidates[0]
		bucket := model.StableBucket("grayrule:"+rule.ID, req.Key)
		side := model.SideBaseline
		target := service.Baseline
		if bucket < rule.WeightBPS {
			side = model.SideCanary
			target = service.Canary
		}
		decision.SelectedRuleID = rule.ID
		decision.SelectedRuleNo = rule.RuleNo
		decision.Bucket = bucket
		decision.WeightBPS = rule.WeightBPS
		decision.Side = side
		decision.MainTarget = target
		decision.MainAllowed = true
		decision.MainConclusion = "allow:" + side
		decision.AdjudicationBasis = fmt.Sprintf(
			"declared-rules=%v;selected=%s;priority=%d;order=%d;bucket=%d;threshold_bps=%d;test=bucket<threshold",
			decision.CandidateRuleIDs, rule.ID, rule.Priority, rule.Order, bucket, rule.WeightBPS,
		)
	}

	applyMirror(project, req, &decision)
	return decision, nil
}

func MatchingRules(rules []model.Rule, req model.Request) []model.Rule {
	matched := make([]model.Rule, 0)
	for _, rule := range rules {
		if rule.Service != req.Service {
			continue
		}
		if labelsMatch(rule.Match.Labels, req.Labels) {
			matched = append(matched, rule)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority > matched[j].Priority
		}
		if matched[i].Order != matched[j].Order {
			return matched[i].Order < matched[j].Order
		}
		return matched[i].ID < matched[j].ID
	})
	return matched
}

func labelsMatch(required, actual map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func applyMirror(project *model.Project, req model.Request, decision *Decision) {
	policy := findMirrorPolicy(project.Mirrors, req.Service, decision.SelectedRuleID)
	if policy == nil {
		return
	}
	decision.MirrorPolicyPresent = true
	decision.MirrorRuleID = policy.RuleID
	if rule, ok := findRuleByID(project.Rules, policy.RuleID); ok {
		decision.MirrorRuleNo = rule.RuleNo
	}

	sampleSeed := "mirror-sample:" + policy.Service + ":" + policy.RuleID
	if policy.Sample != nil && policy.Sample.Present {
		decision.MirrorSamplePresent = true
		decision.MirrorSampled = model.StableSample(sampleSeed, req.Key, policy.Sample.Modulus, policy.Sample.Remainder)
	} else {
		decision.MirrorSampled = true
	}

	bucket := model.StableBucket("mirror:"+policy.Service+":"+policy.RuleID, req.Key)
	decision.MirrorBucket = bucket
	decision.MirrorRatioBPS = policy.RatioBPS
	decision.MirrorSelected = decision.MirrorSampled && bucket < policy.RatioBPS
	if decision.MirrorSelected {
		decision.ShadowStatus = "selected"
	} else if !decision.MirrorSampled {
		decision.ShadowStatus = "unsampled"
	} else {
		decision.ShadowStatus = "ratio_miss"
	}
}

func findMirrorPolicy(policies []model.MirrorPolicy, service, ruleID string) *model.MirrorPolicy {
	var serviceDefault *model.MirrorPolicy
	for i := range policies {
		policy := &policies[i]
		if policy.Service != service {
			continue
		}
		if policy.RuleID == ruleID && ruleID != "" {
			return policy
		}
		if policy.RuleID == "" {
			serviceDefault = policy
		}
	}
	return serviceDefault
}

func findRuleByID(rules []model.Rule, id string) (model.Rule, bool) {
	for _, rule := range rules {
		if rule.ID == id {
			return rule, true
		}
	}
	return model.Rule{}, false
}

func serviceByName(project *model.Project, name string) (model.Service, error) {
	for _, service := range project.Services {
		if service.Name == name {
			return service, nil
		}
	}
	return model.Service{}, model.NewError("E_SERVICE_MISSING", "request points to missing service").With("service", name)
}

func EnumerateMigrations(oldProject, newProject *model.Project, service, ruleID string, keys []string) ([]Migration, error) {
	oldRule, ok := findRuleByID(oldProject.Rules, ruleID)
	if !ok {
		return nil, model.NewError("E_RULE_NOT_FOUND", "old rule not found").With("rule_id", ruleID)
	}
	newRule, ok := findRuleByID(newProject.Rules, ruleID)
	if !ok {
		return nil, model.NewError("E_RULE_NOT_FOUND", "new rule not found").With("rule_id", ruleID)
	}
	if oldRule.Service != service || newRule.Service != service {
		return nil, model.NewError("E_SERVICE_MISSING", "rule service does not match migration query").
			With("service", service).With("rule_id", ruleID)
	}

	migrations := make([]Migration, 0)
	for _, key := range keys {
		bucket := model.StableBucket("grayrule:"+ruleID, key)
		oldSide := model.SideBaseline
		newSide := model.SideBaseline
		if bucket < oldRule.WeightBPS {
			oldSide = model.SideCanary
		}
		if bucket < newRule.WeightBPS {
			newSide = model.SideCanary
		}
		if oldSide == newSide {
			continue
		}
		reason := "weight-window-expansion"
		if newRule.WeightBPS < oldRule.WeightBPS {
			reason = "weight-window-contraction"
		}
		migrations = append(migrations, Migration{
			Key: key, Service: service, RuleID: ruleID, OldSide: oldSide, NewSide: newSide,
			OldBucket: bucket, NewBucket: bucket, OldWeightBPS: oldRule.WeightBPS, NewWeightBPS: newRule.WeightBPS,
			AllowedReason: reason,
		})
	}
	return migrations, nil
}

func EqualDecisions(a, b Decision) bool {
	return a.Tick == b.Tick && a.RequestID == b.RequestID && a.Service == b.Service && a.Key == b.Key &&
		a.SelectedRuleID == b.SelectedRuleID && a.Side == b.Side && a.Bucket == b.Bucket &&
		a.MainAllowed == b.MainAllowed && a.MirrorSelected == b.MirrorSelected &&
		a.MirrorRuleID == b.MirrorRuleID && a.ShadowStatus == b.ShadowStatus &&
		a.ShadowOutcome == b.ShadowOutcome && a.ShadowCode == b.ShadowCode
}

var _ = sort.Strings

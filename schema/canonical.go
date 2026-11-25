package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"alertkernel/errs"
	"alertkernel/fixed"
	"alertkernel/model"
)

const (
	MaxWorkers       = 32
	MaxRuleBuffer    = 4096
	DefaultWorkers   = 4
	DefaultBuffer    = 64
	DefaultBaseEpoch = int64(1764144000000)
)

func Load(data []byte) (*model.Project, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, errs.Wrap(errs.ESchemaCombo, "$", err, "parse document: %v", err)
	}
	switch {
	case hasString(head, "schema_version", "v1"):
		var doc DocumentV1
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return fromV1(&doc)
	case hasString(head, "schemaVersion", "v2"):
		var doc DocumentV2
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return fromV2(&doc)
	case hasString(head, "schema", "alert-kernel/v3"):
		var doc DocumentV3
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return fromV3(&doc)
	default:
		version := "unknown"
		for _, key := range []string{"schema_version", "schemaVersion", "schema"} {
			if raw, ok := head[key]; ok {
				_ = json.Unmarshal(raw, &version)
				break
			}
		}
		return nil, errs.New(errs.ESchemaUnknown, "$.schema", "unrecognized schema/version combination %q", version)
	}
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errs.Wrap(errs.ESchemaCombo, "$", err, "decode versioned document: %v", err)
	}
	return nil
}

func hasString(head map[string]json.RawMessage, key, want string) bool {
	raw, ok := head[key]
	if !ok {
		return false
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return got == want
}

func Canonicalize(p *model.Project) error {
	if p == nil {
		return errs.New(errs.EValidation, "$", "nil project")
	}
	if strings.TrimSpace(p.ProjectID) == "" {
		return errs.New(errs.EValidation, "project_id", "project id is required")
	}
	if p.Clock.Steps <= 0 {
		return errs.New(errs.EValidation, "clock.steps", "logical step count must be positive")
	}
	if p.Clock.StepMS <= 0 {
		return errs.New(errs.EValidation, "clock.step_ms", "step duration must be positive")
	}
	if p.Numeric.Scale <= 0 {
		p.Numeric.Scale = 100
	}
	if p.Numeric.Bits == 0 {
		p.Numeric.Bits = 64
	}
	if _, _, err := fixed.Bounds(p.Numeric.Bits); err != nil {
		return err
	}
	if p.Numeric.OverflowPolicy == "" {
		p.Numeric.OverflowPolicy = fixed.OverflowError
	}
	if p.Numeric.OverflowPolicy != fixed.OverflowError && p.Numeric.OverflowPolicy != fixed.OverflowSaturate {
		return errs.New(errs.EValidation, "numeric.overflow_policy", "unsupported policy %q", p.Numeric.OverflowPolicy)
	}
	if err := ValidateBudget(p.Budget); err != nil {
		return err
	}
	if p.Policy.NoDataAction == "" {
		p.Policy.NoDataAction = "reset_to_inactive"
	}
	if len(p.Policy.ReasonPriority) == 0 {
		p.Policy.ReasonPriority = []string{"suppressed", "silenced"}
	}

	sort.SliceStable(p.Receivers, func(i, j int) bool { return p.Receivers[i].Name < p.Receivers[j].Name })
	receivers := p.ReceiverNames()
	for i, r := range p.Receivers {
		if r.Name == "" {
			return errs.New(errs.EValidation, fmt.Sprintf("receivers[%d].name", i), "receiver name is required")
		}
	}
	for i := range p.Routes {
		route := &p.Routes[i]
		if route.ID == "" {
			route.ID = fmt.Sprintf("route-%d", i)
		}
		if !receivers[route.Receiver] {
			return errs.New(errs.EReceiverNotFound, fmt.Sprintf("routes[%d].receiver", i), "receiver %q is not declared", route.Receiver)
		}
		for j, m := range route.Matchers {
			if err := validateMatcher(m, fmt.Sprintf("routes[%d].matchers[%d]", i, j)); err != nil {
				return err
			}
		}
	}
	sort.SliceStable(p.Routes, func(i, j int) bool {
		if p.Routes[i].Order != p.Routes[j].Order {
			return p.Routes[i].Order < p.Routes[j].Order
		}
		return p.Routes[i].ID < p.Routes[j].ID
	})

	sort.SliceStable(p.Series, func(i, j int) bool { return p.Series[i].ID < p.Series[j].ID })
	seenSeries := map[string]bool{}
	for i, s := range p.Series {
		if s.ID == "" {
			return errs.New(errs.EValidation, fmt.Sprintf("series[%d].id", i), "series id is required")
		}
		if seenSeries[s.ID] {
			return errs.New(errs.EValidation, fmt.Sprintf("series[%d].id", i), "duplicate series id %q", s.ID)
		}
		seenSeries[s.ID] = true
		if s.Metric == "" {
			return errs.New(errs.EValidation, fmt.Sprintf("series[%d].metric", i), "metric name is required")
		}
		sort.SliceStable(s.Samples, func(a, b int) bool { return s.Samples[a].Index < s.Samples[b].Index })
		for j, sample := range s.Samples {
			if err := fixed.CheckRaw(sample.Raw, p.Numeric.Bits, fmt.Sprintf("series[%d].samples[%d].raw", i, j)); err != nil {
				return err
			}
			if sample.AtMS == 0 {
				s.Samples[j].AtMS = p.Clock.TimeMS(sample.Index)
			}
		}
	}

	seenRules := map[string]bool{}
	for i, r := range p.Rules {
		if r.ID == "" {
			return errs.New(errs.EValidation, fmt.Sprintf("rules[%d].id", i), "rule id is required")
		}
		if seenRules[r.ID] {
			return errs.New(errs.EValidation, fmt.Sprintf("rules[%d].id", i), "duplicate rule id %q", r.ID)
		}
		seenRules[r.ID] = true
		if r.ForMS < 0 {
			return errs.New(errs.EDurationNegative, fmt.Sprintf("rules[%d].for_ms", i), "for duration must be >= 0, got %dms", r.ForMS)
		}
		if r.KeepFiringMS < 0 {
			return errs.New(errs.EDurationNegative, fmt.Sprintf("rules[%d].keep_firing_for_ms", i), "keep_firing_for must be >= 0, got %dms", r.KeepFiringMS)
		}
		if r.Predicate.Metric == "" {
			return errs.New(errs.EPredicateMalformed, fmt.Sprintf("rules[%d].predicate.metric", i), "predicate metric is required")
		}
		if _, err := fixed.Compare(1, r.Predicate.Op, 0); err != nil {
			return errs.Wrap(errs.EPredicateMalformed, fmt.Sprintf("rules[%d].predicate.op", i), err, "invalid predicate: %v", err)
		}
		if err := fixed.CheckRaw(r.Predicate.Threshold, p.Numeric.Bits, fmt.Sprintf("rules[%d].predicate.threshold_raw", i)); err != nil {
			return err
		}
		if r.Predicate.AddRaw != nil {
			if err := fixed.CheckRaw(*r.Predicate.AddRaw, p.Numeric.Bits, fmt.Sprintf("rules[%d].predicate.add_raw", i)); err != nil {
				return err
			}
		}
		if r.Predicate.FreshnessMS <= 0 {
			r.Predicate.FreshnessMS = p.Clock.StepMS
		}
		if r.NoDataAction == "" {
			r.NoDataAction = p.Policy.NoDataAction
		}
		if len(r.GroupBy) == 0 {
			r.GroupBy = []string{"alertname"}
		}
		if r.Receiver != "" && !receivers[r.Receiver] {
			return errs.New(errs.EReceiverNotFound, fmt.Sprintf("rules[%d].receiver", i), "receiver %q is not declared", r.Receiver)
		}
		for j, m := range r.Predicate.Matchers {
			if err := validateMatcher(m, fmt.Sprintf("rules[%d].predicate.matchers[%d]", i, j)); err != nil {
				return err
			}
		}
		for key, tmpl := range r.LabelRewrite {
			if strings.Count(tmpl, "${") != strings.Count(tmpl, "}") {
				return errs.New(errs.EPredicateMalformed, fmt.Sprintf("rules[%d].label_rewrite.%s", i, key), "unclosed template %q", tmpl)
			}
		}
	}
	sort.SliceStable(p.Rules, func(i, j int) bool {
		if p.Rules[i].Priority != p.Rules[j].Priority {
			return p.Rules[i].Priority > p.Rules[j].Priority
		}
		return p.Rules[i].ID < p.Rules[j].ID
	})
	for i, r := range p.Rules {
		r.Order = i
	}

	for i, inh := range p.Inhibitions {
		if inh.ID == "" {
			return errs.New(errs.EValidation, fmt.Sprintf("inhibitions[%d].id", i), "inhibition id is required")
		}
		for j, m := range inh.SourceMatchers {
			if err := validateMatcher(m, fmt.Sprintf("inhibitions[%d].source_matchers[%d]", i, j)); err != nil {
				return err
			}
		}
		for j, m := range inh.TargetMatchers {
			if err := validateMatcher(m, fmt.Sprintf("inhibitions[%d].target_matchers[%d]", i, j)); err != nil {
				return err
			}
		}
	}
	for i, s := range p.Silences {
		if s.ID == "" {
			return errs.New(errs.EValidation, fmt.Sprintf("silences[%d].id", i), "silence id is required")
		}
		if s.StartT < 0 || s.EndT < s.StartT || s.EndT >= p.Clock.Steps {
			return errs.New(errs.EValidation, fmt.Sprintf("silences[%d].window", i), "invalid logical window [%d,%d] for %d steps", s.StartT, s.EndT, p.Clock.Steps)
		}
		for j, m := range s.Matchers {
			if err := validateMatcher(m, fmt.Sprintf("silences[%d].matchers[%d]", i, j)); err != nil {
				return err
			}
		}
	}

	if p.RuleVersion == "" {
		version, err := VersionHash(p)
		if err != nil {
			return err
		}
		p.RuleVersion = version
	}
	return nil
}

func ValidateBudget(b model.Budget) error {
	if b.Workers < 1 || b.Workers > MaxWorkers {
		return errs.New(errs.EBudgetWorkersOutOfRange, "budget.workers", "workers must be in [1,%d], got %d", MaxWorkers, b.Workers)
	}
	if b.PerRuleBuffer < 1 || b.PerRuleBuffer > MaxRuleBuffer {
		return errs.New(errs.EBudgetBufferOutOfRange, "budget.per_rule_buffer", "per_rule_buffer must be in [1,%d], got %d", MaxRuleBuffer, b.PerRuleBuffer)
	}
	return nil
}

func validateMatcher(m model.Matcher, path string) error {
	if m.Label == "" {
		return errs.New(errs.EValidation, path+".label", "matcher label is required")
	}
	switch m.Op {
	case "=", "==", "eq", "!=", "ne":
	default:
		return errs.New(errs.EPredicateMalformed, path+".op", "unsupported matcher operator %q", m.Op)
	}
	return nil
}

func VersionHash(p *model.Project) (string, error) {
	clone := *p
	clone.RuleVersion = ""
	clone.SourceSchema = ""
	data, err := json.Marshal(&clone)
	if err != nil {
		return "", errs.Wrap(errs.EValidation, "rule_version", err, "canonical hash: %v", err)
	}
	sum := sha256.Sum256(data)
	return "rv_" + hex.EncodeToString(sum[:])[:16], nil
}

func applyTemplate(tmpl string, source model.Labels) string {
	out := tmpl
	for _, key := range source.SortedKeys() {
		out = strings.ReplaceAll(out, "${"+key+"}", source[key])
	}
	return out
}

func RewriteLabels(rule *model.Rule, source model.Labels) model.Labels {
	out := source.Clone()
	if len(rule.LabelRewrite) > 0 {
		out = model.Labels{}
		for k, tmpl := range rule.LabelRewrite {
			out[k] = applyTemplate(tmpl, source)
		}
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

var _ = boolPtr

package loader

import (
	"encoding/json"
	"fmt"
	"os"

	"grayrelease/internal/model"
)

const (
	CodeSchemaUnsupported = "E_SCHEMA_UNSUPPORTED"
	CodeFieldAmbiguous    = "E_FIELD_AMBIGUOUS"
	CodeFieldMissing      = "E_FIELD_MISSING"
	CodeScaleRange        = "E_SCALE_RANGE"
	CodeServiceMissing    = "E_SERVICE_MISSING"
	CodeRuleOverlap       = "E_RULE_OVERLAP"
	CodeRuleInvalid       = "E_RULE_INVALID"
	CodeTimeInvalid       = "E_TIME_INVALID"
)

type LoadOptions struct {
	StrictOverlap bool
}

func LoadProjectFile(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError("E_FILE_READ", "read project file failed").With("path", path).With("cause", err.Error())
	}
	return LoadProject(data, LoadOptions{})
}

func LoadProject(data []byte, opts LoadOptions) (*model.Project, error) {
	var head struct {
		APIVersion string `json:"apiVersion"`
		APISnake   string `json:"api_version"`
		Schema     string `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, model.NewError("E_JSON_PARSE", "project JSON parse failed").With("cause", err.Error())
	}
	version := firstNonEmpty(head.APIVersion, head.APISnake, head.Schema)
	var project *model.Project
	var err error
	switch version {
	case model.SchemaV1:
		project, err = loadV1(data)
	case model.SchemaV2:
		project, err = loadV2(data)
	case model.SchemaV3:
		project, err = loadV3(data)
	default:
		return nil, model.NewError(CodeSchemaUnsupported, "unsupported traffic engineering schema").With("schema", version)
	}
	if err != nil {
		return nil, err
	}
	if err := ValidateProject(project, opts.StrictOverlap); err != nil {
		return nil, err
	}
	return project, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type v1Project struct {
	APIVersion      string      `json:"api_version"`
	ProjectRevision string      `json:"project_revision"`
	TrafficName     string      `json:"traffic_name"`
	TickDurationMS  int64       `json:"tick_duration_ms"`
	Upstreams       []v1Service `json:"upstreams"`
	GrayRules       []v1Rule    `json:"gray_rules"`
	MirrorRules     []v1Mirror  `json:"mirror_rules"`
}

type v1Service struct {
	Name       string   `json:"name"`
	Main       string   `json:"main_cluster"`
	Gray       string   `json:"gray_cluster"`
	Downstream []string `json:"downstream"`
}

type v1Rule struct {
	RuleID           string            `json:"rule_id"`
	RuleNo           int               `json:"rule_no"`
	Service          string            `json:"service"`
	Priority         int               `json:"priority"`
	DeclarationOrder *int              `json:"declaration_order"`
	MatchLabels      map[string]string `json:"match_labels"`
	WeightPermille   *int              `json:"weight_permille"`
	WeightBPS        *int              `json:"weight_bps"`
}

type v1Mirror struct {
	Service        string `json:"service"`
	RuleID         string `json:"rule_id"`
	MirrorPermille *int   `json:"mirror_permille"`
	MirrorBPS      *int   `json:"mirror_bps"`
}

func loadV1(data []byte) (*model.Project, error) {
	var in v1Project
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, model.NewError("E_JSON_PARSE", "v1 project parse failed").With("cause", err.Error())
	}
	out := &model.Project{
		SchemaVersion:      model.SchemaV1,
		SourceFormat:       "flat-permille-tick",
		EngineeringVersion: in.ProjectRevision,
		Name:               in.TrafficName,
		TickIntervalMS:     defaultInt64(in.TickDurationMS, 100),
	}
	for _, svc := range in.Upstreams {
		out.Services = append(out.Services, model.Service{
			Name: svc.Name, Baseline: svc.Main, Canary: svc.Gray, Dependencies: svc.Downstream,
		})
	}
	for i, rule := range in.GrayRules {
		weight, err := permilleToBPS(rule.WeightPermille, rule.WeightBPS, fmt.Sprintf("gray_rules[%d].weight", i))
		if err != nil {
			return nil, err.With("rule_id", rule.RuleID)
		}
		out.Rules = append(out.Rules, model.Rule{
			ID: rule.RuleID, RuleNo: defaultInt(rule.RuleNo, i+1), Service: rule.Service,
			Priority: rule.Priority, Order: intPtrOr(rule.DeclarationOrder, i),
			Match: model.Match{Labels: rule.MatchLabels}, WeightBPS: weight, DeclaredIn: "gray_rules",
		})
	}
	for i, mirror := range in.MirrorRules {
		ratio, err := permilleToBPS(mirror.MirrorPermille, mirror.MirrorBPS, fmt.Sprintf("mirror_rules[%d].mirror", i))
		if err != nil {
			return nil, err.With("service", mirror.Service).With("rule_id", mirror.RuleID)
		}
		out.Mirrors = append(out.Mirrors, model.MirrorPolicy{
			Service: mirror.Service, RuleID: mirror.RuleID, RatioBPS: ratio,
		})
	}
	return out, nil
}

type v2Project struct {
	APIVersion         string           `json:"apiVersion"`
	EngineeringVersion string           `json:"engineeringVersion"`
	Name               string           `json:"name"`
	TickIntervalMs     int64            `json:"tickIntervalMs"`
	Services           []v2Service      `json:"services"`
	RuleTree           []v2RuleGroup    `json:"ruleTree"`
	MirrorPolicies     []v2MirrorPolicy `json:"mirrorPolicies"`
}

type v2Service struct {
	Name         string   `json:"name"`
	Baseline     string   `json:"baselineCluster"`
	Canary       string   `json:"canaryCluster"`
	Dependencies []string `json:"dependencies"`
}

type v2RuleGroup struct {
	Service string   `json:"service"`
	Rules   []v2Rule `json:"rules"`
}

type v2Rule struct {
	ID               string  `json:"id"`
	RuleNo           int     `json:"ruleNo"`
	Priority         int     `json:"priority"`
	DeclarationOrder *int    `json:"declarationOrder"`
	Match            v2Match `json:"match"`
	WeightBps        *int    `json:"weightBps"`
	WeightPermille   *int    `json:"weightPermille"`
}

type v2Match struct {
	Labels map[string]string `json:"labels"`
}

type v2MirrorPolicy struct {
	Service         string  `json:"service"`
	RuleID          string  `json:"ruleId"`
	CopyRatioBps    *int    `json:"copyRatioBps"`
	RatioPermille   *int    `json:"ratioPermille"`
	SampleModulus   *uint64 `json:"sampleModulus"`
	SampleRemainder uint64  `json:"sampleRemainder"`
}

func loadV2(data []byte) (*model.Project, error) {
	var in v2Project
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, model.NewError("E_JSON_PARSE", "v2 project parse failed").With("cause", err.Error())
	}
	out := &model.Project{
		SchemaVersion: model.SchemaV2, SourceFormat: "grouped-bps-ms",
		EngineeringVersion: in.EngineeringVersion, Name: in.Name,
		TickIntervalMS: defaultInt64(in.TickIntervalMs, 100),
	}
	for _, svc := range in.Services {
		out.Services = append(out.Services, model.Service{
			Name: svc.Name, Baseline: svc.Baseline, Canary: svc.Canary, Dependencies: svc.Dependencies,
		})
	}
	for _, group := range in.RuleTree {
		for i, rule := range group.Rules {
			weight, err := bpsValue(rule.WeightBps, rule.WeightPermille, fmt.Sprintf("ruleTree[%s][%s].weight", group.Service, rule.ID))
			if err != nil {
				return nil, err.With("rule_id", rule.ID).With("service", group.Service)
			}
			out.Rules = append(out.Rules, model.Rule{
				ID: rule.ID, RuleNo: defaultInt(rule.RuleNo, i+1), Service: group.Service,
				Priority: rule.Priority, Order: intPtrOr(rule.DeclarationOrder, i),
				Match: model.Match{Labels: rule.Match.Labels}, WeightBPS: weight, DeclaredIn: "ruleTree",
			})
		}
	}
	for i, mirror := range in.MirrorPolicies {
		ratio, err := bpsValue(mirror.CopyRatioBps, mirror.RatioPermille, fmt.Sprintf("mirrorPolicies[%d].copyRatio", i))
		if err != nil {
			return nil, err.With("service", mirror.Service).With("rule_id", mirror.RuleID)
		}
		policy := model.MirrorPolicy{Service: mirror.Service, RuleID: mirror.RuleID, RatioBPS: ratio}
		if mirror.SampleModulus != nil {
			if *mirror.SampleModulus == 0 {
				return nil, model.NewError(CodeScaleRange, "sample modulus must be positive").With("service", mirror.Service).With("modulus", *mirror.SampleModulus)
			}
			policy.Sample = &model.SamplePolicy{Present: true, Modulus: *mirror.SampleModulus, Remainder: mirror.SampleRemainder}
		}
		out.Mirrors = append(out.Mirrors, policy)
	}
	return out, nil
}

type v3Project struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
	} `json:"metadata"`
	Spec struct {
		Release struct {
			Revision string `json:"revision"`
		} `json:"release"`
		Timing struct {
			WindowMS int64 `json:"windowMs"`
		} `json:"timing"`
		Topology struct {
			Nodes []v3Node `json:"nodes"`
		} `json:"topology"`
		RuleGroups []v3RuleGroup `json:"ruleGroups"`
		Mirroring  struct {
			Policies []v3Mirror `json:"policies"`
		} `json:"mirroring"`
	} `json:"spec"`
}

type v3Node struct {
	Name     string   `json:"name"`
	Baseline string   `json:"baselineCluster"`
	Canary   string   `json:"canaryCluster"`
	Calls    []string `json:"calls"`
}

type v3RuleGroup struct {
	ServiceName string   `json:"serviceName"`
	Rules       []v3Rule `json:"rules"`
}

type v3Rule struct {
	RuleID     string `json:"ruleID"`
	RuleNumber int    `json:"ruleNumber"`
	Precedence int    `json:"precedence"`
	Sequence   *int   `json:"sequence"`
	When       struct {
		Tags map[string]string `json:"tags"`
	} `json:"when"`
	Weight struct {
		BasisPoints *int `json:"basisPoints"`
		Permille    *int `json:"permille"`
	} `json:"weight"`
}

type v3Mirror struct {
	TargetService string `json:"targetService"`
	RuleName      string `json:"ruleName"`
	Copy          struct {
		BasisPoints *int `json:"basisPoints"`
		Permille    *int `json:"permille"`
	} `json:"copy"`
	Sampling *struct {
		Modulus   uint64 `json:"modulus"`
		Remainder uint64 `json:"remainder"`
	} `json:"sampling"`
}

func loadV3(data []byte) (*model.Project, error) {
	var in v3Project
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, model.NewError("E_JSON_PARSE", "v3 project parse failed").With("cause", err.Error())
	}
	revision := firstNonEmpty(in.Spec.Release.Revision, in.Metadata.Revision)
	out := &model.Project{
		SchemaVersion: model.SchemaV3, SourceFormat: "tree-bps-ms-nested",
		EngineeringVersion: revision, Name: firstNonEmpty(in.Metadata.Name, ""),
		TickIntervalMS: defaultInt64(in.Spec.Timing.WindowMS, 100),
	}
	for _, node := range in.Spec.Topology.Nodes {
		out.Services = append(out.Services, model.Service{
			Name: node.Name, Baseline: node.Baseline, Canary: node.Canary, Dependencies: node.Calls,
		})
	}
	for _, group := range in.Spec.RuleGroups {
		for i, rule := range group.Rules {
			weight, err := bpsValue(rule.Weight.BasisPoints, rule.Weight.Permille, fmt.Sprintf("ruleGroups[%s][%s].weight", group.ServiceName, rule.RuleID))
			if err != nil {
				return nil, err.With("rule_id", rule.RuleID).With("service", group.ServiceName)
			}
			out.Rules = append(out.Rules, model.Rule{
				ID: rule.RuleID, RuleNo: defaultInt(rule.RuleNumber, i+1), Service: group.ServiceName,
				Priority: rule.Precedence, Order: intPtrOr(rule.Sequence, i),
				Match: model.Match{Labels: rule.When.Tags}, WeightBPS: weight, DeclaredIn: "ruleGroups",
			})
		}
	}
	for i, mirror := range in.Spec.Mirroring.Policies {
		ratio, err := bpsValue(mirror.Copy.BasisPoints, mirror.Copy.Permille, fmt.Sprintf("mirroring.policies[%d].copy", i))
		if err != nil {
			return nil, err.With("service", mirror.TargetService).With("rule_id", mirror.RuleName)
		}
		policy := model.MirrorPolicy{Service: mirror.TargetService, RuleID: mirror.RuleName, RatioBPS: ratio}
		if mirror.Sampling != nil {
			if mirror.Sampling.Modulus == 0 {
				return nil, model.NewError(CodeScaleRange, "sampling modulus must be positive").With("service", mirror.TargetService).With("modulus", 0)
			}
			policy.Sample = &model.SamplePolicy{Present: true, Modulus: mirror.Sampling.Modulus, Remainder: mirror.Sampling.Remainder}
		}
		out.Mirrors = append(out.Mirrors, policy)
	}
	return out, nil
}

func permilleToBPS(permille, redundantBPS *int, field string) (int, *model.BizError) {
	if permille == nil {
		if redundantBPS != nil {
			return validateBPS(*redundantBPS, field)
		}
		return 0, model.NewError(CodeFieldMissing, "scale field is missing").With("field", field)
	}
	if *permille < 0 || *permille > 1000 {
		return 0, model.NewError(CodeScaleRange, "permille scale is outside [0,1000]").With("field", field).With("value", *permille)
	}
	normalized := *permille * 10
	if redundantBPS != nil && *redundantBPS != normalized {
		return 0, model.NewError(CodeFieldAmbiguous, "permille and basis-point scales disagree").
			With("field", field).With("weight_permille", *permille).With("weight_bps", *redundantBPS)
	}
	return normalized, nil
}

func bpsValue(bps, permille *int, field string) (int, *model.BizError) {
	if bps != nil {
		value, err := validateBPS(*bps, field)
		if err != nil {
			return 0, err
		}
		if permille != nil && *permille*10 != value {
			return 0, model.NewError(CodeFieldAmbiguous, "basis-point and permille scales disagree").
				With("field", field).With("weight_bps", *bps).With("weight_permille", *permille)
		}
		return value, nil
	}
	if permille != nil {
		if *permille < 0 || *permille > 1000 {
			return 0, model.NewError(CodeScaleRange, "permille scale is outside [0,1000]").With("field", field).With("value", *permille)
		}
		return *permille * 10, nil
	}
	return 0, model.NewError(CodeFieldMissing, "scale field is missing").With("field", field)
}

func validateBPS(value int, field string) (int, *model.BizError) {
	if value < 0 || value > model.CanonicalBasis {
		return 0, model.NewError(CodeScaleRange, "basis-point scale is outside [0,10000]").With("field", field).With("value", value)
	}
	return value, nil
}

func ValidateProject(p *model.Project, strictOverlap bool) error {
	if p == nil {
		return model.NewError(CodeFieldMissing, "project is nil")
	}
	if p.EngineeringVersion == "" {
		return model.NewError(CodeFieldMissing, "engineering version/revision is required").With("schema", p.SchemaVersion)
	}
	if p.TickIntervalMS <= 0 {
		return model.NewError(CodeTimeInvalid, "tick interval must be positive").With("tick_interval_ms", p.TickIntervalMS)
	}
	services := map[string]model.Service{}
	for _, svc := range p.Services {
		if svc.Name == "" {
			return model.NewError(CodeFieldMissing, "service name is required")
		}
		if _, exists := services[svc.Name]; exists {
			return model.NewError("E_SERVICE_DUPLICATE", "duplicate service").With("service", svc.Name)
		}
		services[svc.Name] = svc
	}
	for _, svc := range p.Services {
		for _, dep := range svc.Dependencies {
			if _, ok := services[dep]; !ok {
				return model.NewError(CodeServiceMissing, "topology dependency points to missing service").
					With("service", svc.Name).With("missing_service", dep)
			}
		}
	}

	seenRuleIDs := map[string]string{}
	seenRuleNo := map[string]string{}
	for i := range p.Rules {
		rule := &p.Rules[i]
		if len(rule.Match.Labels) == 0 {
			rule.Match.Labels = nil
		}
		if rule.ID == "" {
			return model.NewError(CodeRuleInvalid, "rule id is required").With("service", rule.Service).With("rule_no", rule.RuleNo)
		}
		if rule.Service == "" {
			return model.NewError(CodeServiceMissing, "rule has no service").With("rule_id", rule.ID)
		}
		if _, ok := services[rule.Service]; !ok {
			return model.NewError(CodeServiceMissing, "rule points to missing service").
				With("rule_id", rule.ID).With("missing_service", rule.Service)
		}
		if previous, ok := seenRuleIDs[rule.ID]; ok {
			return model.NewError(CodeRuleOverlap, "duplicate rule id creates indistinguishable overlap").
				With("rule_id", rule.ID).With("services", []string{previous, rule.Service})
		}
		seenRuleIDs[rule.ID] = rule.Service
		key := fmt.Sprintf("%s#%d", rule.Service, rule.RuleNo)
		if previousID, ok := seenRuleNo[key]; ok {
			return model.NewError(CodeRuleOverlap, "duplicate rule number creates indistinguishable declaration").
				With("rule_no", rule.RuleNo).With("service", rule.Service).With("rule_ids", []string{previousID, rule.ID})
		}
		seenRuleNo[key] = rule.ID
		if rule.WeightBPS < 0 || rule.WeightBPS > model.CanonicalBasis {
			return model.NewError(CodeScaleRange, "normalized rule weight is outside [0,10000]").With("rule_id", rule.ID).With("value", rule.WeightBPS)
		}
	}
	for _, mirror := range p.Mirrors {
		if _, ok := services[mirror.Service]; !ok {
			return model.NewError(CodeServiceMissing, "mirror policy points to missing service").With("missing_service", mirror.Service)
		}
		if mirror.RuleID != "" {
			rule, ok := findRule(p, mirror.RuleID)
			if !ok {
				return model.NewError(CodeServiceMissing, "mirror policy points to missing rule").
					With("service", mirror.Service).With("rule_id", mirror.RuleID)
			}
			if rule.Service != mirror.Service {
				return model.NewError(CodeFieldAmbiguous, "mirror rule belongs to another service").
					With("service", mirror.Service).With("rule_id", mirror.RuleID).With("rule_service", rule.Service)
			}
		}
		if mirror.RatioBPS < 0 || mirror.RatioBPS > model.CanonicalBasis {
			return model.NewError(CodeScaleRange, "normalized mirror ratio is outside [0,10000]").With("service", mirror.Service).With("value", mirror.RatioBPS)
		}
	}
	if err := rejectIndistinguishableRuleTies(p); err != nil {
		return err
	}
	if strictOverlap {
		if err := rejectOverlappingRules(p); err != nil {
			return err
		}
	}
	return nil
}

func findRule(p *model.Project, id string) (model.Rule, bool) {
	for _, rule := range p.Rules {
		if rule.ID == id {
			return rule, true
		}
	}
	return model.Rule{}, false
}

func rejectIndistinguishableRuleTies(p *model.Project) error {
	for i := 0; i < len(p.Rules); i++ {
		for j := i + 1; j < len(p.Rules); j++ {
			a, b := p.Rules[i], p.Rules[j]
			if a.Service != b.Service || a.Priority != b.Priority || a.Order != b.Order {
				continue
			}
			if !labelsCanOverlap(a.Match.Labels, b.Match.Labels) {
				continue
			}
			return model.NewError(CodeRuleOverlap, "rules have identical priority and declaration order and overlap").
				With("service", a.Service).With("rule_ids", []string{a.ID, b.ID}).
				With("priority", a.Priority).With("order", a.Order)
		}
	}
	return nil
}

func rejectOverlappingRules(p *model.Project) error {
	for i := 0; i < len(p.Rules); i++ {
		for j := i + 1; j < len(p.Rules); j++ {
			a, b := p.Rules[i], p.Rules[j]
			if a.Service != b.Service || !labelsCanOverlap(a.Match.Labels, b.Match.Labels) {
				continue
			}
			return model.NewError(CodeRuleOverlap, "strict mode rejects simultaneously matching rules").
				With("service", a.Service).With("rule_ids", []string{a.ID, b.ID}).
				With("priorities", []int{a.Priority, b.Priority}).With("orders", []int{a.Order, b.Order})
		}
	}
	return nil
}

func labelsCanOverlap(a, b map[string]string) bool {
	for key, value := range a {
		if other, ok := b[key]; ok && other != value {
			return false
		}
	}
	return true
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func defaultInt64(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}

func intPtrOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

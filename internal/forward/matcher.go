package forward

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

type PathStep struct {
	RuleID       string   `json:"rule_id"`
	Priority     int      `json:"priority"`
	DeclOrder    int      `json:"decl_order"`
	Evaluated    bool     `json:"evaluated"`
	Matched      bool     `json:"matched"`
	Selected     bool     `json:"selected"`
	PrefixLength int      `json:"prefix_length"`
	Reasons      []string `json:"reasons,omitempty"`
}

type OuterRoute struct {
	TargetID     string `json:"target_id"`
	Destination  string `json:"destination"`
	NextHop      string `json:"next_hop"`
	Egress       string `json:"egress"`
	PrefixLength int    `json:"prefix_length"`
	Source       string `json:"source"`
}

type Verdict struct {
	PacketID       string     `json:"packet_id"`
	Matched        bool       `json:"matched"`
	WinnerRuleID   string     `json:"winner_rule_id,omitempty"`
	Basis          string     `json:"basis"`
	FallbackSource string     `json:"fallback_source,omitempty"`
	Action         Action     `json:"action"`
	Path           []PathStep `json:"path"`
	MatchingIDs    []string   `json:"matching_rule_ids"`
	Dependencies   []string   `json:"dependencies"`
	OuterRoute     OuterRoute `json:"outer_route"`
}

func SortedRules(rules []Rule) []Rule {
	out := append([]Rule(nil), rules...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].DeclOrder < out[j].DeclOrder
	})
	return out
}

func EvaluateVerdict(p *Project, pkt PacketSpec) Verdict {
	verdict := Verdict{PacketID: pkt.ID, Path: []PathStep{}, Dependencies: []string{"rules-index"}}
	ip := net.ParseIP(pkt.DstIP)
	rules := SortedRules(p.Rules)

	groups := map[int][]Rule{}
	priorities := []int{}
	for _, rule := range rules {
		if _, ok := groups[rule.Priority]; !ok {
			priorities = append(priorities, rule.Priority)
		}
		groups[rule.Priority] = append(groups[rule.Priority], rule)
	}
	sort.Ints(priorities)

	winnerFound := false
	for _, priority := range priorities {
		group := groups[priority]
		matches := []PathStep{}
		for _, rule := range group {
			step := matchRule(rule, pkt, ip)
			step.Evaluated = !winnerFound
			verdict.Path = append(verdict.Path, step)
			if step.Matched {
				verdict.MatchingIDs = append(verdict.MatchingIDs, rule.ID)
				verdict.Dependencies = append(verdict.Dependencies, "rule:"+rule.ID)
				matches = append(matches, step)
			}
		}
		if len(matches) > 0 {
			sort.SliceStable(matches, func(i, j int) bool {
				if matches[i].PrefixLength != matches[j].PrefixLength {
					return matches[i].PrefixLength > matches[j].PrefixLength
				}
				return matches[i].DeclOrder < matches[j].DeclOrder
			})
			selected := matches[0]
			for i := range verdict.Path {
				if verdict.Path[i].RuleID == selected.RuleID {
					verdict.Path[i].Selected = true
				}
			}
			rule, _, _ := p.Rule(selected.RuleID)
			verdict.Matched = true
			verdict.WinnerRuleID = selected.RuleID
			verdict.Basis = fmt.Sprintf("rule:%s:prefix=%d:order=%d", selected.RuleID, selected.PrefixLength, selected.DeclOrder)
			verdict.Action = rule.Action
			winnerFound = true
			break
		}
		// Every rule in an exhausted group was actually evaluated and supports the negative decision.
		for _, rule := range group {
			verdict.Dependencies = append(verdict.Dependencies, "rule:"+rule.ID)
		}
	}

	if !winnerFound {
		verdict.Action = p.Fallback
		verdict.Matched = false
		verdict.Basis = "fallback:declared_action"
		verdict.FallbackSource = "project.fallback"
		verdict.Dependencies = append(verdict.Dependencies, "fallback:declared_action")
	}
	verdict.OuterRoute = LookupOuterRoute(p, pkt.DstIP)
	if verdict.OuterRoute.TargetID != "" {
		verdict.Dependencies = append(verdict.Dependencies, "route:"+verdict.OuterRoute.TargetID)
	}
	return verdict
}

func matchRule(rule Rule, pkt PacketSpec, ip net.IP) PathStep {
	step := PathStep{
		RuleID:       rule.ID,
		Priority:     rule.Priority,
		DeclOrder:    rule.DeclOrder,
		PrefixLength: -1,
	}
	networkOK := len(rule.CIDRs) == 0
	for _, raw := range rule.CIDRs {
		network, err := ParseCIDR(raw)
		if err != nil || ip == nil {
			continue
		}
		if network.Contains(ip) {
			ones, _ := network.Mask.Size()
			if ones > step.PrefixLength {
				step.PrefixLength = ones
			}
			networkOK = true
			step.Reasons = append(step.Reasons, "cidr:"+network.String())
		}
	}
	if len(rule.CIDRs) > 0 && !networkOK {
		step.Reasons = append(step.Reasons, "cidr_miss")
	}

	domainOK := len(rule.DomainSuffixes) == 0
	if len(rule.DomainSuffixes) > 0 {
		domain := strings.ToLower(strings.TrimSuffix(pkt.Domain, "."))
		for _, suffixRaw := range rule.DomainSuffixes {
			suffix := strings.ToLower(strings.TrimPrefix(suffixRaw, "."))
			if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
				domainOK = true
				step.Reasons = append(step.Reasons, "suffix:"+suffix)
				break
			}
		}
		if !domainOK {
			step.Reasons = append(step.Reasons, "suffix_miss")
		}
	}

	portOK := len(rule.PortRanges) == 0
	if len(rule.PortRanges) > 0 {
		for _, pr := range rule.PortRanges {
			if pkt.DstPort >= pr.Start && pkt.DstPort <= pr.End {
				portOK = true
				step.Reasons = append(step.Reasons, fmt.Sprintf("port:%d-in-%d-%d", pkt.DstPort, pr.Start, pr.End))
				break
			}
		}
		if !portOK {
			step.Reasons = append(step.Reasons, "port_miss")
		}
	}

	step.Matched = networkOK && domainOK && portOK
	if step.Matched && len(rule.CIDRs) == 0 {
		step.PrefixLength = -1
	}
	return step
}

func LookupOuterRoute(p *Project, dstIP string) OuterRoute {
	ip := net.ParseIP(dstIP)
	if ip == nil {
		// Tunnel remote addresses are still routable even if a test packet has only a domain.
		ip = net.IPv4(0, 0, 0, 0)
	}
	var best *RouteTarget
	bestOnes := -1
	for i := range p.RouteTargets {
		rt := &p.RouteTargets[i]
		network, err := ParseCIDR(rt.Destination)
		if err != nil || !network.Contains(ip) {
			continue
		}
		ones, _ := network.Mask.Size()
		if ones > bestOnes || (ones == bestOnes && best != nil && rt.DeclOrder < best.DeclOrder) {
			best = rt
			bestOnes = ones
		}
	}
	if best == nil {
		return OuterRoute{Source: "route_table:empty"}
	}
	return OuterRoute{
		TargetID:     best.ID,
		Destination:  best.Destination,
		NextHop:      best.NextHop,
		Egress:       best.Egress,
		PrefixLength: bestOnes,
		Source:       "longest_prefix",
	}
}

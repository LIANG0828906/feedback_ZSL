package forward

import (
	"fmt"
	"net"
	"strings"
)

type ValidationOptions struct {
	RejectEqualPriorityOverlap bool `json:"reject_equal_priority_overlap"`
}

func DefaultValidationOptions() ValidationOptions {
	return ValidationOptions{}
}

func ValidateProject(p *Project) error {
	return ValidateProjectWith(p, DefaultValidationOptions())
}

func ValidateProjectWith(p *Project, opts ValidationOptions) error {
	if p == nil {
		return NewBizError(CodeFieldRequired, "project", "project is missing")
	}
	if strings.TrimSpace(p.ID) == "" {
		return NewBizError(CodeFieldRequired, "project.id", "project id is required")
	}
	if p.IntegerBits == 0 {
		p.IntegerBits = 32
	}
	if p.IntegerBits != 16 && p.IntegerBits != 32 && p.IntegerBits != 64 {
		return NewBizError(CodeFragmentWidth, "project.integer_bits", "integer width must be 16, 32 or 64 bits").
			WithContext("actual", p.IntegerBits)
	}
	if p.MaxFragments <= 0 {
		return NewBizError(CodeFieldOutOfRange, "project.max_fragments", "max_fragments must be positive").
			WithContext("actual", p.MaxFragments)
	}

	peerIDs := map[string]bool{}
	for i := range p.Peers {
		peer := &p.Peers[i]
		path := fmt.Sprintf("peers[%d].id", i)
		if peer.ID == "" {
			return NewBizError(CodeFieldRequired, path, "peer id is required")
		}
		if peerIDs[peer.ID] {
			return NewBizError(CodeFieldConflict, path, "duplicate peer id %q", peer.ID)
		}
		peerIDs[peer.ID] = true
	}

	tunnelIDs := map[string]bool{}
	for i := range p.Tunnels {
		t := &p.Tunnels[i]
		base := fmt.Sprintf("tunnels[%d]", i)
		if t.ID == "" {
			return NewBizError(CodeFieldRequired, base+".id", "tunnel id is required")
		}
		if tunnelIDs[t.ID] {
			return NewBizError(CodeFieldConflict, base+".id", "duplicate tunnel id %q", t.ID)
		}
		tunnelIDs[t.ID] = true
		if !peerIDs[t.PeerID] {
			return NewBizError(CodePeerNotFound, base+".peer_id", "tunnel %s references missing peer %q", t.ID, t.PeerID).
				WithContext("tunnel_id", t.ID).WithContext("peer_id", t.PeerID)
		}
		if net.ParseIP(t.Local.IP) == nil {
			return NewBizError(CodeFieldType, base+".local.ip", "local address %q is not a valid IP literal", t.Local.IP)
		}
		if net.ParseIP(t.Remote.IP) == nil {
			return NewBizError(CodeFieldType, base+".remote.ip", "remote address %q is not a valid IP literal", t.Remote.IP)
		}
		if t.Local.Port < 1 || t.Local.Port > 65535 {
			return NewBizError(CodeFieldOutOfRange, base+".local.port", "port must be 1..65535").WithContext("actual", t.Local.Port)
		}
		if t.Remote.Port < 1 || t.Remote.Port > 65535 {
			return NewBizError(CodeFieldOutOfRange, base+".remote.port", "port must be 1..65535").WithContext("actual", t.Remote.Port)
		}
		overhead := ProtocolOverhead(t.OuterProtocol)
		if overhead < 0 {
			return NewBizError(CodeFieldConflict, base+".outer_protocol", "unsupported outer protocol %q", t.OuterProtocol)
		}
		if t.MTUBytes < overhead {
			return NewBizError(CodeMTUTooSmall, base+".mtu_bytes",
				"mtu %d is smaller than outer protocol overhead %d", t.MTUBytes, overhead).
				WithContext("tunnel_id", t.ID).
				WithContext("mtu_bytes", t.MTUBytes).
				WithContext("outer_overhead_bytes", overhead).
				WithContext("outer_protocol", t.OuterProtocol)
		}
		if t.PathMTUBytes != 0 && t.PathMTUBytes < overhead+MinInnerOverhead {
			return NewBizError(CodePathMTUTooSmall, base+".path_mtu_bytes",
				"path mtu %d cannot carry outer overhead %d and inner header %d", t.PathMTUBytes, overhead, MinInnerOverhead)
		}
		if t.KeepaliveMS < 0 {
			return NewBizError(CodeKeepaliveNegative, base+".keepalive_ms", "keepalive must not be negative").
				WithContext("tunnel_id", t.ID).WithContext("keepalive_ms", t.KeepaliveMS)
		}
		if t.KeepaliveWatermarkBytes != nil && *t.KeepaliveWatermarkBytes < 0 {
			return NewBizError(CodeKeepaliveNegative, base+".keepalive_watermark_bytes", "watermark must not be negative").
				WithContext("tunnel_id", t.ID).WithContext("watermark_bytes", *t.KeepaliveWatermarkBytes)
		}
		if _, ok := CipherOverhead(t.CipherSuite); !ok {
			return NewBizError(CodeFieldConflict, base+".cipher_suite", "unsupported cipher suite %q", t.CipherSuite)
		}
	}

	for i := range p.RouteTargets {
		r := &p.RouteTargets[i]
		if r.Destination != "" {
			if _, err := ParseCIDR(r.Destination); err != nil {
				return NewBizError(CodeFieldType, fmt.Sprintf("route_targets[%d].destination", i), "invalid CIDR: %v", err)
			}
		}
	}

	ruleIDs := map[string]bool{}
	for i := range p.Rules {
		r := &p.Rules[i]
		base := fmt.Sprintf("rules[%d]", i)
		if r.ID == "" {
			return NewBizError(CodeFieldRequired, base+".id", "rule id is required")
		}
		if ruleIDs[r.ID] {
			return NewBizError(CodeFieldConflict, base+".id", "duplicate rule id %q", r.ID)
		}
		ruleIDs[r.ID] = true
		for j, cidr := range r.CIDRs {
			if _, err := ParseCIDR(cidr); err != nil {
				return NewBizError(CodeFieldType, fmt.Sprintf("%s.cidrs[%d]", base, j), "invalid CIDR %q: %v", cidr, err)
			}
		}
		for j, pr := range r.PortRanges {
			if pr.Start < 0 || pr.Start > 65535 || pr.End < 0 || pr.End > 65535 || pr.Start > pr.End {
				return NewBizError(CodeFieldOutOfRange, fmt.Sprintf("%s.port_ranges[%d]", base, j), "invalid port range %+v", pr)
			}
		}
		if err := validateAction(r.Action, p, base+".action"); err != nil {
			return err
		}
	}
	if err := validateAction(p.Fallback, p, "fallback"); err != nil {
		return err
	}
	if opts.RejectEqualPriorityOverlap {
		if err := rejectOverlap(p); err != nil {
			return err
		}
	}
	return nil
}

func validateAction(a Action, p *Project, path string) error {
	switch a.Type {
	case ActionDirect, ActionDrop, ActionReject:
		return nil
	case ActionTunnel:
		if a.TunnelID == "" {
			return NewBizError(CodeFieldRequired, path+".tunnel_id", "tunnel action requires tunnel_id")
		}
		if _, _, ok := p.Tunnel(a.TunnelID); !ok {
			return NewBizError(CodeTunnelNotFound, path+".tunnel_id", "action references missing tunnel %q", a.TunnelID).
				WithContext("tunnel_id", a.TunnelID)
		}
		return nil
	default:
		return NewBizError(CodeActionInvalid, path+".type", "unsupported action type %q", a.Type)
	}
}

func rejectOverlap(p *Project) error {
	type parsed struct {
		rule Rule
		nets []*net.IPNet
	}
	items := make([]parsed, 0, len(p.Rules))
	for _, r := range p.Rules {
		item := parsed{rule: r}
		for _, c := range r.CIDRs {
			if n, err := ParseCIDR(c); err == nil {
				item.nets = append(item.nets, n)
			}
		}
		items = append(items, item)
	}
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			if a.rule.Priority != b.rule.Priority || len(a.nets) == 0 || len(b.nets) == 0 {
				continue
			}
			for _, na := range a.nets {
				for _, nb := range b.nets {
					if networksOverlap(na, nb) && !actionsEquivalent(a.rule.Action, b.rule.Action) {
						path := fmt.Sprintf("rules[%s].cidrs", a.rule.ID)
						return NewBizError(CodeRuleOverlapConflict, path,
							"rules %s and %s overlap at equal priority %d with conflicting actions",
							a.rule.ID, b.rule.ID, a.rule.Priority).
							WithContext("rule_ids", []string{a.rule.ID, b.rule.ID}).
							WithContext("priority", a.rule.Priority).
							WithContext("networks", []string{na.String(), nb.String()})
					}
				}
			}
		}
	}
	return nil
}

func actionsEquivalent(a, b Action) bool {
	return a.Type == b.Type && a.TunnelID == b.TunnelID
}

func networksOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// DottedMaskToPrefix converts 255.255.255.0 style masks to an integer prefix.
func DottedMaskToPrefix(mask string) (int, error) {
	ip := net.ParseIP(mask).To4()
	if ip == nil {
		return 0, NewBizError(CodeFieldType, "", "mask %q is not an IPv4 dotted mask", mask)
	}
	ones, bits := net.IPv4Mask(ip[0], ip[1], ip[2], ip[3]).Size()
	if bits != 32 {
		return 0, NewBizError(CodeFieldType, "", "mask %q is not an IPv4 mask", mask)
	}
	if ones == 0 && mask != "0.0.0.0" {
		// Size already reports non-contiguous masks as 0,16 style? Go returns 0,32 maybe.
		return 0, NewBizError(CodeFieldConflict, "", "mask %q is not a contiguous prefix mask", mask)
	}
	return ones, nil
}

func PrefixToDottedMask(prefix int) (string, error) {
	if prefix < 0 || prefix > 32 {
		return "", NewBizError(CodeFieldOutOfRange, "", "prefix %d outside IPv4 range", prefix)
	}
	mask := net.CIDRMask(prefix, 32)
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3]), nil
}

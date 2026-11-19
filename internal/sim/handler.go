package sim

import (
	"fmt"
	"hash/fnv"
	"net"
	"strings"
	"time"

	"dnssim/internal/dnswire"
)

type handlerReply struct {
	msg          dnswire.Message
	cacheStatus  string
	anomaly      string
	nextResolver string
	nextAddress  string
	drop         bool
	fromCache    bool
}

func (e *Engine) answer(rt *resolverRuntime, req dnswire.Message, peer net.Addr, transport string, lease *slotLease) (handlerReply, bool) {
	q := req.Questions[0]
	meta := decodeTraceMeta(req)
	reply := handlerReply{
		msg: dnswire.Message{
			Response:           true,
			RecursionDesired:   req.RecursionDesired,
			RecursionAvailable: true,
			RCode:              dnswire.RCodeNoError,
			Questions:          req.Questions,
			Answers:            []dnswire.RR{},
			Authorities:        []dnswire.RR{},
			Additionals:        []dnswire.RR{},
		},
		cacheStatus: "none",
	}

	if !meta.NoFaults {
		if waitContext(e.ctx, time.Duration(rt.cfg.LatencyMS)*time.Millisecond) == false {
			return reply, true
		}
		if shouldDrop(rt, q, meta, e.cfg.Seed) {
			return reply, true
		}
	}

	fault := e.matchingFault(rt, q, meta, transport)
	if fault != nil && !meta.NoFaults {
		if waitContext(e.ctx, time.Duration(fault.DelayMS)*time.Millisecond) == false {
			return reply, true
		}
		switch fault.Type {
		case "timeout":
			return reply, true
		case "servfail":
			reply.msg.RCode = dnswire.RCodeServFail
			reply.anomaly = "servfail"
			return reply, false
		case "wrong_delegation":
			if rt.cfg.Role != RoleRoot && rt.cfg.Role != RoleTLD {
				reply.msg.RCode = dnswire.RCodeServFail
				reply.anomaly = "wrong_delegation"
				return reply, false
			}
			return e.answerDelegation(rt, q, fault, true), false
		}
	}

	switch rt.cfg.Role {
	case RoleRoot, RoleTLD:
		return e.answerDelegation(rt, q, nil, false), false
	case RoleAuthority:
		return e.answerAuthority(rt, q), false
	case RoleRecursive, RoleForwarder:
		return e.answerRecursive(rt, q, meta, lease), false
	default:
		reply.msg.RCode = dnswire.RCodeServFail
		reply.anomaly = "invalid_role"
		return reply, false
	}
}

func shouldDrop(rt *resolverRuntime, q dnswire.Question, meta traceMeta, seed int64) bool {
	if rt.cfg.DropRate <= 0 {
		return false
	}
	attempt := meta.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s|%s|%s|%d", seed, rt.cfg.ID, q.Name, q.Type.String(), attempt)
	value := float64(h.Sum64()%100000) / 100000
	return value < rt.cfg.DropRate
}

func (e *Engine) matchingFault(rt *resolverRuntime, q dnswire.Question, meta traceMeta, transport string) *Fault {
	attempt := meta.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	for i := range rt.faults {
		f := &rt.faults[i]
		if !faultDomainMatches(f.Domain, q.Name) {
			continue
		}
		if f.Transport != "" && f.Transport != transport {
			continue
		}
		if len(f.Attempts) > 0 && !containsInt(f.Attempts, attempt) {
			continue
		}
		return f
	}
	return nil
}

func faultDomainMatches(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(name, suffix) && name != strings.TrimPrefix(suffix, ".")
	}
	return pattern == name
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (e *Engine) answerDelegation(rt *resolverRuntime, q dnswire.Question, fault *Fault, injected bool) handlerReply {
	reply := handlerReply{
		msg: dnswire.Message{
			Response:           true,
			RecursionDesired:   true,
			RecursionAvailable: true,
			Questions:          []dnswire.Question{q},
			Answers:            []dnswire.RR{},
			Authorities:        []dnswire.RR{},
			Additionals:        []dnswire.RR{},
		},
		cacheStatus: "none",
	}
	var delegation *Delegation
	var longest int
	for i := range rt.cfg.Delegations {
		d := &rt.cfg.Delegations[i]
		if dnswire.IsSubdomain(q.Name, d.Zone) && len(d.Zone) > longest {
			longest = len(d.Zone)
			delegation = d
		}
	}
	if delegation == nil {
		reply.msg.RCode = dnswire.RCodeNXDomain
		reply.msg.Authorities = []dnswire.RR{negativeSOA(".")}
		return reply
	}

	target := e.resolvers[delegation.Target]
	if target == nil {
		reply.msg.RCode = dnswire.RCodeServFail
		reply.anomaly = "missing_delegation_target"
		return reply
	}
	nextAddr := target.addr
	nextID := target.cfg.ID
	if injected && fault != nil {
		reply.anomaly = "wrong_delegation"
		if fault.WrongTarget != "" {
			if wrong := e.resolvers[fault.WrongTarget]; wrong != nil {
				nextAddr = wrong.addr
				nextID = wrong.cfg.ID
			}
		} else {
			nextAddr = fault.WrongAddress
			nextID = ""
		}
	}
	host, _, err := net.SplitHostPort(nextAddr)
	if err != nil {
		reply.msg.RCode = dnswire.RCodeServFail
		reply.anomaly = "invalid_delegation_address"
		return reply
	}
	ttl := delegation.TTL
	reply.msg.Authorities = []dnswire.RR{{
		Name: delegation.Zone, Type: dnswire.TypeNS, Class: dnswire.ClassIN, TTL: ttl,
		Target: "ns.dnslab.",
	}}
	ip := net.ParseIP(host)
	if ip == nil {
		reply.msg.RCode = dnswire.RCodeServFail
		reply.anomaly = "invalid_delegation_address"
		return reply
	}
	if ip.To4() != nil {
		reply.msg.Additionals = []dnswire.RR{{
			Name: "ns.dnslab.", Type: dnswire.TypeA, Class: dnswire.ClassIN, TTL: ttl, A: ip.String(),
		}}
	} else {
		reply.msg.Additionals = []dnswire.RR{{
			Name: "ns.dnslab.", Type: dnswire.TypeAAAA, Class: dnswire.ClassIN, TTL: ttl, A: ip.String(),
		}}
	}
	reply.nextResolver = nextID
	reply.nextAddress = nextAddr
	return reply
}

func (e *Engine) answerAuthority(rt *resolverRuntime, q dnswire.Question) handlerReply {
	reply := handlerReply{
		msg: dnswire.Message{
			Response:           true,
			RecursionDesired:   true,
			RecursionAvailable: false,
			Questions:          []dnswire.Question{q},
			Answers:            []dnswire.RR{},
			Authorities:        []dnswire.RR{},
			Additionals:        []dnswire.RR{},
		},
		cacheStatus: "miss",
	}
	zone := e.zoneForAuthority(rt.cfg.ID, q.Name)
	if zone == nil {
		reply.msg.RCode = dnswire.RCodeRefused
		return reply
	}

	ownerExists := e.authorityOwnerExists(rt.cfg.ID, q.Name)
	if !ownerExists {
		reply.msg.RCode = dnswire.RCodeNXDomain
		reply.msg.Authorities = []dnswire.RR{negativeSOA(zone.cfg.Name)}
		return reply
	}

	answers, found := e.authorityAnswers(rt.cfg.ID, q.Name, q.Type)
	reply.msg.Answers = answers
	if !found && len(answers) == 0 {
		reply.msg.Authorities = []dnswire.RR{negativeSOA(zone.cfg.Name)}
	}
	return reply
}

func (e *Engine) zoneForAuthority(authority, name string) *zoneRuntime {
	var best *zoneRuntime
	for _, zone := range e.zones {
		if zone.authority != authority || !dnswire.IsSubdomain(name, zone.cfg.Name) {
			continue
		}
		if best == nil || len(zone.cfg.Name) > len(best.cfg.Name) {
			best = zone
		}
	}
	return best
}

func (e *Engine) authorityOwnerExists(authority, name string) bool {
	for _, zone := range e.zones {
		if zone.authority != authority {
			continue
		}
		if _, ok := zone.records[name]; ok {
			return true
		}
	}
	return false
}

func (e *Engine) recordsForAuthority(authority, name string) []dnswire.RR {
	var out []dnswire.RR
	for _, zone := range e.zones {
		if zone.authority != authority {
			continue
		}
		out = append(out, zone.records[name]...)
	}
	return out
}

func (e *Engine) authorityAnswers(authority, name string, queryType dnswire.Type) ([]dnswire.RR, bool) {
	var out []dnswire.RR
	seen := map[string]bool{}
	current := name
	for depth := 0; depth < 8; depth++ {
		if seen[current] {
			return out, false
		}
		seen[current] = true
		records := e.recordsForAuthority(authority, current)
		if len(records) == 0 {
			return out, len(out) > 0
		}
		var cname *dnswire.RR
		matched := false
		for i := range records {
			rr := records[i]
			if rr.Type == dnswire.TypeCNAME {
				c := rr
				cname = &c
				continue
			}
			if queryType == dnswire.TypeANY || rr.Type == queryType {
				out = append(out, rr)
				matched = true
			}
		}
		if queryType == dnswire.TypeCNAME && cname != nil {
			out = append(out, *cname)
			return out, true
		}
		if cname != nil && queryType != dnswire.TypeCNAME {
			out = append(out, *cname)
			current = cname.Target
			continue
		}
		return out, matched
	}
	return out, len(out) > 0
}

func negativeSOA(zone string) dnswire.RR {
	return dnswire.RR{
		Name: zone, Type: dnswire.TypeSOA, Class: dnswire.ClassIN, TTL: 60,
		SOA: &dnswire.SOA{
			MName: "ns.dnslab.", RName: "hostmaster.dnslab.",
			Serial: 1, Refresh: 60, Retry: 60, Expire: 300, Minimum: 60,
		},
	}
}

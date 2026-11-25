package projectio

import (
	"strings"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

func ScaledToBP(value int64, scale int) (int64, error) {
	switch scale {
	case 1000:
		return value * 10, nil
	case 10000:
		return value, nil
	default:
		return 0, bizerr.Newf(bizerr.EBudgetScaleUnsupported,
			"budget scale %d is out of supported range {1000,10000}", scale).
			WithContext("scale", scale)
	}
}

func BPToScaled(bp int64, scale int) (int64, error) {
	switch scale {
	case 1000:
		if bp%10 != 0 {
			return 0, bizerr.Newf(bizerr.EBudgetScaleUnsupported, "budget %d BP is not representable at scale 1000", bp)
		}
		return bp / 10, nil
	case 10000:
		return bp, nil
	default:
		return 0, bizerr.Newf(bizerr.EBudgetScaleUnsupported, "budget scale %d is unsupported", scale)
	}
}

func NormalizeProtocol(v string) string {
	v = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), "_", "."))
	v = strings.ReplaceAll(v, "TLSV", "TLS")
	if strings.HasPrefix(v, "TLS") && !strings.Contains(v, ".") {
		if len(v) == 5 { // TLS12 -> TLS1.2
			v = "TLS" + v[3:4] + "." + v[4:]
		}
	}
	return v
}

var protocolOrder = map[string]int{
	"SSL3.0": 30,
	"TLS1.0": 10,
	"TLS1.1": 11,
	"TLS1.2": 12,
	"TLS1.3": 13,
}

func ProtocolRank(v string) (int, bool) {
	r, ok := protocolOrder[NormalizeProtocol(v)]
	return r, ok
}

func ProtocolAllowed(offered, minV, maxV string) bool {
	o, ok1 := ProtocolRank(offered)
	lo, ok2 := ProtocolRank(minV)
	hi, ok3 := ProtocolRank(maxV)
	if !ok1 || !ok2 || !ok3 || lo > hi {
		return false
	}
	return o >= lo && o <= hi
}

func CipherAllowed(offered []string, allowed []string) (string, bool) {
	set := make(map[string]struct{}, len(allowed))
	for _, c := range allowed {
		set[strings.ToUpper(c)] = struct{}{}
	}
	for _, c := range offered {
		if _, ok := set[strings.ToUpper(c)]; ok {
			return c, true
		}
	}
	return "", false
}

func validateCanonical(p *model.Project) error {
	if p.ProjectID == "" {
		return bizerr.New(bizerr.ESchemaFieldInvalid, "project_id is required").At("project_id")
	}
	if p.ThreadBudget <= 0 {
		return bizerr.New(bizerr.ESchemaFieldInvalid, "thread_budget must be positive").At("thread_budget")
	}
	if len(p.Endpoints) == 0 {
		return bizerr.New(bizerr.ESchemaFieldInvalid, "at least one endpoint is required").At("endpoints")
	}
	seenEP := map[string]struct{}{}
	for i := range p.Endpoints {
		ep := &p.Endpoints[i]
		if ep.ID == "" {
			return bizerr.New(bizerr.ESchemaFieldInvalid, "endpoint id/name is required").At("endpoints[" + itoa(i) + "].id")
		}
		if _, ok := seenEP[ep.ID]; ok {
			return bizerr.Newf(bizerr.ESchemaFieldConflict, "duplicate endpoint %q", ep.ID).At("endpoints")
		}
		seenEP[ep.ID] = struct{}{}
		if ep.PeriodMs <= 0 || ep.TimeoutMs <= 0 {
			return bizerr.New(bizerr.ESchemaFieldInvalid, "period and timeout must be positive").At("endpoints." + ep.ID)
		}
		if ep.MaxParallel <= 0 || ep.BufferCapacity <= 0 {
			return bizerr.New(bizerr.ESchemaFieldInvalid, "max_parallel and buffer_capacity must be positive").At("endpoints." + ep.ID)
		}
		if ep.BudgetBP <= 0 {
			return bizerr.New(bizerr.ESchemaFieldInvalid, "budget must be positive after scale normalization").At("endpoints." + ep.ID)
		}
		if !ProtocolAllowed(ep.MinProtocol, ep.MinProtocol, ep.MaxProtocol) {
			return bizerr.Newf(bizerr.ESchemaFieldConflict, "invalid protocol range %q..%q", ep.MinProtocol, ep.MaxProtocol).
				At("endpoints." + ep.ID + ".protocol_range")
		}
		for _, cid := range ep.ChainIDs {
			if _, ok := p.Chain(cid); !ok {
				return bizerr.Newf(bizerr.ESchemaFieldInvalid, "endpoint references unknown chain %q", cid).
					At("endpoints." + ep.ID + ".candidate_chains")
			}
		}
	}
	seenChain := map[string]struct{}{}
	for i := range p.Chains {
		ch := &p.Chains[i]
		if ch.ID == "" {
			return bizerr.New(bizerr.ESchemaFieldInvalid, "chain id is required").At("chains[" + itoa(i) + "].id")
		}
		if _, ok := seenChain[ch.ID]; ok {
			return bizerr.Newf(bizerr.ESchemaFieldConflict, "duplicate chain %q", ch.ID).At("chains")
		}
		seenChain[ch.ID] = struct{}{}
		if len(ch.Certificates) == 0 {
			return bizerr.Newf(bizerr.ESchemaFieldInvalid, "chain %q has no certificates", ch.ID).At("chains." + ch.ID)
		}
		if err := ValidateChainStructure(*ch); err != nil {
			return err
		}
	}
	for i, ev := range p.Events {
		if ev.Kind != model.KindHandshake && ev.Kind != model.KindChainUpdate {
			return bizerr.Newf(bizerr.ESchemaFieldInvalid, "unknown event kind %q", ev.Kind).At("events[" + itoa(i) + "].kind")
		}
		if ev.AtMs < 0 {
			return bizerr.New(bizerr.ESchemaFieldInvalid, "event time cannot be negative").At("events[" + itoa(i) + "].at")
		}
		if ev.Request != nil {
			if _, ok := p.Endpoint(ev.Request.EndpointID); !ok {
				return bizerr.Newf(bizerr.ENotFound, "request references unknown endpoint %q", ev.Request.EndpointID).At("events.request.endpoint")
			}
		}
		if ev.Update != nil {
			ep, ok := p.Endpoint(ev.Update.EndpointID)
			if !ok {
				return bizerr.Newf(bizerr.ENotFound, "update references unknown endpoint %q", ev.Update.EndpointID).At("events.update.endpoint")
			}
			if _, ok := p.Chain(ev.Update.ChainID); !ok {
				return bizerr.Newf(bizerr.ENotFound, "update references unknown chain %q", ev.Update.ChainID).At("events.update.chain")
			}
			if ev.Update.CommitAtMs < ev.AtMs {
				return bizerr.New(bizerr.ESchemaFieldConflict, "commit time precedes update time").At("events.update.commit_at")
			}
			_ = ep
		}
	}
	return nil
}

func ValidateChainStructure(ch model.Chain) error {
	subjectToIssuer := map[string]string{}
	for i, cert := range ch.Certificates {
		if cert.Digest == "" || cert.Subject == "" || cert.Issuer == "" {
			return bizerr.Newf(bizerr.ESchemaFieldInvalid, "chain %q level %d is missing digest/subject/issuer", ch.ID, i).
				At("chains." + ch.ID + ".certificates[" + itoa(i) + "]")
		}
		if cert.NotAfterMs < cert.NotBeforeMs {
			return bizerr.Newf(bizerr.ESchemaFieldConflict, "certificate %q has inverted validity interval", cert.Digest).
				At("chains." + ch.ID + ".certificates[" + itoa(i) + "].validity")
		}
		if old, exists := subjectToIssuer[cert.Subject]; exists && old != cert.Issuer {
			return bizerr.Newf(bizerr.EChainCycle, "chain %q has conflicting issuer mapping for %q", ch.ID, cert.Subject).
				WithContext("chain_id", ch.ID).WithContext("subject", cert.Subject)
		}
		subjectToIssuer[cert.Subject] = cert.Issuer
	}
	root := ch.Certificates[len(ch.Certificates)-1]
	if root.Issuer != root.Subject {
		// A non-self-signed final certificate is acceptable only if its issuer is
		// a trust anchor outside the supplied chain. Cycles are still rejected.
		visited := map[string]int{}
		current := root.Subject
		for step := 0; step <= len(ch.Certificates)+1; step++ {
			if issuer, ok := subjectToIssuer[current]; ok {
				if visited[current] > 0 && issuer != current {
					return bizerr.Newf(bizerr.EChainCycle, "issuer/subject cycle detected in chain %q at %q", ch.ID, current).
						WithContext("chain_id", ch.ID).WithContext("subject", current)
				}
				visited[current]++
				if issuer == current {
					return nil
				}
				current = issuer
				continue
			}
			return nil
		}
		return bizerr.Newf(bizerr.EChainCycle, "issuer/subject cycle detected in chain %q", ch.ID).WithContext("chain_id", ch.ID)
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

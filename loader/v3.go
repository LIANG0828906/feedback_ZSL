package loader

import (
	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

type v3RawRule struct {
	number   string
	suffix   string
	priority int
	upstream int
	region   string
}

func loadV3(root map[string]any) (*spec.Project, error) {
	meta, err := reqMap(root, "metadata", "$")
	if err != nil {
		return nil, err
	}
	projectID, err := reqString(meta, "projectId", "$.metadata")
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(meta, "seed", "$.metadata")
	if err != nil {
		return nil, err
	}
	version, err := reqString(meta, "snapshotVersion", "$.metadata")
	if err != nil {
		return nil, err
	}
	clock, err := reqMap(meta, "logicalClock", "$.metadata")
	if err != nil {
		return nil, err
	}
	refMS, err := reqInt(clock, "referenceEpochMs", "$.metadata.logicalClock")
	if err != nil {
		return nil, err
	}
	settings, err := reqMap(root, "settings", "$")
	if err != nil {
		return nil, err
	}
	defaultTimeout, err := reqInt(settings, "defaultTimeoutMs", "$.settings")
	if err != nil {
		return nil, err
	}
	workers, err := reqInt(settings, "workerThreads", "$.settings")
	if err != nil {
		return nil, err
	}
	hops, err := reqInt(settings, "maxForwardHops", "$.settings")
	if err != nil {
		return nil, err
	}
	p := &spec.Project{
		ProjectID: projectID, Seed: seed, Version: version, ReferenceNowMS: refMS,
		DefaultTimeoutMS: defaultTimeout, WorkerBudget: int(workers), MaxHops: int(hops),
	}
	if negV, ok := settings["negativeCache"]; ok && negV != nil {
		nm, err := asMap(negV, "$.settings.negativeCache")
		if err != nil {
			return nil, err
		}
		p.NegativeCacheEnabled = boolOr(nm, "enabled", false)
		if v, ok := nm["defaultTtlMs"]; ok {
			n, err := asInt(v, "$.settings.negativeCache.defaultTtlMs")
			if err != nil {
				return nil, err
			}
			if err := fixedint.ValidateTTLMS(n); err != nil {
				return nil, err
			}
			p.NegativeDefaultTTLMS = n
		}
	}

	pool, err := reqArray(root, "upstreamPool", "$")
	if err != nil {
		return nil, err
	}
	for i, uv := range pool {
		up := "$.upstreamPool[" + itoa(i) + "]"
		um, err := asMap(uv, up)
		if err != nil {
			return nil, err
		}
		number, err := reqInt(um, "number", up)
		if err != nil {
			return nil, err
		}
		if int(number) != i+1 {
			return nil, codes.New(codes.EFieldInvalid, "upstream pool number must match ordered array position").
				WithField(up+".number").WithContext("declared", number).WithContext("expected", i+1)
		}
		name, err := reqString(um, "name", up)
		if err != nil {
			return nil, err
		}
		literal, err := reqString(um, "literal", up)
		if err != nil {
			return nil, err
		}
		timeoutObj, err := reqMap(um, "timeout", up)
		if err != nil {
			return nil, err
		}
		timeout, err := reqInt(timeoutObj, "milliseconds", up+".timeout")
		if err != nil {
			return nil, err
		}
		latency, err := reqInt(um, "latencyMilliseconds", up)
		if err != nil {
			return nil, err
		}
		u := spec.Upstream{Index: i + 1, ID: name, Literal: literal, TimeoutMS: timeout, LatencyMS: latency}
		if fwdV, ok := um["forwardToNumber"]; ok && fwdV != nil {
			fwd, err := asInt(fwdV, up+".forwardToNumber")
			if err != nil {
				return nil, err
			}
			u.ForwardTo = int(fwd)
		}
		p.Upstreams = append(p.Upstreams, u)
	}

	tree, err := reqArray(root, "regionTree", "$")
	if err != nil {
		return nil, err
	}
	var rawRules []v3RawRule
	var walk func(nodes []any, path, parent string) error
	walk = func(nodes []any, path, parent string) error {
		for i, nv := range nodes {
			np := path + "[" + itoa(i) + "]"
			nm, err := asMap(nv, np)
			if err != nil {
				return err
			}
			id, err := reqString(nm, "regionId", np)
			if err != nil {
				return err
			}
			domain, err := reqString(nm, "domain", np)
			if err != nil {
				return err
			}
			p.Regions = append(p.Regions, spec.Region{ID: id, Name: domain, ParentID: parent})
			ruleArr, err := reqArray(nm, "rules", np)
			if err != nil {
				return err
			}
			for j, rv := range ruleArr {
				rp := np + ".rules[" + itoa(j) + "]"
				rm, err := asMap(rv, rp)
				if err != nil {
					return err
				}
				number, err := reqString(rm, "ruleId", rp)
				if err != nil {
					return err
				}
				match, err := reqString(rm, "match", rp)
				if err != nil {
					return err
				}
				precedence, err := reqInt(rm, "precedence", rp)
				if err != nil {
					return err
				}
				ref, err := reqInt(rm, "upstreamRef", rp)
				if err != nil {
					return err
				}
				rawRules = append(rawRules, v3RawRule{
					number: number, suffix: match, priority: int(precedence),
					upstream: int(ref), region: id,
				})
			}
			childV, ok := nm["children"]
			if !ok || childV == nil {
				continue
			}
			children, err := asArray(childV, np+".children")
			if err != nil {
				return err
			}
			if err := walk(children, np+".children", id); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree, "$.regionTree", ""); err != nil {
		return nil, err
	}
	for i, rr := range rawRules {
		p.Rules = append(p.Rules, spec.Rule{
			Ordinal: i, Number: rr.number, RegionID: rr.region, Suffix: rr.suffix,
			Priority: rr.priority, UpstreamIndex: rr.upstream,
		})
	}

	cacheObj, err := reqMap(root, "cache", "$")
	if err != nil {
		return nil, err
	}
	segments, err := reqArray(cacheObj, "segments", "$.cache")
	if err != nil {
		return nil, err
	}
	for i, sv := range segments {
		sp := "$.cache.segments[" + itoa(i) + "]"
		sm, err := asMap(sv, sp)
		if err != nil {
			return nil, err
		}
		region, err := reqString(sm, "regionId", sp)
		if err != nil {
			return nil, err
		}
		entries, err := reqArray(sm, "entries", sp)
		if err != nil {
			return nil, err
		}
		for j, ev := range entries {
			ep := sp + ".entries[" + itoa(j) + "]"
			em, err := asMap(ev, ep)
			if err != nil {
				return nil, err
			}
			qname, err := reqString(em, "qname", ep)
			if err != nil {
				return nil, err
			}
			qtype, err := reqString(em, "qtype", ep)
			if err != nil {
				return nil, err
			}
			negative := boolOr(em, "negative", false)
			seed := spec.CacheSeed{
				CacheKey: spec.Key(qname, qtype), QName: qname, QType: qtype,
				RegionID: region, Negative: negative,
			}
			if negative {
				exp, err := reqInt(em, "negativeExpiresAtMs", ep)
				if err != nil {
					return nil, err
				}
				ttl := exp - refMS
				if err := fixedint.ValidateTTLMS(ttl); err != nil {
					return nil, err.(*codes.Error).
						WithContext("absolute_expires_at_ms", exp).
						WithContext("reference_now_ms", refMS)
				}
				seed.NegativeExpiresAtMS = exp
				seed.NegativeTTLMS = ttl
			} else {
				recArr, err := reqArray(em, "records", ep)
				if err != nil {
					return nil, err
				}
				for k, rv := range recArr {
					rp := ep + ".records[" + itoa(k) + "]"
					rm, err := asMap(rv, rp)
					if err != nil {
						return nil, err
					}
					name, err := reqString(rm, "name", rp)
					if err != nil {
						return nil, err
					}
					rtype, err := reqString(rm, "type", rp)
					if err != nil {
						return nil, err
					}
					value, err := reqString(rm, "value", rp)
					if err != nil {
						return nil, err
					}
					exp, err := reqInt(rm, "expiresAtMs", rp)
					if err != nil {
						return nil, err
					}
					ttl := exp - refMS
					if err := fixedint.ValidateTTLMS(ttl); err != nil {
						return nil, err.(*codes.Error).
							WithContext("absolute_expires_at_ms", exp).
							WithContext("reference_now_ms", refMS)
					}
					seed.Records = append(seed.Records, spec.Record{
						Name: name, Type: rtype, Value: value, TTLMS: ttl, ExpiresAtMS: exp,
					})
				}
			}
			p.CacheSeeds = append(p.CacheSeeds, seed)
		}
	}

	answers, err := reqArray(root, "answers", "$")
	if err != nil {
		return nil, err
	}
	for i, av := range answers {
		ap := "$.answers[" + itoa(i) + "]"
		am, err := asMap(av, ap)
		if err != nil {
			return nil, err
		}
		qname, err := reqString(am, "qname", ap)
		if err != nil {
			return nil, err
		}
		qtype, err := reqString(am, "qtype", ap)
		if err != nil {
			return nil, err
		}
		region, err := reqString(am, "regionId", ap)
		if err != nil {
			return nil, err
		}
		recArr, err := reqArray(am, "records", ap)
		if err != nil {
			return nil, err
		}
		set := spec.AnswerSet{CacheKey: spec.Key(qname, qtype), QName: qname, QType: qtype, RegionID: region}
		for j, rv := range recArr {
			rp := ap + ".records[" + itoa(j) + "]"
			rm, err := asMap(rv, rp)
			if err != nil {
				return nil, err
			}
			name, err := reqString(rm, "name", rp)
			if err != nil {
				return nil, err
			}
			rtype, err := reqString(rm, "type", rp)
			if err != nil {
				return nil, err
			}
			value, err := reqString(rm, "value", rp)
			if err != nil {
				return nil, err
			}
			ttl, err := reqInt(rm, "validForMs", rp)
			if err != nil {
				return nil, err
			}
			if err := fixedint.ValidateTTLMS(ttl); err != nil {
				return nil, err
			}
			set.Records = append(set.Records, spec.Record{Name: name, Type: rtype, Value: value, TTLMS: ttl})
		}
		p.AnswerSets = append(p.AnswerSets, set)
	}
	return finalizeProject(p)
}

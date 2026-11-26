package loader

import (
	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

func loadV2(root map[string]any) (*spec.Project, error) {
	projectID, err := reqString(root, "projectId", "$")
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(root, "seed", "$")
	if err != nil {
		return nil, err
	}
	version, err := reqString(root, "version", "$")
	if err != nil {
		return nil, err
	}
	refMS, err := reqInt(root, "referenceNowMs", "$")
	if err != nil {
		return nil, err
	}
	defaultTimeout, err := reqInt(root, "defaultTimeoutMs", "$")
	if err != nil {
		return nil, err
	}
	workers, err := reqInt(root, "workerBudget", "$")
	if err != nil {
		return nil, err
	}
	hops, err := reqInt(root, "maxHops", "$")
	if err != nil {
		return nil, err
	}
	p := &spec.Project{
		ProjectID: projectID, Seed: seed, Version: version, ReferenceNowMS: refMS,
		DefaultTimeoutMS: defaultTimeout, WorkerBudget: int(workers), MaxHops: int(hops),
		NegativeCacheEnabled: boolOr(root, "negativeCacheEnabled", false),
	}
	if v, ok := root["negativeDefaultTtlMs"]; ok {
		n, err := asInt(v, "$.negativeDefaultTtlMs")
		if err != nil {
			return nil, err
		}
		if err := fixedint.ValidateTTLMS(n); err != nil {
			return nil, err
		}
		p.NegativeDefaultTTLMS = n
	}

	zones, err := reqArray(root, "zones", "$")
	if err != nil {
		return nil, err
	}
	for i, zv := range zones {
		zp := "$.zones[" + itoa(i) + "]"
		zm, err := asMap(zv, zp)
		if err != nil {
			return nil, err
		}
		id, err := reqString(zm, "id", zp)
		if err != nil {
			return nil, err
		}
		name, err := reqString(zm, "name", zp)
		if err != nil {
			return nil, err
		}
		parent, _ := zm["parentId"].(string)
		p.Regions = append(p.Regions, spec.Region{ID: id, Name: name, ParentID: parent})
	}

	ups, err := reqArray(root, "upstreams", "$")
	if err != nil {
		return nil, err
	}
	for i, uv := range ups {
		up := "$.upstreams[" + itoa(i) + "]"
		um, err := asMap(uv, up)
		if err != nil {
			return nil, err
		}
		idx, err := reqInt(um, "upstreamIndex", up)
		if err != nil {
			return nil, err
		}
		if int(idx) != i+1 {
			return nil, codes.New(codes.EFieldInvalid, "ordered upstream index must equal array position plus one").
				WithField(up+".upstreamIndex").
				WithContext("declared", idx).
				WithContext("expected", i+1)
		}
		id, err := reqString(um, "upstreamId", up)
		if err != nil {
			return nil, err
		}
		address, err := reqString(um, "address", up)
		if err != nil {
			return nil, err
		}
		timeout, err := reqInt(um, "timeoutMs", up)
		if err != nil {
			return nil, err
		}
		latency, err := reqInt(um, "latencyMs", up)
		if err != nil {
			return nil, err
		}
		u := spec.Upstream{Index: i + 1, ID: id, Literal: address, TimeoutMS: timeout, LatencyMS: latency}
		if fwdV, ok := um["forwardToIndex"]; ok && fwdV != nil {
			fwd, err := asInt(fwdV, up+".forwardToIndex")
			if err != nil {
				return nil, err
			}
			u.ForwardTo = int(fwd)
		}
		p.Upstreams = append(p.Upstreams, u)
	}

	rules, err := reqArray(root, "rules", "$")
	if err != nil {
		return nil, err
	}
	for i, rv := range rules {
		rp := "$.rules[" + itoa(i) + "]"
		rm, err := asMap(rv, rp)
		if err != nil {
			return nil, err
		}
		number, err := reqString(rm, "ruleNumber", rp)
		if err != nil {
			return nil, err
		}
		region, err := reqString(rm, "zoneId", rp)
		if err != nil {
			return nil, err
		}
		suffix, err := reqString(rm, "matchSuffix", rp)
		if err != nil {
			return nil, err
		}
		priority, err := reqInt(rm, "priority", rp)
		if err != nil {
			return nil, err
		}
		upIdx, err := reqInt(rm, "upstreamIndex", rp)
		if err != nil {
			return nil, err
		}
		p.Rules = append(p.Rules, spec.Rule{
			Ordinal: i, Number: number, RegionID: region, Suffix: suffix,
			Priority: int(priority), UpstreamIndex: int(upIdx),
		})
	}

	cache, err := reqArray(root, "cacheSeeds", "$")
	if err != nil {
		return nil, err
	}
	for i, cv := range cache {
		cp := "$.cacheSeeds[" + itoa(i) + "]"
		cm, err := asMap(cv, cp)
		if err != nil {
			return nil, err
		}
		qname, err := reqString(cm, "qname", cp)
		if err != nil {
			return nil, err
		}
		qtype, err := reqString(cm, "qtype", cp)
		if err != nil {
			return nil, err
		}
		region, err := reqString(cm, "regionId", cp)
		if err != nil {
			return nil, err
		}
		negative := boolOr(cm, "negative", false)
		seed := spec.CacheSeed{
			CacheKey: spec.Key(qname, qtype), QName: qname, QType: qtype,
			RegionID: region, Negative: negative,
		}
		if negative {
			ttl, err := reqInt(cm, "negativeTtlMs", cp)
			if err != nil {
				return nil, err
			}
			if err := fixedint.ValidateTTLMS(ttl); err != nil {
				return nil, err
			}
			exp, err := absoluteFromRemaining(refMS, ttl, cp+".negativeTtlMs")
			if err != nil {
				return nil, err
			}
			seed.NegativeTTLMS = ttl
			seed.NegativeExpiresAtMS = exp
		}
		if recV, ok := cm["records"]; ok && recV != nil {
			recArr, err := asArray(recV, cp+".records")
			if err != nil {
				return nil, err
			}
			for j, rv := range recArr {
				rpath := cp + ".records[" + itoa(j) + "]"
				rm, err := asMap(rv, rpath)
				if err != nil {
					return nil, err
				}
				name, err := reqString(rm, "name", rpath)
				if err != nil {
					return nil, err
				}
				rtype, err := reqString(rm, "type", rpath)
				if err != nil {
					return nil, err
				}
				value, err := reqString(rm, "value", rpath)
				if err != nil {
					return nil, err
				}
				ttl, err := reqInt(rm, "ttlMs", rpath)
				if err != nil {
					return nil, err
				}
				if err := fixedint.ValidateTTLMS(ttl); err != nil {
					return nil, err
				}
				exp, err := absoluteFromRemaining(refMS, ttl, rpath+".ttlMs")
				if err != nil {
					return nil, err
				}
				seed.Records = append(seed.Records, spec.Record{
					Name: name, Type: rtype, Value: value, TTLMS: ttl, ExpiresAtMS: exp,
				})
			}
		}
		if !negative && len(seed.Records) == 0 {
			return nil, codes.New(codes.EFieldMissing, "positive cache seed requires records").
				WithField(cp + ".records")
		}
		p.CacheSeeds = append(p.CacheSeeds, seed)
	}

	answers, err := reqArray(root, "answerSets", "$")
	if err != nil {
		return nil, err
	}
	for i, av := range answers {
		ap := "$.answerSets[" + itoa(i) + "]"
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
			ttl, err := reqInt(rm, "ttlMs", rp)
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

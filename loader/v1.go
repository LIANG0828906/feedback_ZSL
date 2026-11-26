package loader

import (
	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

func loadV1(root map[string]any) (*spec.Project, error) {
	const path = "$"
	projectID, err := reqString(root, "project_id", path)
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(root, "seed", path)
	if err != nil {
		return nil, err
	}
	version, err := reqString(root, "version", path)
	if err != nil {
		return nil, err
	}
	refSec, err := reqInt(root, "reference_now_seconds", path)
	if err != nil {
		return nil, err
	}
	refMS, err := msFromSeconds(refSec, "$.reference_now_seconds")
	if err != nil {
		return nil, err
	}
	timeoutSec, err := reqInt(root, "default_timeout_seconds", path)
	if err != nil {
		return nil, err
	}
	defaultTimeoutMS, err := msFromSeconds(timeoutSec, "$.default_timeout_seconds")
	if err != nil {
		return nil, err
	}
	workerBudget, err := reqInt(root, "worker_budget", path)
	if err != nil {
		return nil, err
	}
	maxHops, err := reqInt(root, "max_hops", path)
	if err != nil {
		return nil, err
	}

	p := &spec.Project{
		ProjectID: projectID, Seed: seed, Version: version, ReferenceNowMS: refMS,
		DefaultTimeoutMS: defaultTimeoutMS, WorkerBudget: int(workerBudget), MaxHops: int(maxHops),
	}
	if neg, ok := root["negative_cache"]; ok && neg != nil {
		nm, err := asMap(neg, "$.negative_cache")
		if err != nil {
			return nil, err
		}
		p.NegativeCacheEnabled = boolOr(nm, "enabled", false)
		if v, exists := nm["default_ttl_seconds"]; exists {
			n, err := asInt(v, "$.negative_cache.default_ttl_seconds")
			if err != nil {
				return nil, err
			}
			ms, err := msFromSeconds(n, "$.negative_cache.default_ttl_seconds")
			if err != nil {
				return nil, err
			}
			if err := fixedint.ValidateTTLMS(ms); err != nil {
				return nil, err
			}
			p.NegativeDefaultTTLMS = ms
		}
	}

	zoneArr, err := reqArray(root, "zones", path)
	if err != nil {
		return nil, err
	}
	for i, zv := range zoneArr {
		zp := "$.zones[" + itoa(i) + "]"
		zm, err := asMap(zv, zp)
		if err != nil {
			return nil, err
		}
		id, err := reqString(zm, "zone_id", zp)
		if err != nil {
			return nil, err
		}
		name, err := reqString(zm, "domain_name", zp)
		if err != nil {
			return nil, err
		}
		parent, _ := zm["parent_zone"].(string)
		p.Regions = append(p.Regions, spec.Region{ID: id, Name: name, ParentID: parent})
	}

	profilesV, err := reqMap(root, "authority_profiles", path)
	if err != nil {
		return nil, err
	}
	literalToIndex := map[string]int{}
	var intern func(literal, field string) (int, error)
	intern = func(literal, field string) (int, error) {
		if idx, ok := literalToIndex[literal]; ok {
			return idx, nil
		}
		pv, ok := profilesV[literal]
		if !ok {
			return 0, codes.New(codes.EUpstreamReference, "authority literal has no declared profile").
				WithField(field).WithContext("literal", literal)
		}
		pm, err := asMap(pv, "$.authority_profiles")
		if err != nil {
			return 0, err
		}
		id, err := reqString(pm, "authority_id", "$.authority_profiles."+literal)
		if err != nil {
			return 0, err
		}
		timeoutSec, err := reqInt(pm, "timeout_seconds", "$.authority_profiles."+literal)
		if err != nil {
			return 0, err
		}
		timeoutMS, err := msFromSeconds(timeoutSec, "$.authority_profiles."+literal+".timeout_seconds")
		if err != nil {
			return 0, err
		}
		latency, err := reqInt(pm, "simulated_latency_ms", "$.authority_profiles."+literal)
		if err != nil {
			return 0, err
		}
		if latency < 0 {
			return 0, codes.New(codes.EFieldInvalid, "simulated latency must be non-negative").
				WithContext("literal", literal).WithContext("latency_ms", latency)
		}
		idx := len(p.Upstreams) + 1
		literalToIndex[literal] = idx
		p.Upstreams = append(p.Upstreams, spec.Upstream{
			Index: idx, ID: id, Literal: literal, TimeoutMS: timeoutMS, LatencyMS: latency,
		})
		if nextV, exists := pm["next_authority"]; exists && nextV != nil && nextV != "" {
			next, err := asString(nextV, "$.authority_profiles."+literal+".next_authority")
			if err != nil {
				return 0, err
			}
			nextIdx, err := intern(next, "$.authority_profiles."+literal+".next_authority")
			if err != nil {
				return 0, err
			}
			p.Upstreams[idx-1].ForwardTo = nextIdx
		}
		return idx, nil
	}

	ruleArr, err := reqArray(root, "flat_rules", path)
	if err != nil {
		return nil, err
	}
	for i, rv := range ruleArr {
		rp := "$.flat_rules[" + itoa(i) + "]"
		rm, err := asMap(rv, rp)
		if err != nil {
			return nil, err
		}
		number, err := reqString(rm, "rule_id", rp)
		if err != nil {
			return nil, err
		}
		regionID, err := reqString(rm, "zone_id", rp)
		if err != nil {
			return nil, err
		}
		suffix, err := reqString(rm, "match_suffix", rp)
		if err != nil {
			return nil, err
		}
		priority, err := reqInt(rm, "priority", rp)
		if err != nil {
			return nil, err
		}
		literal, err := reqString(rm, "authority", rp)
		if err != nil {
			return nil, err
		}
		upIdx, err := intern(literal, rp+".authority")
		if err != nil {
			return nil, err
		}
		p.Rules = append(p.Rules, spec.Rule{
			Ordinal: i, Number: number, RegionID: regionID, Suffix: suffix,
			Priority: int(priority), UpstreamIndex: upIdx,
		})
	}

	if err := loadV1Cache(root, p, refMS); err != nil {
		return nil, err
	}
	if err := loadV1Answers(root, p); err != nil {
		return nil, err
	}
	return finalizeProject(p)
}

func loadV1Cache(root map[string]any, p *spec.Project, refMS int64) error {
	arr, err := reqArray(root, "cache_entries", "$")
	if err != nil {
		return err
	}
	for i, cv := range arr {
		cp := "$.cache_entries[" + itoa(i) + "]"
		cm, err := asMap(cv, cp)
		if err != nil {
			return err
		}
		qname, err := reqString(cm, "qname", cp)
		if err != nil {
			return err
		}
		qtype, err := reqString(cm, "qtype", cp)
		if err != nil {
			return err
		}
		regionID, err := reqString(cm, "zone_id", cp)
		if err != nil {
			return err
		}
		seed := spec.CacheSeed{CacheKey: spec.Key(qname, qtype), QName: qname, QType: qtype, RegionID: regionID}
		if negV, exists := cm["negative_ttl_seconds"]; exists {
			n, err := asInt(negV, cp+".negative_ttl_seconds")
			if err != nil {
				return err
			}
			ms, err := msFromSeconds(n, cp+".negative_ttl_seconds")
			if err != nil {
				return err
			}
			if err := fixedint.ValidateTTLMS(ms); err != nil {
				return err
			}
			exp, err := absoluteFromRemaining(refMS, ms, cp+".negative_ttl_seconds")
			if err != nil {
				return err
			}
			seed.Negative = true
			seed.NegativeTTLMS = ms
			seed.NegativeExpiresAtMS = exp
		} else {
			recArr, err := reqArray(cm, "records", cp)
			if err != nil {
				return err
			}
			for j, rv := range recArr {
				rp := cp + ".records[" + itoa(j) + "]"
				rm, err := asMap(rv, rp)
				if err != nil {
					return err
				}
				name, err := reqString(rm, "rr_name", rp)
				if err != nil {
					return err
				}
				rtype, err := reqString(rm, "rr_type", rp)
				if err != nil {
					return err
				}
				value, err := reqString(rm, "rdata", rp)
				if err != nil {
					return err
				}
				ttlSec, err := reqInt(rm, "ttl_seconds", rp)
				if err != nil {
					return err
				}
				ttlMS, err := msFromSeconds(ttlSec, rp+".ttl_seconds")
				if err != nil {
					return err
				}
				if err := fixedint.ValidateTTLMS(ttlMS); err != nil {
					return err
				}
				exp, err := absoluteFromRemaining(refMS, ttlMS, rp+".ttl_seconds")
				if err != nil {
					return err
				}
				seed.Records = append(seed.Records, spec.Record{
					Name: name, Type: rtype, Value: value, TTLMS: ttlMS, ExpiresAtMS: exp,
				})
			}
		}
		p.CacheSeeds = append(p.CacheSeeds, seed)
	}
	return nil
}

func loadV1Answers(root map[string]any, p *spec.Project) error {
	arr, err := reqArray(root, "answer_sets", "$")
	if err != nil {
		return err
	}
	for i, av := range arr {
		ap := "$.answer_sets[" + itoa(i) + "]"
		am, err := asMap(av, ap)
		if err != nil {
			return err
		}
		qname, err := reqString(am, "qname", ap)
		if err != nil {
			return err
		}
		qtype, err := reqString(am, "qtype", ap)
		if err != nil {
			return err
		}
		regionID, err := reqString(am, "zone_id", ap)
		if err != nil {
			return err
		}
		recArr, err := reqArray(am, "records", ap)
		if err != nil {
			return err
		}
		set := spec.AnswerSet{CacheKey: spec.Key(qname, qtype), QName: qname, QType: qtype, RegionID: regionID}
		for j, rv := range recArr {
			rp := ap + ".records[" + itoa(j) + "]"
			rm, err := asMap(rv, rp)
			if err != nil {
				return err
			}
			name, err := reqString(rm, "rr_name", rp)
			if err != nil {
				return err
			}
			rtype, err := reqString(rm, "rr_type", rp)
			if err != nil {
				return err
			}
			value, err := reqString(rm, "rdata", rp)
			if err != nil {
				return err
			}
			ttlSec, err := reqInt(rm, "ttl_seconds", rp)
			if err != nil {
				return err
			}
			ttlMS, err := msFromSeconds(ttlSec, rp+".ttl_seconds")
			if err != nil {
				return err
			}
			if err := fixedint.ValidateTTLMS(ttlMS); err != nil {
				return err
			}
			set.Records = append(set.Records, spec.Record{Name: name, Type: rtype, Value: value, TTLMS: ttlMS})
		}
		p.AnswerSets = append(p.AnswerSets, set)
	}
	return nil
}

func itoa(i int) string {
	// local allocation-free enough for diagnostic documents
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

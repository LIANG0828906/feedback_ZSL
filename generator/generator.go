// Package generator deterministically constructs the DNS engineering sample,
// its three schema serializations, query samples, and expected truth.
package generator

import (
	"fmt"
	"math/rand"

	"dnslab/spec"
)

const ReferenceNowMS = int64(1_764_000_000_000)

// Generate builds a complete deterministic bundle for seed.
func Generate(seed int64) (*spec.Bundle, error) {
	rng := rand.New(rand.NewSource(seed))
	p := buildProject(seed, rng)
	queries := buildQueries()
	truth := buildTruth(p, queries)
	docs, err := buildDocuments(p)
	if err != nil {
		return nil, err
	}
	return &spec.Bundle{Project: p, Queries: queries, Truth: truth, Docs: docs}, nil
}

func buildProject(seed int64, rng *rand.Rand) *spec.Project {
	txt := fmt.Sprintf("seed-%d-nonce-%04d", seed, rng.Intn(10000))
	p := &spec.Project{
		ProjectID:            fmt.Sprintf("dns-project-%d", seed),
		Seed:                 seed,
		ReferenceNowMS:       ReferenceNowMS,
		DefaultTimeoutMS:     2000,
		WorkerBudget:         4,
		MaxHops:              4,
		StrictOverlap:        false,
		NegativeCacheEnabled: true,
		NegativeDefaultTTLMS: 30_000,
		Regions: []spec.Region{
			{ID: "rg-example", Name: "example.test"},
			{ID: "rg-api", Name: "api.example.test", ParentID: "rg-example"},
			{ID: "rg-dual-low", Name: "dual-low.example.test", ParentID: "rg-example"},
			{ID: "rg-dual-high", Name: "dual-high.example.test", ParentID: "rg-example"},
			{ID: "rg-tie-first", Name: "tie-first.example.test", ParentID: "rg-example"},
			{ID: "rg-tie-second", Name: "tie-second.example.test", ParentID: "rg-example"},
			{ID: "rg-slow", Name: "slow.example.test", ParentID: "rg-example"},
			{ID: "rg-lab", Name: "lab.test"},
			{ID: "rg-chain", Name: "chain.lab.test", ParentID: "rg-lab"},
			{ID: "rg-cycle", Name: "cycle.lab.test", ParentID: "rg-lab"},
			{ID: "rg-longhop", Name: "long.lab.test", ParentID: "rg-lab"},
		},
		Upstreams: []spec.Upstream{
			{Index: 1, ID: "u-edge-1", Literal: "10.42.0.1:53", TimeoutMS: 2000, LatencyMS: 1},
			{Index: 2, ID: "u-edge-2", Literal: "10.42.0.2:53", TimeoutMS: 2000, LatencyMS: 2},
			{Index: 3, ID: "u-edge-3", Literal: "10.42.0.3:53", TimeoutMS: 1000, LatencyMS: 1},
			{Index: 4, ID: "u-slow", Literal: "10.42.9.14:53", TimeoutMS: 1000, LatencyMS: 2500},
			{Index: 5, ID: "u-chain-3", Literal: "10.42.1.6:53", TimeoutMS: 1000, LatencyMS: 2},
			{Index: 6, ID: "u-chain-1", Literal: "10.42.1.4:53", TimeoutMS: 1000, LatencyMS: 2, ForwardTo: 7},
			{Index: 7, ID: "u-chain-2", Literal: "10.42.1.5:53", TimeoutMS: 1000, LatencyMS: 2, ForwardTo: 5},
			{Index: 8, ID: "u-cycle-a", Literal: "10.42.2.7:53", TimeoutMS: 1000, LatencyMS: 1, ForwardTo: 9},
			{Index: 9, ID: "u-cycle-b", Literal: "10.42.2.8:53", TimeoutMS: 1000, LatencyMS: 1, ForwardTo: 8},
			{Index: 10, ID: "u-long-1", Literal: "10.42.3.9:53", TimeoutMS: 1000, LatencyMS: 1, ForwardTo: 11},
			{Index: 11, ID: "u-long-2", Literal: "10.42.3.10:53", TimeoutMS: 1000, LatencyMS: 1, ForwardTo: 12},
			{Index: 12, ID: "u-long-3", Literal: "10.42.3.11:53", TimeoutMS: 1000, LatencyMS: 1, ForwardTo: 13},
			{Index: 13, ID: "u-long-4", Literal: "10.42.3.12:53", TimeoutMS: 1000, LatencyMS: 1, ForwardTo: 14},
			{Index: 14, ID: "u-long-5", Literal: "10.42.3.13:53", TimeoutMS: 1000, LatencyMS: 1},
		},
		Rules: []spec.Rule{
			{Ordinal: 0, Number: "R-001", RegionID: "rg-example", Suffix: "example.test", Priority: 10, UpstreamIndex: 1},
			{Ordinal: 1, Number: "R-002", RegionID: "rg-api", Suffix: "api.example.test", Priority: 20, UpstreamIndex: 2},
			{Ordinal: 2, Number: "R-003", RegionID: "rg-dual-low", Suffix: "dual.example.test", Priority: 10, UpstreamIndex: 1},
			{Ordinal: 3, Number: "R-004", RegionID: "rg-dual-high", Suffix: "dual.example.test", Priority: 20, UpstreamIndex: 3},
			{Ordinal: 4, Number: "R-005", RegionID: "rg-tie-first", Suffix: "tie.example.test", Priority: 10, UpstreamIndex: 2},
			{Ordinal: 5, Number: "R-006", RegionID: "rg-tie-second", Suffix: "tie.example.test", Priority: 10, UpstreamIndex: 3},
			{Ordinal: 6, Number: "R-007", RegionID: "rg-slow", Suffix: "slow.example.test", Priority: 30, UpstreamIndex: 4},
			{Ordinal: 7, Number: "R-008", RegionID: "rg-lab", Suffix: "lab.test", Priority: 5, UpstreamIndex: 5},
			{Ordinal: 8, Number: "R-009", RegionID: "rg-chain", Suffix: "chain.lab.test", Priority: 10, UpstreamIndex: 6},
			{Ordinal: 9, Number: "R-010", RegionID: "rg-cycle", Suffix: "cycle.lab.test", Priority: 10, UpstreamIndex: 8},
			{Ordinal: 10, Number: "R-011", RegionID: "rg-longhop", Suffix: "long.lab.test", Priority: 10, UpstreamIndex: 10},
		},
	}

	p.CacheSeeds = []spec.CacheSeed{
		positiveSeed("fresh.example.test", "A", "rg-example", 100_000, ReferenceNowMS,
			record("fresh.example.test", "A", "192.0.2.11", 100_000, ReferenceNowMS),
			record("fresh.example.test", "A", "192.0.2.10", 100_000, ReferenceNowMS),
		),
		{
			CacheKey: spec.Key("blank.example.test", "A"), QName: "blank.example.test", QType: "A",
			RegionID: "rg-example", Negative: true, NegativeTTLMS: 30_000,
			NegativeExpiresAtMS: ReferenceNowMS + 30_000,
		},
		positiveSeed("advance.example.test", "A", "rg-example", 100_000, ReferenceNowMS,
			record("advance.example.test", "A", "192.0.2.20", 100_000, ReferenceNowMS),
		),
		positiveSeed("stale.example.test", "A", "rg-example", 60_000, ReferenceNowMS,
			record("stale.example.test", "A", "198.51.100.9", 60_000, ReferenceNowMS),
		),
		positiveSeed("api.example.test", "A", "rg-api", 45_000, ReferenceNowMS,
			record("api.example.test", "A", "203.0.113.20", 45_000, ReferenceNowMS),
			record("api.example.test", "TXT", txt, 45_000, ReferenceNowMS),
		),
	}

	p.AnswerSets = []spec.AnswerSet{
		answer("stale.example.test", "A", "rg-example",
			record("stale.example.test", "A", "198.51.100.10", 45_000, 0),
			record("stale.example.test", "A", "198.51.100.11", 45_000, 0),
		),
		answer("dual.example.test", "A", "rg-dual-high",
			record("dual.example.test", "A", "203.0.113.40", 20_000, 0),
		),
		answer("tie.example.test", "A", "rg-tie-first",
			record("tie.example.test", "A", "203.0.113.50", 25_000, 0),
		),
		answer("chain.lab.test", "A", "rg-chain",
			record("chain.lab.test", "A", "192.0.2.60", 30_000, 0),
		),
		answer("node.lab.test", "A", "rg-lab",
			record("node.lab.test", "A", "192.0.2.61", 50_000, 0),
			record("node.lab.test", "A", "192.0.2.62", 50_000, 0),
		),
	}

	p.Fingerprint = spec.FingerprintOf(p)
	p.Version = "det-" + p.Fingerprint[:12]
	return p
}

func record(name, qtype, value string, ttlMS, baseMS int64) spec.Record {
	r := spec.Record{Name: spec.NormalizeName(name), Type: qtype, Value: value, TTLMS: ttlMS}
	if baseMS != 0 {
		r.ExpiresAtMS = baseMS + ttlMS
	}
	return r
}

func positiveSeed(name, qtype, region string, ttlMS, baseMS int64, records ...spec.Record) spec.CacheSeed {
	return spec.CacheSeed{
		CacheKey: spec.Key(name, qtype), QName: name, QType: qtype, RegionID: region,
		Records: records,
	}
}

func answer(name, qtype, region string, records ...spec.Record) spec.AnswerSet {
	return spec.AnswerSet{CacheKey: spec.Key(name, qtype), QName: name, QType: qtype, RegionID: region, Records: records}
}

func buildQueries() []spec.Query {
	return []spec.Query{
		{Sequence: 1, ID: "Q-01-positive-fresh", Name: "fresh.example.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 2, ID: "Q-02-negative-fresh", Name: "blank.example.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 3, ID: "Q-03-ttl-advance", Name: "advance.example.test", QType: "A", AtMS: ReferenceNowMS + 20_000},
		{Sequence: 4, ID: "Q-04-expired-refresh", Name: "stale.example.test", QType: "A", AtMS: ReferenceNowMS + 120_000},
		{Sequence: 5, ID: "Q-05-api-longest", Name: "api.example.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 6, ID: "Q-06-priority", Name: "dual.example.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 7, ID: "Q-07-declared-order", Name: "tie.example.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 8, ID: "Q-08-forward-chain", Name: "chain.lab.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 9, ID: "Q-09-forward-cycle", Name: "loop.cycle.lab.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 10, ID: "Q-10-hop-limit", Name: "node.long.lab.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 11, ID: "Q-11-timeout", Name: "slow.example.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 12, ID: "Q-12-no-match", Name: "missing.other.test", QType: "A", AtMS: ReferenceNowMS},
		{Sequence: 13, ID: "Q-13-lab-terminal", Name: "node.lab.test", QType: "A", AtMS: ReferenceNowMS},
	}
}

func buildTruth(p *spec.Project, qs []spec.Query) []spec.Truth {
	truth := []spec.Truth{
		{
			Sequence: 1, QueryID: qs[0].ID, Code: "OK", Matched: true, RuleNumber: "R-001", RuleOrdinal: 0,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-001"}, UpstreamIndex: 1,
			Source: spec.SourcePositiveSeed, Records: spec.SortedRecords(p.CacheSeeds[0].Records),
			RemainingTTLMS: []int64{100_000, 100_000}, ExpiryBasis: "expires_at_ms=reference_now_ms+ttl_ms; expires_at_ms>logical_now_ms",
		},
		{
			Sequence: 2, QueryID: qs[1].ID, Code: "OK_NEGATIVE", Matched: true, RuleNumber: "R-001", RuleOrdinal: 0,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-001"}, UpstreamIndex: 1,
			Source: spec.SourceNegativeSeed, Negative: true, EntryRemainingMS: 30_000,
			ExpiryBasis: "negative_expires_at_ms>logical_now_ms",
		},
		{
			Sequence: 3, QueryID: qs[2].ID, Code: "OK", Matched: true, RuleNumber: "R-001", RuleOrdinal: 0,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-001"}, UpstreamIndex: 1,
			Source: spec.SourcePositiveSeed, Records: spec.SortedRecords(p.CacheSeeds[2].Records),
			RemainingTTLMS: []int64{80_000}, EntryRemainingMS: 80_000,
			ExpiryBasis: "remaining_ms=expires_at_ms-logical_now_ms",
		},
		{
			Sequence: 4, QueryID: qs[3].ID, Code: "OK", Matched: true, RuleNumber: "R-001", RuleOrdinal: 0,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-001"}, UpstreamIndex: 1,
			Source: spec.SourcePositiveRefresh, Records: spec.SortedRecords(p.AnswerSets[0].Records),
			RemainingTTLMS: []int64{45_000, 45_000}, HopCount: 1, VirtualElapsedMS: 1,
			ExpiryBasis: "seed expires_at_ms<=logical_now_ms; refresh ttl_ms starts at logical_now_ms",
		},
		{
			Sequence: 5, QueryID: qs[4].ID, Code: "OK", Matched: true, RuleNumber: "R-002", RuleOrdinal: 1,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-002", "R-001"}, UpstreamIndex: 2,
			Source: spec.SourcePositiveSeed, Records: spec.SortedRecords(p.CacheSeeds[4].Records),
			RemainingTTLMS: []int64{45_000, 45_000}, EntryRemainingMS: 45_000,
		},
		{
			Sequence: 6, QueryID: qs[5].ID, Code: "OK", Matched: true, RuleNumber: "R-004", RuleOrdinal: 3,
			DecisionBasis: "priority_desc", CandidateRules: []string{"R-004", "R-003", "R-001"}, UpstreamIndex: 3,
			Source: spec.SourceUpstream, Records: spec.SortedRecords(p.AnswerSets[1].Records),
			RemainingTTLMS: []int64{20_000}, HopCount: 1, VirtualElapsedMS: 1,
		},
		{
			Sequence: 7, QueryID: qs[6].ID, Code: "OK", Matched: true, RuleNumber: "R-005", RuleOrdinal: 4,
			DecisionBasis: "declared_order_asc", CandidateRules: []string{"R-005", "R-006", "R-001"}, UpstreamIndex: 2,
			Source: spec.SourceUpstream, Records: spec.SortedRecords(p.AnswerSets[2].Records),
			RemainingTTLMS: []int64{25_000}, HopCount: 1, VirtualElapsedMS: 2,
		},
		{
			Sequence: 8, QueryID: qs[7].ID, Code: "OK", Matched: true, RuleNumber: "R-009", RuleOrdinal: 8,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-009", "R-008"}, UpstreamIndex: 6,
			Source: spec.SourceUpstream, Records: spec.SortedRecords(p.AnswerSets[3].Records),
			RemainingTTLMS: []int64{30_000}, HopCount: 3, VirtualElapsedMS: 6,
		},
		{
			Sequence: 9, QueryID: qs[8].ID, Code: "E_UPSTREAM_FORWARD_CYCLE", Matched: true, RuleNumber: "R-010", RuleOrdinal: 9,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-010", "R-008"}, UpstreamIndex: 8,
			Source: spec.SourceError, HopCount: 2, VirtualElapsedMS: 2,
		},
		{
			Sequence: 10, QueryID: qs[9].ID, Code: "E_UPSTREAM_HOPS_EXCEEDED", Matched: true, RuleNumber: "R-011", RuleOrdinal: 10,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-011", "R-008"}, UpstreamIndex: 10,
			Source: spec.SourceError, HopCount: 4, VirtualElapsedMS: 4,
		},
		{
			Sequence: 11, QueryID: qs[10].ID, Code: "E_UPSTREAM_TIMEOUT", Matched: true, RuleNumber: "R-007", RuleOrdinal: 6,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-007", "R-001"}, UpstreamIndex: 4,
			Source: spec.SourceError, HopCount: 1, VirtualElapsedMS: 2500,
		},
		{
			Sequence: 12, QueryID: qs[11].ID, Code: "E_ROUTE_NO_MATCH", Matched: false,
			Source: spec.SourceError,
		},
		{
			Sequence: 13, QueryID: qs[12].ID, Code: "OK", Matched: true, RuleNumber: "R-008", RuleOrdinal: 7,
			DecisionBasis: "longest_suffix", CandidateRules: []string{"R-008"}, UpstreamIndex: 5,
			Source: spec.SourceUpstream, Records: spec.SortedRecords(p.AnswerSets[4].Records),
			RemainingTTLMS: []int64{50_000, 50_000}, HopCount: 1, VirtualElapsedMS: 2,
		},
	}
	for i := range truth {
		truth[i].Sequence = i + 1
	}
	return truth
}

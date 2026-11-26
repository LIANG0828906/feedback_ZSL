// Package spec contains the canonical domain model shared by loaders, the
// deterministic generator, resolver engine, checkpoint store, and workbench.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const (
	SchemaV1 = "dns-project/v1-flat-literal-seconds"
	SchemaV2 = "dns-project/v2-flat-list-millis"
	SchemaV3 = "dns-project/v3-tree-absolute-negative"

	SourcePositiveSeed    = "positive_cache_hit"
	SourceNegativeSeed    = "negative_cache_hit"
	SourcePositiveRefresh = "positive_cache_expired_refresh"
	SourceUpstream        = "upstream_response"
	SourceError           = "error"
)

// Project is the schema-independent parsed engineering project.
type Project struct {
	ProjectID            string      `json:"project_id"`
	Seed                 int64       `json:"seed"`
	Version              string      `json:"version"`
	Fingerprint          string      `json:"fingerprint"`
	ReferenceNowMS       int64       `json:"reference_now_ms"`
	DefaultTimeoutMS     int64       `json:"default_timeout_ms"`
	WorkerBudget         int         `json:"worker_budget"`
	MaxHops              int         `json:"max_hops"`
	StrictOverlap        bool        `json:"strict_overlap"`
	NegativeCacheEnabled bool        `json:"negative_cache_enabled"`
	NegativeDefaultTTLMS int64       `json:"negative_default_ttl_ms"`
	Regions              []Region    `json:"regions"`
	Upstreams            []Upstream  `json:"upstreams"`
	Rules                []Rule      `json:"rules"`
	CacheSeeds           []CacheSeed `json:"cache_seeds"`
	AnswerSets           []AnswerSet `json:"answer_sets"`
}

type Region struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id,omitempty"`
}

type Upstream struct {
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Literal   string `json:"literal"`
	TimeoutMS int64  `json:"timeout_ms"`
	LatencyMS int64  `json:"latency_ms"`
	ForwardTo int    `json:"forward_to,omitempty"`
}

type Rule struct {
	Ordinal       int    `json:"ordinal"`
	Number        string `json:"number"`
	RegionID      string `json:"region_id"`
	Suffix        string `json:"suffix"`
	Priority      int    `json:"priority"`
	UpstreamIndex int    `json:"upstream_index"`
}

type Record struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Value       string `json:"value"`
	TTLMS       int64  `json:"ttl_ms"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

type CacheSeed struct {
	CacheKey            string   `json:"cache_key"`
	QName               string   `json:"qname"`
	QType               string   `json:"qtype"`
	RegionID            string   `json:"region_id"`
	Negative            bool     `json:"negative"`
	NegativeTTLMS       int64    `json:"negative_ttl_ms,omitempty"`
	NegativeExpiresAtMS int64    `json:"negative_expires_at_ms,omitempty"`
	Records             []Record `json:"records,omitempty"`
}

type AnswerSet struct {
	CacheKey string   `json:"cache_key"`
	QName    string   `json:"qname"`
	QType    string   `json:"qtype"`
	RegionID string   `json:"region_id"`
	Records  []Record `json:"records"`
}

type Query struct {
	Sequence int    `json:"sequence"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	QType    string `json:"qtype"`
	AtMS     int64  `json:"at_ms"`
}

// Truth is generated while constructing the deterministic engineering sample.
// It is the only expected-result oracle used by acceptance tests.
type Truth struct {
	Sequence         int      `json:"sequence"`
	QueryID          string   `json:"query_id"`
	Code             string   `json:"code"`
	Matched          bool     `json:"matched"`
	RuleNumber       string   `json:"rule_number,omitempty"`
	RuleOrdinal      int      `json:"rule_ordinal,omitempty"`
	DecisionBasis    string   `json:"decision_basis,omitempty"`
	CandidateRules   []string `json:"candidate_rules,omitempty"`
	UpstreamIndex    int      `json:"upstream_index,omitempty"`
	HopCount         int      `json:"hop_count,omitempty"`
	Source           string   `json:"source,omitempty"`
	Negative         bool     `json:"negative,omitempty"`
	Records          []Record `json:"records,omitempty"`
	RemainingTTLMS   []int64  `json:"remaining_ttl_ms,omitempty"`
	EntryRemainingMS int64    `json:"entry_remaining_ms,omitempty"`
	ExpiryBasis      string   `json:"expiry_basis,omitempty"`
	VirtualElapsedMS int64    `json:"virtual_elapsed_ms,omitempty"`
}

type Bundle struct {
	Project *Project          `json:"project"`
	Queries []Query           `json:"queries"`
	Truth   []Truth           `json:"truth"`
	Docs    map[string][]byte `json:"docs,omitempty"`
}

// Key builds the canonical cache key.
func Key(qname, qtype string) string {
	return strings.ToUpper(qtype) + ":" + NormalizeName(qname)
}

// NormalizeName lower-cases and fully qualifies a DNS name.
func NormalizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "." {
		return "."
	}
	return strings.TrimSuffix(name, ".") + "."
}

// MatchesSuffix reports whether qname equals or is a child of suffix.
func MatchesSuffix(qname, suffix string) bool {
	q := NormalizeName(qname)
	s := NormalizeName(suffix)
	if s == "." {
		return q != ""
	}
	return q == s || strings.HasSuffix(q, "."+s)
}

// LabelCount counts DNS labels for longest-suffix routing.
func LabelCount(name string) int {
	n := NormalizeName(name)
	if n == "." {
		return 0
	}
	return strings.Count(n, ".")
}

// SortedRecords returns a deterministic copy sorted by the complete record set.
func SortedRecords(records []Record) []Record {
	out := append([]Record(nil), records...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		keys := [][3]string{
			{a.Name, a.Type, a.Value},
			{b.Name, b.Type, b.Value},
		}
		for k := range keys[0] {
			if keys[0][k] != keys[1][k] {
				return keys[0][k] < keys[1][k]
			}
		}
		if a.TTLMS != b.TTLMS {
			return a.TTLMS < b.TTLMS
		}
		return a.ExpiresAtMS < b.ExpiresAtMS
	})
	return out
}

// CanonicalJSON returns stable JSON for fingerprints and comparisons.
func (p *Project) CanonicalJSON() ([]byte, error) {
	cp := p.Clone()
	cp.Fingerprint = ""
	cp.Version = p.Version
	return json.Marshal(cp)
}

// FingerprintOf computes a content fingerprint excluding the fingerprint field.
func FingerprintOf(p *Project) string {
	cp := p.Clone()
	cp.Fingerprint = ""
	cp.Version = ""
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out Project
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

func (t *Truth) Clone() Truth {
	b, _ := json.Marshal(t)
	var out Truth
	_ = json.Unmarshal(b, &out)
	return out
}

package sim

import (
	"fmt"
	"strings"
)

// SampleConfig returns a deterministic three-site topology with one forwarding
// branch, a polluted cache, a static hosts mapping, a negative cache, and
// domain-specific fault injections.
func SampleConfig() *Config {
	cfg := &Config{
		Version: 1,
		Seed:    264264,
		Limits: Limits{
			MaxConcurrentQueries: 16,
			MaxConcurrentTraces:  4,
			TraceQueueSize:       8,
			DefaultTimeoutMS:     400,
			TraceQueueTimeoutMS:  1500,
			DefaultRetries:       0,
		},
		Resolvers: []Resolver{
			{
				ID: "root", Role: RoleRoot, Address: "127.0.0.1:15351",
				LatencyMS: 2, DelegationTTL: 300,
				Delegations: []Delegation{{Zone: "test.", Target: "tld"}},
			},
			{
				ID: "tld", Role: RoleTLD, Address: "127.0.0.1:15352",
				LatencyMS: 2, TimeoutMS: 100, DelegationTTL: 120,
				Delegations: []Delegation{{Zone: "example.test.", Target: "auth"}},
			},
			{
				ID: "auth", Role: RoleAuthority, Address: "127.0.0.1:15353",
				LatencyMS: 2, TimeoutMS: 100, UDPMaxBytes: 512,
			},
			{
				ID: "rec-shanghai", Role: RoleRecursive, Address: "127.0.0.1:15354",
				Upstreams: []string{"root"}, LatencyMS: 1,
			},
			{
				ID: "rec-hangzhou", Role: RoleRecursive, Address: "127.0.0.1:15355",
				Upstreams: []string{"root"}, LatencyMS: 1,
				Cache: &CacheConfig{Preload: []PreloadEntry{{
					Name: "www.example.test.", Type: "A", RCode: "NOERROR", TTL: 45,
					Answers: []PreloadAnswer{{
						Name: "www.example.test.", Type: "A", TTL: 45, Value: "10.20.0.99",
					}},
				}}},
			},
			{
				ID: "fwd-shenzhen", Role: RoleForwarder, Address: "127.0.0.1:15356",
				Upstreams: []string{"root"}, LatencyMS: 1,
			},
			{
				ID: "rec-shenzhen", Role: RoleRecursive, Address: "127.0.0.1:15357",
				Upstreams: []string{"fwd-shenzhen"}, LatencyMS: 1,
			},
			{
				ID: "rec-wuhan", Role: RoleRecursive, Address: "127.0.0.1:15358",
				Upstreams: []string{"root"}, LatencyMS: 1, Retries: intPtr(1),
			},
		},
		Zones: []Zone{{
			Name: "example.test.", Authority: "auth",
			Records: []Record{
				{Name: "www.example.test.", Type: "CNAME", TTL: 30, Value: "edge.example.test."},
				{Name: "edge.example.test.", Type: "A", TTL: 30, Value: "10.20.0.10"},
				{Name: "api.example.test.", Type: "A", TTL: 120, Value: "10.20.0.11"},
				{Name: "broken.example.test.", Type: "A", TTL: 60, Value: "10.20.0.12"},
				{Name: "servfail.example.test.", Type: "A", TTL: 60, Value: "10.20.0.13"},
				{Name: "wrong.example.test.", Type: "A", TTL: 60, Value: "10.20.0.14"},
				{Name: "short.example.test.", Type: "A", TTL: 1, Value: "10.20.0.15"},
				{Name: "empty.example.test.", Type: "TXT", TTL: 60, Value: "only-txt"},
				{Name: "big.example.test.", Type: "TXT", TTL: 60, Value: strings.Repeat("x", 900)},
			},
		}},
		Sites: []Site{
			{ID: "shanghai", Recursive: "rec-shanghai"},
			{
				ID: "hangzhou", Recursive: "rec-hangzhou",
				StaticMappings: map[string]string{},
			},
			{ID: "shenzhen", Recursive: "rec-shenzhen"},
			{
				ID: "nanjing", Recursive: "rec-shanghai",
				StaticMappings: map[string]string{"www.example.test.": "10.20.0.77"},
			},
			{
				ID: "beijing", Recursive: "rec-shanghai",
				NegativeCache: []NegativeCacheEntry{{
					Name: "api.example.test.", Type: "A", RCode: "NXDOMAIN", TTL: 90,
				}},
			},
			{ID: "wuhan", Recursive: "rec-wuhan"},
		},
		Faults: []Fault{
			{
				ID: "tld-broken-first-attempt", Resolver: "tld", Domain: "broken.example.test.",
				Type: "timeout", Attempts: []int{1},
			},
			{
				ID: "tld-servfail", Resolver: "tld", Domain: "servfail.example.test.",
				Type: "servfail",
			},
			{
				ID: "root-wrong-delegation", Resolver: "root", Domain: "wrong.example.test.",
				Type: "wrong_delegation", WrongAddress: "127.0.0.1:15999",
			},
		},
	}
	if err := PrepareConfig(cfg); err != nil {
		panic(fmt.Sprintf("invalid built-in sample: %v", err))
	}
	return cfg
}

func intPtr(v int) *int { return &v }

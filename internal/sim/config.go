package sim

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"dnssim/internal/dnswire"
)

const (
	RoleRoot      = "root"
	RoleTLD       = "tld"
	RoleAuthority = "authority"
	RoleRecursive = "recursive"
	RoleForwarder = "forwarder"
)

type Limits struct {
	MaxConcurrentQueries int `json:"max_concurrent_queries"`
	MaxConcurrentTraces  int `json:"max_concurrent_traces"`
	TraceQueueSize       int `json:"trace_queue_size"`
	DefaultTimeoutMS     int `json:"default_timeout_ms"`
	TraceQueueTimeoutMS  int `json:"trace_queue_timeout_ms"`
	DefaultRetries       int `json:"default_retries"`
}

type Delegation struct {
	Zone   string `json:"zone"`
	Target string `json:"target"`
	TTL    uint32 `json:"ttl,omitempty"`
}

type Record struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   uint32 `json:"ttl"`
	Value string `json:"value"`
}

type Zone struct {
	Name      string   `json:"name"`
	Authority string   `json:"authority"`
	Records   []Record `json:"records"`
}

type PreloadAnswer struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   uint32 `json:"ttl,omitempty"`
	Value string `json:"value"`
}

type PreloadEntry struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	RCode   string          `json:"rcode,omitempty"`
	TTL     uint32          `json:"ttl"`
	Answers []PreloadAnswer `json:"answers,omitempty"`
}

type CacheConfig struct {
	Preload []PreloadEntry `json:"preload,omitempty"`
}

type Fault struct {
	ID           string `json:"id,omitempty"`
	Resolver     string `json:"resolver,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Type         string `json:"type"`
	Transport    string `json:"transport,omitempty"`
	Attempts     []int  `json:"attempts,omitempty"`
	DelayMS      int    `json:"delay_ms,omitempty"`
	WrongTarget  string `json:"wrong_target,omitempty"`
	WrongAddress string `json:"wrong_address,omitempty"`
}

type Resolver struct {
	ID            string       `json:"id"`
	Role          string       `json:"role"`
	Address       string       `json:"address"`
	Upstreams     []string     `json:"upstreams,omitempty"`
	Delegations   []Delegation `json:"delegations,omitempty"`
	LatencyMS     int          `json:"latency_ms,omitempty"`
	DropRate      float64      `json:"drop_rate,omitempty"`
	TimeoutMS     int          `json:"timeout_ms,omitempty"`
	Retries       *int         `json:"retries,omitempty"`
	TruncateUDP   bool         `json:"truncate_udp,omitempty"`
	UDPMaxBytes   int          `json:"udp_max_bytes,omitempty"`
	Cache         *CacheConfig `json:"cache,omitempty"`
	Faults        []Fault      `json:"faults,omitempty"`
	DelegationTTL uint32       `json:"delegation_ttl,omitempty"`
}

type StaticMapping struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type NegativeCacheEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	RCode string `json:"rcode"`
	TTL   uint32 `json:"ttl"`
}

type Site struct {
	ID             string               `json:"id"`
	Recursive      string               `json:"recursive"`
	Retries        *int                 `json:"retries,omitempty"`
	StaticMappings map[string]string    `json:"static_mappings,omitempty"`
	NegativeCache  []NegativeCacheEntry `json:"negative_cache,omitempty"`
}

type Config struct {
	Version   int        `json:"version"`
	Seed      int64      `json:"seed,omitempty"`
	Limits    Limits     `json:"limits"`
	Zones     []Zone     `json:"zones"`
	Resolvers []Resolver `json:"resolvers"`
	Sites     []Site     `json:"sites"`
	Faults    []Fault    `json:"faults,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, bizError(CodeConfigInvalidTopology, "config", "cannot read config file")
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, bizError(CodeConfigInvalidTopology, "config", "invalid JSON: %v", err)
	}
	if err := PrepareConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PrepareConfig applies deterministic defaults, canonicalizes names and
// validates the complete topology. It never opens a socket.
func PrepareConfig(cfg *Config) error {
	if cfg == nil {
		return bizError(CodeConfigInvalidTopology, "config", "config is nil")
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Version != 1 {
		return bizError(CodeConfigInvalidTopology, "version", "unsupported config version %d", cfg.Version)
	}
	applyLimitDefaults(&cfg.Limits)
	if err := validateLimits(cfg.Limits); err != nil {
		return err
	}
	if len(cfg.Resolvers) == 0 {
		return bizError(CodeConfigInvalidTopology, "resolvers", "at least one resolver is required")
	}
	if len(cfg.Zones) == 0 {
		return bizError(CodeConfigInvalidTopology, "zones", "at least one authoritative zone is required")
	}
	if len(cfg.Sites) == 0 {
		return bizError(CodeConfigInvalidTopology, "sites", "at least one site is required")
	}

	resolvers := make(map[string]*Resolver, len(cfg.Resolvers))
	for i := range cfg.Resolvers {
		r := &cfg.Resolvers[i]
		r.ID = strings.TrimSpace(r.ID)
		if r.ID == "" {
			return bizError(CodeConfigInvalidTopology, fmt.Sprintf("resolvers[%d].id", i), "resolver id is required")
		}
		if _, exists := resolvers[r.ID]; exists {
			return bizError(CodeConfigDuplicateID, fmt.Sprintf("resolvers[%d].id", i), "duplicate resolver id %q", r.ID)
		}
		r.Role = strings.ToLower(strings.TrimSpace(r.Role))
		switch r.Role {
		case RoleRoot, RoleTLD, RoleAuthority, RoleRecursive, RoleForwarder:
		default:
			return bizError(CodeConfigInvalidRole, fmt.Sprintf("resolvers[%d].role", i), "unknown resolver role %q", r.Role)
		}
		if r.LatencyMS < 0 {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("resolvers[%d].latency_ms", i), "latency must be non-negative")
		}
		if r.DropRate < 0 || r.DropRate > 1 {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("resolvers[%d].drop_rate", i), "drop_rate must be between 0 and 1")
		}
		if r.TimeoutMS < 0 {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("resolvers[%d].timeout_ms", i), "timeout must be non-negative")
		}
		if r.UDPMaxBytes < 0 || (r.UDPMaxBytes > 0 && r.UDPMaxBytes < 512) {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("resolvers[%d].udp_max_bytes", i), "udp_max_bytes must be 0 or at least 512")
		}
		if r.Retries != nil && *r.Retries < 0 {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("resolvers[%d].retries", i), "retries must be non-negative")
		}
		if r.DelegationTTL == 0 {
			r.DelegationTTL = 300
		}
		if err := validateAddress(r.Address); err != nil {
			return bizError(CodeConfigInvalidAddress, fmt.Sprintf("resolvers[%d].address", i), "%v", err)
		}
		for j := range r.Delegations {
			d := &r.Delegations[j]
			zone, err := dnswire.CanonicalName(d.Zone)
			if err != nil {
				return bizError(CodeConfigInvalidName, fmt.Sprintf("resolvers[%d].delegations[%d].zone", i, j), "%v", err)
			}
			d.Zone = zone
			if d.Target == "" {
				return bizError(CodeConfigInvalidValue, fmt.Sprintf("resolvers[%d].delegations[%d].target", i, j), "delegation target is required")
			}
			if d.TTL == 0 {
				d.TTL = r.DelegationTTL
			}
		}
		resolvers[r.ID] = r
	}

	for i := range cfg.Resolvers {
		r := &cfg.Resolvers[i]
		switch r.Role {
		case RoleRoot, RoleTLD, RoleAuthority:
			if len(r.Upstreams) != 0 {
				return bizError(CodeConfigInvalidRole, fmt.Sprintf("resolvers[%d].upstreams", i), "%s resolver cannot have upstreams", r.Role)
			}
		case RoleRecursive, RoleForwarder:
			if len(r.Upstreams) == 0 {
				return bizError(CodeConfigInvalidTopology, fmt.Sprintf("resolvers[%d].upstreams", i), "%s resolver requires at least one upstream", r.Role)
			}
		}
		for j, upstream := range r.Upstreams {
			if upstream == r.ID {
				return bizError(CodeConfigForwardCycle, fmt.Sprintf("resolvers[%d].upstreams[%d]", i, j), "resolver forwards to itself")
			}
			if _, ok := resolvers[upstream]; !ok {
				return bizError(CodeConfigMissingRef, fmt.Sprintf("resolvers[%d].upstreams[%d]", i, j), "unknown resolver %q", upstream)
			}
		}
		for j, d := range r.Delegations {
			target, ok := resolvers[d.Target]
			if !ok {
				return bizError(CodeConfigMissingRef, fmt.Sprintf("resolvers[%d].delegations[%d].target", i, j), "unknown resolver %q", d.Target)
			}
			if d.Target == r.ID {
				return bizError(CodeConfigDelegationCycle, fmt.Sprintf("resolvers[%d].delegations[%d].target", i, j), "resolver delegates to itself")
			}
			if target.Role != RoleTLD && target.Role != RoleAuthority {
				return bizError(CodeConfigInvalidRole, fmt.Sprintf("resolvers[%d].delegations[%d].target", i, j), "delegation target %q must be tld or authority", target.ID)
			}
		}
		if r.Cache != nil {
			for j := range r.Cache.Preload {
				if err := normalizePreload(&r.Cache.Preload[j], fmt.Sprintf("resolvers[%d].cache.preload[%d]", i, j)); err != nil {
					return err
				}
			}
		}
	}
	if err := detectForwardCycles(cfg.Resolvers, resolvers); err != nil {
		return err
	}
	if err := detectDelegationCycles(cfg.Resolvers, resolvers); err != nil {
		return err
	}

	zoneNames := make(map[string]string, len(cfg.Zones))
	for i := range cfg.Zones {
		z := &cfg.Zones[i]
		name, err := dnswire.CanonicalName(z.Name)
		if err != nil {
			return bizError(CodeConfigInvalidName, fmt.Sprintf("zones[%d].name", i), "%v", err)
		}
		z.Name = name
		if _, duplicate := zoneNames[name]; duplicate {
			return bizError(CodeConfigDuplicateID, fmt.Sprintf("zones[%d].name", i), "duplicate zone %q", name)
		}
		zoneNames[name] = z.Authority
		auth, ok := resolvers[z.Authority]
		if !ok {
			return bizError(CodeConfigMissingRef, fmt.Sprintf("zones[%d].authority", i), "unknown authority resolver %q", z.Authority)
		}
		if auth.Role != RoleAuthority {
			return bizError(CodeConfigInvalidRole, fmt.Sprintf("zones[%d].authority", i), "%q is not an authority resolver", z.Authority)
		}
		for j := range z.Records {
			if err := normalizeRecord(&z.Records[j], name, fmt.Sprintf("zones[%d].records[%d]", i, j)); err != nil {
				return err
			}
		}
		if err := detectCNAMECycles(z.Records); err != nil {
			return bizError(CodeConfigCNAMECycle, fmt.Sprintf("zones[%d]", i), "%v", err)
		}
	}

	for i := range cfg.Faults {
		if err := normalizeFault(&cfg.Faults[i], fmt.Sprintf("faults[%d]", i), "", resolvers); err != nil {
			return err
		}
	}
	for i := range cfg.Resolvers {
		r := &cfg.Resolvers[i]
		for j := range r.Faults {
			if r.Faults[j].Resolver != "" && r.Faults[j].Resolver != r.ID {
				return bizError(CodeConfigInvalidFault, fmt.Sprintf("resolvers[%d].faults[%d].resolver", i, j), "embedded fault resolver must match %q", r.ID)
			}
			r.Faults[j].Resolver = r.ID
			if err := normalizeFault(&r.Faults[j], fmt.Sprintf("resolvers[%d].faults[%d]", i, j), r.ID, resolvers); err != nil {
				return err
			}
		}
	}

	siteIDs := make(map[string]struct{}, len(cfg.Sites))
	for i := range cfg.Sites {
		s := &cfg.Sites[i]
		s.ID = strings.TrimSpace(s.ID)
		if s.ID == "" {
			return bizError(CodeConfigInvalidTopology, fmt.Sprintf("sites[%d].id", i), "site id is required")
		}
		if _, duplicate := siteIDs[s.ID]; duplicate {
			return bizError(CodeConfigDuplicateID, fmt.Sprintf("sites[%d].id", i), "duplicate site id %q", s.ID)
		}
		siteIDs[s.ID] = struct{}{}
		rec, ok := resolvers[s.Recursive]
		if !ok {
			return bizError(CodeConfigMissingRef, fmt.Sprintf("sites[%d].recursive", i), "unknown resolver %q", s.Recursive)
		}
		if rec.Role != RoleRecursive && rec.Role != RoleForwarder {
			return bizError(CodeConfigInvalidRole, fmt.Sprintf("sites[%d].recursive", i), "%q is not a recursive or forwarder resolver", s.Recursive)
		}
		if s.Retries != nil && *s.Retries < 0 {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("sites[%d].retries", i), "retries must be non-negative")
		}
		if s.StaticMappings == nil {
			s.StaticMappings = map[string]string{}
		}
		normalizedMappings := make(map[string]string, len(s.StaticMappings))
		staticNames := make([]string, 0, len(s.StaticMappings))
		for name := range s.StaticMappings {
			staticNames = append(staticNames, name)
		}
		sort.Strings(staticNames)
		for _, name := range staticNames {
			value := s.StaticMappings[name]
			canonical, err := dnswire.CanonicalName(name)
			if err != nil {
				return bizError(CodeConfigInvalidName, fmt.Sprintf("sites[%d].static_mappings", i), "%v", err)
			}
			if net.ParseIP(value) == nil {
				return bizError(CodeConfigInvalidValue, fmt.Sprintf("sites[%d].static_mappings[%s]", i, name), "static mapping value must be an IP address")
			}
			normalizedMappings[canonical] = strings.TrimSpace(value)
		}
		s.StaticMappings = normalizedMappings
		for j := range s.NegativeCache {
			e := &s.NegativeCache[j]
			name, err := dnswire.CanonicalName(e.Name)
			if err != nil {
				return bizError(CodeConfigInvalidName, fmt.Sprintf("sites[%d].negative_cache[%d].name", i, j), "%v", err)
			}
			e.Name = name
			t, err := dnswire.ParseType(e.Type)
			if err != nil || t == dnswire.TypeANY {
				return bizError(CodeConfigInvalidType, fmt.Sprintf("sites[%d].negative_cache[%d].type", i, j), "invalid negative cache type %q", e.Type)
			}
			e.Type = t.String()
			e.RCode = strings.ToUpper(strings.TrimSpace(e.RCode))
			if e.RCode != "NXDOMAIN" && e.RCode != "NODATA" {
				return bizError(CodeConfigInvalidValue, fmt.Sprintf("sites[%d].negative_cache[%d].rcode", i, j), "negative cache rcode must be NXDOMAIN or NODATA")
			}
			if e.TTL == 0 {
				return bizError(CodeConfigInvalidValue, fmt.Sprintf("sites[%d].negative_cache[%d].ttl", i, j), "negative cache ttl must be positive")
			}
		}
	}

	if err := validateReachability(cfg, resolvers); err != nil {
		return err
	}
	return nil
}

func applyLimitDefaults(l *Limits) {
	if l.MaxConcurrentQueries == 0 {
		l.MaxConcurrentQueries = 32
	}
	if l.MaxConcurrentTraces == 0 {
		l.MaxConcurrentTraces = 8
	}
	if l.TraceQueueSize == 0 {
		l.TraceQueueSize = 32
	}
	if l.DefaultTimeoutMS == 0 {
		l.DefaultTimeoutMS = 750
	}
	if l.TraceQueueTimeoutMS == 0 {
		l.TraceQueueTimeoutMS = 2000
	}
	if l.DefaultRetries < 0 {
		l.DefaultRetries = 0
	}
}

func validateLimits(l Limits) error {
	if l.MaxConcurrentQueries <= 0 {
		return bizError(CodeConfigInvalidLimit, "limits.max_concurrent_queries", "must be positive")
	}
	if l.MaxConcurrentTraces <= 0 {
		return bizError(CodeConfigInvalidLimit, "limits.max_concurrent_traces", "must be positive")
	}
	if l.TraceQueueSize < 0 {
		return bizError(CodeConfigInvalidLimit, "limits.trace_queue_size", "must be non-negative")
	}
	if l.DefaultTimeoutMS <= 0 {
		return bizError(CodeConfigInvalidLimit, "limits.default_timeout_ms", "must be positive")
	}
	if l.TraceQueueTimeoutMS <= 0 {
		return bizError(CodeConfigInvalidLimit, "limits.trace_queue_timeout_ms", "must be positive")
	}
	if l.DefaultRetries < 0 {
		return bizError(CodeConfigInvalidLimit, "limits.default_retries", "must be non-negative")
	}
	return nil
}

func validateAddress(address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return fmt.Errorf("address is required")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("address %q must be host:port", address)
	}
	if portText == "" {
		return fmt.Errorf("port is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("port %q is invalid", portText)
	}
	if host == "" {
		return fmt.Errorf("host is required")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("host %q is not a loopback address", host)
	}
	return nil
}

func normalizePreload(entry *PreloadEntry, field string) error {
	name, err := dnswire.CanonicalName(entry.Name)
	if err != nil {
		return bizError(CodeConfigInvalidName, field+".name", "%v", err)
	}
	entry.Name = name
	t, err := dnswire.ParseType(entry.Type)
	if err != nil || t == dnswire.TypeANY || t == dnswire.TypeOPT {
		return bizError(CodeConfigInvalidType, field+".type", "invalid cache type %q", entry.Type)
	}
	entry.Type = t.String()
	entry.RCode = strings.ToUpper(strings.TrimSpace(entry.RCode))
	if entry.RCode == "" {
		entry.RCode = "NOERROR"
	}
	switch entry.RCode {
	case "NOERROR", "NXDOMAIN", "NODATA", "SERVFAIL":
	default:
		return bizError(CodeConfigInvalidValue, field+".rcode", "unsupported cache rcode %q", entry.RCode)
	}
	if entry.TTL == 0 {
		return bizError(CodeConfigInvalidValue, field+".ttl", "cache ttl must be positive")
	}
	for i := range entry.Answers {
		a := &entry.Answers[i]
		n, err := dnswire.CanonicalName(a.Name)
		if err != nil {
			return bizError(CodeConfigInvalidName, fmt.Sprintf("%s.answers[%d].name", field, i), "%v", err)
		}
		a.Name = n
		at, err := dnswire.ParseType(a.Type)
		if err != nil || at == dnswire.TypeANY || at == dnswire.TypeOPT {
			return bizError(CodeConfigInvalidType, fmt.Sprintf("%s.answers[%d].type", field, i), "invalid answer type %q", a.Type)
		}
		a.Type = at.String()
		if a.TTL == 0 {
			a.TTL = entry.TTL
		}
		if err := validateRecordValue(a.Type, a.Value); err != nil {
			return bizError(CodeConfigInvalidValue, fmt.Sprintf("%s.answers[%d].value", field, i), "%v", err)
		}
		if a.Type == "CNAME" || a.Type == "NS" {
			target, err := dnswire.CanonicalName(a.Value)
			if err != nil {
				return bizError(CodeConfigInvalidName, fmt.Sprintf("%s.answers[%d].value", field, i), "%v", err)
			}
			a.Value = target
		}
	}
	return nil
}

func normalizeRecord(rec *Record, zone, field string) error {
	name, err := dnswire.CanonicalName(rec.Name)
	if err != nil {
		return bizError(CodeConfigInvalidName, field+".name", "%v", err)
	}
	if !dnswire.IsSubdomain(name, zone) {
		return bizError(CodeConfigInvalidTopology, field+".name", "record %q is outside zone %q", name, zone)
	}
	rec.Name = name
	t, err := dnswire.ParseType(rec.Type)
	if err != nil || t == dnswire.TypeANY || t == dnswire.TypeOPT || t == dnswire.TypeSOA {
		return bizError(CodeConfigInvalidType, field+".type", "unsupported record type %q", rec.Type)
	}
	rec.Type = t.String()
	if err := validateRecordValue(rec.Type, rec.Value); err != nil {
		return bizError(CodeConfigInvalidValue, field+".value", "%v", err)
	}
	if rec.Type == "CNAME" || rec.Type == "NS" {
		target, err := dnswire.CanonicalName(rec.Value)
		if err != nil {
			return bizError(CodeConfigInvalidName, field+".value", "%v", err)
		}
		rec.Value = target
	}
	return nil
}

func validateRecordValue(recordType, value string) error {
	switch recordType {
	case "A":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("A value %q is not an IPv4 address", value)
		}
	case "AAAA":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() != nil {
			return fmt.Errorf("AAAA value %q is not an IPv6 address", value)
		}
	case "CNAME", "NS":
		if _, err := dnswire.CanonicalName(value); err != nil {
			return err
		}
	case "TXT":
		if value == "" {
			return fmt.Errorf("TXT value is required")
		}
		if len(value) > 4096 {
			return fmt.Errorf("TXT value exceeds 4096 bytes")
		}
	default:
		return fmt.Errorf("unsupported record type %q", recordType)
	}
	return nil
}

func normalizeFault(f *Fault, field, resolver string, resolvers map[string]*Resolver) error {
	if f.Resolver == "" {
		f.Resolver = resolver
	}
	if f.Resolver == "" {
		return bizError(CodeConfigInvalidFault, field+".resolver", "fault resolver is required")
	}
	if _, ok := resolvers[f.Resolver]; !ok {
		return bizError(CodeConfigMissingRef, field+".resolver", "unknown resolver %q", f.Resolver)
	}
	if f.Domain != "" {
		if strings.HasPrefix(f.Domain, "*.") {
			name, err := dnswire.CanonicalName(strings.TrimPrefix(f.Domain, "*."))
			if err != nil {
				return bizError(CodeConfigInvalidName, field+".domain", "%v", err)
			}
			f.Domain = "*." + name
		} else {
			name, err := dnswire.CanonicalName(f.Domain)
			if err != nil {
				return bizError(CodeConfigInvalidName, field+".domain", "%v", err)
			}
			f.Domain = name
		}
	}
	f.Type = strings.ToLower(strings.TrimSpace(f.Type))
	switch f.Type {
	case "timeout", "servfail", "wrong_delegation":
	default:
		return bizError(CodeConfigInvalidFault, field+".type", "unsupported fault type %q", f.Type)
	}
	f.Transport = strings.ToLower(strings.TrimSpace(f.Transport))
	switch f.Transport {
	case "", "udp", "tcp":
	default:
		return bizError(CodeConfigInvalidFault, field+".transport", "transport must be udp or tcp")
	}
	if f.DelayMS < 0 {
		return bizError(CodeConfigInvalidFault, field+".delay_ms", "delay must be non-negative")
	}
	seen := map[int]struct{}{}
	for _, attempt := range f.Attempts {
		if attempt <= 0 {
			return bizError(CodeConfigInvalidFault, field+".attempts", "attempt numbers are one-based")
		}
		if _, duplicate := seen[attempt]; duplicate {
			return bizError(CodeConfigInvalidFault, field+".attempts", "duplicate attempt %d", attempt)
		}
		seen[attempt] = struct{}{}
	}
	if f.Type == "wrong_delegation" {
		if f.WrongTarget == "" && f.WrongAddress == "" {
			return bizError(CodeConfigInvalidFault, field, "wrong_delegation requires wrong_target or wrong_address")
		}
		if f.WrongTarget != "" {
			if _, ok := resolvers[f.WrongTarget]; !ok {
				return bizError(CodeConfigMissingRef, field+".wrong_target", "unknown resolver %q", f.WrongTarget)
			}
		}
		if f.WrongAddress != "" {
			if err := validateAddress(f.WrongAddress); err != nil {
				return bizError(CodeConfigInvalidAddress, field+".wrong_address", "%v", err)
			}
		}
	}
	if f.ID == "" {
		f.ID = fmt.Sprintf("%s/%s/%d", f.Resolver, f.Type, faultIndexHash(f))
	}
	return nil
}

func faultIndexHash(f *Fault) uint32 {
	h := uint32(2166136261)
	for _, s := range []string{f.Domain, f.Transport, fmt.Sprint(f.Attempts)} {
		for i := 0; i < len(s); i++ {
			h ^= uint32(s[i])
			h *= 16777619
		}
	}
	return h
}

func detectForwardCycles(all []Resolver, byID map[string]*Resolver) error {
	state := make(map[string]uint8, len(all))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return bizError(CodeConfigForwardCycle, "resolvers", "forwarding cycle includes resolver %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, next := range byID[id].Upstreams {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, r := range all {
		if err := visit(r.ID); err != nil {
			return err
		}
	}
	return nil
}

func detectDelegationCycles(all []Resolver, byID map[string]*Resolver) error {
	state := make(map[string]uint8, len(all))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return bizError(CodeConfigDelegationCycle, "resolvers", "delegation cycle includes resolver %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, d := range byID[id].Delegations {
			if err := visit(d.Target); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, r := range all {
		if err := visit(r.ID); err != nil {
			return err
		}
	}
	return nil
}

func detectCNAMECycles(records []Record) error {
	targets := make(map[string][]string)
	for _, r := range records {
		if r.Type == "CNAME" {
			targets[r.Name] = append(targets[r.Name], r.Value)
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 1 {
			return fmt.Errorf("CNAME cycle includes %q", name)
		}
		if state[name] == 2 {
			return nil
		}
		state[name] = 1
		for _, target := range targets[name] {
			if err := visit(target); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for name := range targets {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func validateReachability(cfg *Config, resolvers map[string]*Resolver) error {
	zonesByAuthority := map[string]int{}
	for _, z := range cfg.Zones {
		zonesByAuthority[z.Authority]++
	}
	for _, r := range cfg.Resolvers {
		if r.Role == RoleAuthority && zonesByAuthority[r.ID] == 0 {
			return bizError(CodeConfigInvalidTopology, "resolvers."+r.ID, "authority resolver has no zone")
		}
	}
	for _, s := range cfg.Sites {
		rec := resolvers[s.Recursive]
		seen := map[string]bool{}
		cur := rec
		for cur.Role == RoleRecursive || cur.Role == RoleForwarder {
			if seen[cur.ID] {
				return bizError(CodeConfigForwardCycle, "sites."+s.ID+".recursive", "recursive chain contains a cycle")
			}
			seen[cur.ID] = true
			next, ok := resolvers[cur.Upstreams[0]]
			if !ok {
				return bizError(CodeConfigMissingRef, "sites."+s.ID+".recursive", "unknown upstream %q", cur.Upstreams[0])
			}
			cur = next
		}
		if cur.Role != RoleRoot {
			return bizError(CodeConfigInvalidTopology, "sites."+s.ID+".recursive", "recursive chain must terminate at a root resolver")
		}
	}
	return nil
}

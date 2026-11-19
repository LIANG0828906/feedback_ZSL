package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	ErrorInvalidJSON        = "CONFIG_INVALID_JSON"
	ErrorUnknownField       = "CONFIG_UNKNOWN_FIELD"
	ErrorMissingField       = "CONFIG_MISSING_FIELD"
	ErrorInvalidTimeout     = "CONFIG_INVALID_TIMEOUT"
	ErrorRetryRange         = "CONFIG_RETRY_RANGE"
	ErrorPriorityDuplicate  = "CONFIG_PRIORITY_DUPLICATE"
	ErrorPriorityRange      = "CONFIG_PRIORITY_RANGE"
	ErrorDuplicateID        = "CONFIG_DUPLICATE_ID"
	ErrorUnknownUpstream    = "CONFIG_UNKNOWN_UPSTREAM"
	ErrorInvalidRoute       = "CONFIG_INVALID_ROUTE"
	ErrorInvalidUpstream    = "CONFIG_INVALID_UPSTREAM"
	ErrorInvalidListen      = "CONFIG_INVALID_LISTEN"
	ErrorListenChange       = "CONFIG_LISTEN_CHANGE_REQUIRES_RESTART"
	ErrorInvalidTLS         = "CONFIG_INVALID_TLS"
	ErrorInvalidLimit       = "CONFIG_INVALID_LIMIT"
	ErrorInvalidPriorityRef = "CONFIG_INVALID_PRIORITY_REFERENCE"
)

// Duration accepts Go duration strings ("250ms", "2s") in JSON.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return errors.New("duration must be a Go duration string or an integer number of nanoseconds")
	}
	*d = Duration(n)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

type Config struct {
	ListenH2     string           `json:"listen_h2"`
	ListenH3     string           `json:"listen_h3"`
	AdminListen  string           `json:"admin_listen"`
	TLSCertFile  string           `json:"tls_cert_file"`
	TLSKeyFile   string           `json:"tls_key_file"`
	Upstreams    []UpstreamConfig `json:"upstreams"`
	Routes       []RouteConfig    `json:"routes"`
	Scheduler    SchedulerConfig  `json:"scheduler"`
	Priorities   []PriorityConfig `json:"priorities"`
	DefaultPrio  string           `json:"default_priority"`
	Timeouts     TimeoutConfig    `json:"timeouts"`
	Limits       LimitConfig      `json:"limits"`
	Retry        RetryConfig      `json:"retry"`
	RequiredHdrs []string         `json:"required_request_headers"`

	Revision string `json:"-"`
	Path     string `json:"-"`
}

type UpstreamConfig struct {
	ID             string   `json:"id"`
	URL            string   `json:"url"`
	Protocol       string   `json:"protocol"`
	TLSInsecure    bool     `json:"tls_insecure"`
	MaxConnections int      `json:"max_connections"`
	MaxInflight    int      `json:"max_inflight"`
	HealthPath     string   `json:"health_path"`
	HealthInterval Duration `json:"health_interval"`
	HealthTimeout  Duration `json:"health_timeout"`
}

type RouteConfig struct {
	ID         string   `json:"id"`
	PathPrefix string   `json:"path_prefix"`
	Methods    []string `json:"methods"`
	Upstreams  []string `json:"upstreams"`
}

type SchedulerConfig struct {
	MaxInflight   int      `json:"max_inflight"`
	MaxWait       Duration `json:"max_wait"`
	AgingInterval Duration `json:"aging_interval"`
}

type PriorityConfig struct {
	Name         string   `json:"name"`
	Rank         int      `json:"rank"`
	H2WeightMin  int      `json:"h2_weight_min"`
	H2WeightMax  int      `json:"h2_weight_max"`
	H3Urgency    []int    `json:"h3_urgency"`
	HeaderValues []string `json:"header_values"`
}

type TimeoutConfig struct {
	Dial            Duration `json:"dial"`
	UpstreamHeaders Duration `json:"upstream_headers"`
	Request         Duration `json:"request"`
	HealthInterval  Duration `json:"health_interval"`
	HealthTimeout   Duration `json:"health_timeout"`
	Shutdown        Duration `json:"shutdown"`
	IdleConnection  Duration `json:"idle_connection"`
}

type LimitConfig struct {
	MaxHeaderBytes   int   `json:"max_header_bytes"`
	MaxRequestBytes  int   `json:"max_request_bytes"`
	MaxResponseBytes int64 `json:"max_response_bytes"`
}

type RetryConfig struct {
	Max     int      `json:"max"`
	Backoff Duration `json:"backoff"`
}

type ValidationError struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"error"`
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " (" + e.Field + "): " + e.Message
}

func (c *Config) ApplyDefaults() {
	if c.Scheduler.MaxInflight == 0 {
		c.Scheduler.MaxInflight = 4
	}
	if c.Scheduler.MaxWait == 0 {
		c.Scheduler.MaxWait = Duration(150 * time.Millisecond)
	}
	if c.Scheduler.AgingInterval == 0 {
		c.Scheduler.AgingInterval = Duration(40 * time.Millisecond)
	}
	if c.Timeouts.Dial == 0 {
		c.Timeouts.Dial = Duration(time.Second)
	}
	if c.Timeouts.UpstreamHeaders == 0 {
		c.Timeouts.UpstreamHeaders = Duration(3 * time.Second)
	}
	if c.Timeouts.Request == 0 {
		c.Timeouts.Request = Duration(10 * time.Second)
	}
	if c.Timeouts.HealthInterval == 0 {
		c.Timeouts.HealthInterval = Duration(time.Second)
	}
	if c.Timeouts.HealthTimeout == 0 {
		c.Timeouts.HealthTimeout = Duration(500 * time.Millisecond)
	}
	if c.Timeouts.Shutdown == 0 {
		c.Timeouts.Shutdown = Duration(2 * time.Second)
	}
	if c.Timeouts.IdleConnection == 0 {
		c.Timeouts.IdleConnection = Duration(15 * time.Second)
	}
	if c.Retry.Backoff == 0 {
		c.Retry.Backoff = Duration(10 * time.Millisecond)
	}
	if c.Limits.MaxHeaderBytes == 0 {
		c.Limits.MaxHeaderBytes = 32 << 10
	}
	if c.Limits.MaxRequestBytes == 0 {
		c.Limits.MaxRequestBytes = 2 << 20
	}
	if c.DefaultPrio == "" && len(c.Priorities) > 0 {
		c.DefaultPrio = c.Priorities[len(c.Priorities)-1].Name
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.MaxConnections == 0 {
			u.MaxConnections = 2
		}
		if u.MaxInflight == 0 {
			u.MaxInflight = 4
		}
		if u.HealthPath == "" {
			u.HealthPath = "/__health"
		}
		if u.HealthInterval == 0 {
			u.HealthInterval = c.Timeouts.HealthInterval
		}
		if u.HealthTimeout == 0 {
			u.HealthTimeout = c.Timeouts.HealthTimeout
		}
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ValidationError{Code: ErrorInvalidJSON, Message: err.Error()}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		code := ErrorInvalidJSON
		if strings.Contains(err.Error(), "unknown field") {
			code = ErrorUnknownField
		}
		return nil, &ValidationError{Code: code, Message: err.Error()}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, &ValidationError{Code: ErrorInvalidJSON, Message: "multiple JSON values are not allowed"}
	}
	c.Path = path
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	c.Revision = hex.EncodeToString(sum[:6])
	return &c, nil
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.ListenH2) == "" {
		return &ValidationError{Code: ErrorMissingField, Field: "listen_h2", Message: "HTTP/2 TCP listen address is required"}
	}
	if strings.TrimSpace(c.ListenH3) == "" {
		return &ValidationError{Code: ErrorMissingField, Field: "listen_h3", Message: "HTTP/3 UDP listen address is required"}
	}
	if strings.TrimSpace(c.AdminListen) == "" {
		return &ValidationError{Code: ErrorMissingField, Field: "admin_listen", Message: "admin TCP listen address is required"}
	}
	if c.ListenH2 == c.AdminListen {
		return &ValidationError{Code: ErrorInvalidListen, Field: "admin_listen", Message: "admin and HTTP/2 listeners cannot share a TCP address"}
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return &ValidationError{Code: ErrorInvalidTLS, Message: "tls_cert_file and tls_key_file must both be set or both be empty"}
	}
	if len(c.Upstreams) == 0 {
		return &ValidationError{Code: ErrorMissingField, Field: "upstreams", Message: "at least one upstream is required"}
	}
	if len(c.Routes) == 0 {
		return &ValidationError{Code: ErrorMissingField, Field: "routes", Message: "at least one route is required"}
	}
	if len(c.Priorities) == 0 {
		return &ValidationError{Code: ErrorMissingField, Field: "priorities", Message: "at least one priority mapping is required"}
	}
	for name, d := range map[string]time.Duration{
		"timeouts.dial":             c.Timeouts.Dial.Duration(),
		"timeouts.upstream_headers": c.Timeouts.UpstreamHeaders.Duration(),
		"timeouts.request":          c.Timeouts.Request.Duration(),
		"timeouts.health_interval":  c.Timeouts.HealthInterval.Duration(),
		"timeouts.health_timeout":   c.Timeouts.HealthTimeout.Duration(),
		"timeouts.shutdown":         c.Timeouts.Shutdown.Duration(),
		"timeouts.idle_connection":  c.Timeouts.IdleConnection.Duration(),
		"scheduler.max_wait":        c.Scheduler.MaxWait.Duration(),
		"scheduler.aging_interval":  c.Scheduler.AgingInterval.Duration(),
		"retry.backoff":             c.Retry.Backoff.Duration(),
	} {
		if d < 0 {
			return &ValidationError{Code: ErrorInvalidTimeout, Field: name, Message: "duration must not be negative"}
		}
	}
	if c.Scheduler.MaxInflight < 1 || c.Scheduler.MaxInflight > 1024 {
		return &ValidationError{Code: ErrorInvalidLimit, Field: "scheduler.max_inflight", Message: "must be between 1 and 1024"}
	}
	if c.Scheduler.MaxWait <= 0 {
		return &ValidationError{Code: ErrorInvalidTimeout, Field: "scheduler.max_wait", Message: "must be greater than zero"}
	}
	if c.Scheduler.AgingInterval <= 0 {
		return &ValidationError{Code: ErrorInvalidTimeout, Field: "scheduler.aging_interval", Message: "must be greater than zero"}
	}
	if c.Retry.Max < 0 || c.Retry.Max > 10 {
		return &ValidationError{Code: ErrorRetryRange, Field: "retry.max", Message: "must be between 0 and 10"}
	}
	if c.Limits.MaxHeaderBytes < 1024 {
		return &ValidationError{Code: ErrorInvalidLimit, Field: "limits.max_header_bytes", Message: "must be at least 1024"}
	}
	if c.Limits.MaxRequestBytes < 0 {
		return &ValidationError{Code: ErrorInvalidLimit, Field: "limits.max_request_bytes", Message: "must not be negative"}
	}
	if c.Limits.MaxResponseBytes < 0 {
		return &ValidationError{Code: ErrorInvalidLimit, Field: "limits.max_response_bytes", Message: "must not be negative"}
	}
	upstreams := make(map[string]bool, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.ID == "" {
			return &ValidationError{Code: ErrorMissingField, Field: fmt.Sprintf("upstreams[%d].id", i), Message: "id is required"}
		}
		if upstreams[u.ID] {
			return &ValidationError{Code: ErrorDuplicateID, Field: "upstreams", Message: "duplicate upstream id " + u.ID}
		}
		upstreams[u.ID] = true
		switch u.Protocol {
		case "http1", "http2", "h2c":
		default:
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].protocol", i), Message: "must be http1, http2, or h2c"}
		}
		parsed, err := url.Parse(u.URL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].url", i), Message: "must be an absolute URL"}
		}
		if parsed.Path != "" && parsed.Path != "/" {
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].url", i), Message: "base paths are not supported because the client path must be preserved"}
		}
		if u.Protocol == "http2" && parsed.Scheme != "https" {
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].url", i), Message: "http2 upstreams must use https"}
		}
		if u.Protocol == "http1" && parsed.Scheme != "http" {
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].url", i), Message: "http1 fixture upstreams must use http"}
		}
		if u.Protocol == "h2c" && parsed.Scheme != "http" {
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].url", i), Message: "h2c upstreams must use http"}
		}
		if u.MaxConnections < 1 || u.MaxConnections > 64 {
			return &ValidationError{Code: ErrorInvalidLimit, Field: fmt.Sprintf("upstreams[%d].max_connections", i), Message: "must be between 1 and 64"}
		}
		if u.MaxInflight < 1 || u.MaxInflight > 1024 {
			return &ValidationError{Code: ErrorInvalidLimit, Field: fmt.Sprintf("upstreams[%d].max_inflight", i), Message: "must be between 1 and 1024"}
		}
		if !strings.HasPrefix(u.HealthPath, "/") {
			return &ValidationError{Code: ErrorInvalidUpstream, Field: fmt.Sprintf("upstreams[%d].health_path", i), Message: "must start with /"}
		}
		if u.HealthInterval < 0 || u.HealthTimeout < 0 {
			return &ValidationError{Code: ErrorInvalidTimeout, Field: fmt.Sprintf("upstreams[%d].health_*", i), Message: "durations must not be negative"}
		}
	}

	prioByName := make(map[string]PriorityConfig)
	prioRanks := make(map[int]string)
	h3Mapped := make(map[int]string)
	headersMapped := make(map[string]string)
	type weightRange struct {
		min, max int
		name     string
	}
	var ranges []weightRange
	for i, p := range c.Priorities {
		field := fmt.Sprintf("priorities[%d]", i)
		if strings.TrimSpace(p.Name) == "" {
			return &ValidationError{Code: ErrorMissingField, Field: field + ".name", Message: "name is required"}
		}
		if _, ok := prioByName[p.Name]; ok {
			return &ValidationError{Code: ErrorPriorityDuplicate, Field: field + ".name", Message: "duplicate priority name " + p.Name}
		}
		if other, ok := prioRanks[p.Rank]; ok {
			return &ValidationError{Code: ErrorPriorityDuplicate, Field: field + ".rank", Message: fmt.Sprintf("rank %d already maps to %s", p.Rank, other)}
		}
		prioByName[p.Name] = p
		prioRanks[p.Rank] = p.Name
		if p.H2WeightMin < 1 || p.H2WeightMax > 256 || p.H2WeightMin > p.H2WeightMax {
			return &ValidationError{Code: ErrorPriorityRange, Field: field + ".h2_weight", Message: "weight range must be inside 1..256 and min <= max"}
		}
		for _, r := range ranges {
			if p.H2WeightMin <= r.max && p.H2WeightMax >= r.min {
				return &ValidationError{Code: ErrorPriorityDuplicate, Field: field + ".h2_weight", Message: fmt.Sprintf("weight range overlaps mapping %s", r.name)}
			}
		}
		ranges = append(ranges, weightRange{p.H2WeightMin, p.H2WeightMax, p.Name})
		if len(p.H3Urgency) == 0 {
			return &ValidationError{Code: ErrorMissingField, Field: field + ".h3_urgency", Message: "at least one urgency value is required"}
		}
		for _, urgency := range p.H3Urgency {
			if urgency < 0 || urgency > 7 {
				return &ValidationError{Code: ErrorPriorityRange, Field: field + ".h3_urgency", Message: "urgency must be inside 0..7"}
			}
			if other, ok := h3Mapped[urgency]; ok {
				return &ValidationError{Code: ErrorPriorityDuplicate, Field: field + ".h3_urgency", Message: fmt.Sprintf("urgency %d already maps to %s", urgency, other)}
			}
			h3Mapped[urgency] = p.Name
		}
		for _, hv := range p.HeaderValues {
			hv = strings.ToLower(strings.TrimSpace(hv))
			if hv == "" {
				return &ValidationError{Code: ErrorPriorityRange, Field: field + ".header_values", Message: "empty header value mapping"}
			}
			if other, ok := headersMapped[hv]; ok {
				return &ValidationError{Code: ErrorPriorityDuplicate, Field: field + ".header_values", Message: fmt.Sprintf("%q already maps to %s", hv, other)}
			}
			headersMapped[hv] = p.Name
		}
	}
	if _, ok := prioByName[c.DefaultPrio]; !ok {
		return &ValidationError{Code: ErrorInvalidPriorityRef, Field: "default_priority", Message: "unknown priority " + c.DefaultPrio}
	}

	routeIDs := make(map[string]bool)
	prefixes := make(map[string]string)
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.ID == "" {
			return &ValidationError{Code: ErrorMissingField, Field: fmt.Sprintf("routes[%d].id", i), Message: "id is required"}
		}
		if routeIDs[r.ID] {
			return &ValidationError{Code: ErrorDuplicateID, Field: "routes", Message: "duplicate route id " + r.ID}
		}
		routeIDs[r.ID] = true
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return &ValidationError{Code: ErrorInvalidRoute, Field: fmt.Sprintf("routes[%d].path_prefix", i), Message: "must start with /"}
		}
		if other, ok := prefixes[r.PathPrefix]; ok {
			return &ValidationError{Code: ErrorInvalidRoute, Field: fmt.Sprintf("routes[%d].path_prefix", i), Message: "duplicate prefix also used by route " + other}
		}
		prefixes[r.PathPrefix] = r.ID
		if len(r.Upstreams) == 0 {
			return &ValidationError{Code: ErrorMissingField, Field: fmt.Sprintf("routes[%d].upstreams", i), Message: "at least one upstream candidate is required"}
		}
		seen := make(map[string]bool)
		for j, id := range r.Upstreams {
			if !upstreams[id] {
				return &ValidationError{Code: ErrorUnknownUpstream, Field: fmt.Sprintf("routes[%d].upstreams[%d]", i, j), Message: "undeclared upstream " + id}
			}
			if seen[id] {
				return &ValidationError{Code: ErrorDuplicateID, Field: fmt.Sprintf("routes[%d].upstreams", i), Message: "duplicate candidate " + id}
			}
			seen[id] = true
		}
		for j, m := range r.Methods {
			if !validMethod(m) {
				return &ValidationError{Code: ErrorInvalidRoute, Field: fmt.Sprintf("routes[%d].methods[%d]", i, j), Message: "invalid HTTP method " + m}
			}
			r.Methods[j] = strings.ToUpper(m)
		}
	}
	for i, h := range c.RequiredHdrs {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || strings.ContainsAny(h, " :\t\r\n") {
			return &ValidationError{Code: ErrorInvalidRoute, Field: fmt.Sprintf("required_request_headers[%d]", i), Message: "invalid header name"}
		}
		c.RequiredHdrs[i] = h
	}
	return nil
}

func validMethod(m string) bool {
	if m == "" {
		return false
	}
	for _, r := range m {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={}", r) {
			return false
		}
	}
	return true
}

// SortedUpstreams returns a stable list for diagnostics.
func (c *Config) SortedUpstreams() []UpstreamConfig {
	out := append([]UpstreamConfig(nil), c.Upstreams...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

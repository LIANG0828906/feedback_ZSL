package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validConfig() *Config {
	c := &Config{
		ListenH2:    "127.0.0.1:0",
		ListenH3:    "127.0.0.1:0",
		AdminListen: "127.0.0.1:1",
		Upstreams: []UpstreamConfig{
			{ID: "one", URL: "http://127.0.0.1:19001", Protocol: "http1"},
		},
		Routes: []RouteConfig{
			{ID: "r", PathPrefix: "/api", Upstreams: []string{"one"}},
		},
		Priorities: []PriorityConfig{
			{Name: "high", Rank: 0, H2WeightMin: 192, H2WeightMax: 256, H3Urgency: []int{0, 1}},
			{Name: "normal", Rank: 10, H2WeightMin: 64, H2WeightMax: 191, H3Urgency: []int{2, 3, 4}},
			{Name: "low", Rank: 20, H2WeightMin: 1, H2WeightMax: 63, H3Urgency: []int{5, 6, 7}},
		},
		DefaultPrio: "normal",
		Scheduler: SchedulerConfig{
			MaxInflight:   4,
			MaxWait:       Duration(150 * time.Millisecond),
			AgingInterval: Duration(40 * time.Millisecond),
		},
		Timeouts: TimeoutConfig{
			Dial:            Duration(time.Second),
			UpstreamHeaders: Duration(time.Second),
			Request:         Duration(time.Second),
			HealthInterval:  Duration(time.Second),
			HealthTimeout:   Duration(500 * time.Millisecond),
			Shutdown:        Duration(time.Second),
			IdleConnection:  Duration(time.Second),
		},
		Limits: LimitConfig{MaxHeaderBytes: 16 << 10, MaxRequestBytes: 1 << 20},
		Retry:  RetryConfig{Max: 1, Backoff: Duration(time.Millisecond)},
	}
	c.ApplyDefaults()
	return c
}

func TestValidateRejectsNegativeTimeout(t *testing.T) {
	c := validConfig()
	c.Timeouts.Dial = Duration(-time.Second)
	err := c.Validate()
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != ErrorInvalidTimeout {
		t.Fatalf("got %#v, want %s", err, ErrorInvalidTimeout)
	}
}

func TestValidateRejectsRetryRange(t *testing.T) {
	c := validConfig()
	c.Retry.Max = 11
	err := c.Validate()
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != ErrorRetryRange {
		t.Fatalf("got %#v, want %s", err, ErrorRetryRange)
	}
}

func TestValidateRejectsDuplicatePriorityMapping(t *testing.T) {
	c := validConfig()
	c.Priorities = append(c.Priorities, PriorityConfig{
		Name: "duplicate", Rank: 30, H2WeightMin: 257, H2WeightMax: 300, H3Urgency: []int{7},
	})
	err := c.Validate()
	ve, ok := err.(*ValidationError)
	if !ok || (ve.Code != ErrorPriorityDuplicate && ve.Code != ErrorPriorityRange) {
		t.Fatalf("got %#v, want duplicate/range priority error", err)
	}
}

func TestLoadRejectsTrailingJSONAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"listen_h2":"127.0.0.1:1"} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected trailing JSON to be rejected")
	}
	path = filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(path, []byte(`{"listen_h2":"127.0.0.1:1","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

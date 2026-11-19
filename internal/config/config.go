package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	DropPolicyReject = "reject"
	DropPolicyDrop   = "drop"
)

type RedisConfig struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	DB       int    `json:"db"`
	Prefix   string `json:"prefix"`
}

type TenantConfig struct {
	LogsPerSecond     int64  `json:"logs_per_second"`
	BytesPerSecond    int64  `json:"bytes_per_second"`
	DropPolicy        string `json:"drop_policy"`
	MaxPendingBatches int64  `json:"max_degraded_pending_batches"`
	MaxPendingBytes   int64  `json:"max_degraded_pending_bytes"`
}

type BackpressureConfig struct {
	Enabled              bool    `json:"enabled"`
	EnterBacklogBatches  int64   `json:"enter_backlog_batches"`
	ExitBacklogBatches   int64   `json:"exit_backlog_batches"`
	Ratio                float64 `json:"ratio"`
	PollIntervalMS       int     `json:"poll_interval_ms"`
	ExitHoldMS           int     `json:"exit_hold_ms"`
	MaxRetryAfterSeconds int     `json:"max_retry_after_seconds"`
}

type DegradeConfig struct {
	WriteTimeoutMS          int `json:"write_timeout_ms"`
	WindowMS                int `json:"window_ms"`
	TimeoutBatchesThreshold int `json:"timeout_batches_threshold"`
	RecoverySuccessSamples  int `json:"recovery_success_samples"`
	RecoveryQuietMS         int `json:"recovery_quiet_ms"`
	ScanIntervalMS          int `json:"scan_interval_ms"`
}

type Config struct {
	ListenAddr   string                  `json:"listen_addr"`
	AdminEnabled bool                    `json:"admin_enabled"`
	MaxBodyBytes int64                   `json:"max_body_bytes"`
	Redis        RedisConfig             `json:"redis"`
	Tenants      map[string]TenantConfig `json:"tenants"`
	Backpressure BackpressureConfig      `json:"backpressure"`
	Degrade      DegradeConfig           `json:"degrade"`
}

func Default() Config {
	return Config{
		ListenAddr:   "127.0.0.1:8080",
		AdminEnabled: true,
		MaxBodyBytes: 16 << 20,
		Redis: RedisConfig{
			Addr:   "127.0.0.1:6379",
			DB:     0,
			Prefix: "lq:",
		},
		Tenants: map[string]TenantConfig{},
		Backpressure: BackpressureConfig{
			Enabled:              true,
			EnterBacklogBatches:  120,
			ExitBacklogBatches:   60,
			Ratio:                0.25,
			PollIntervalMS:       200,
			ExitHoldMS:           1000,
			MaxRetryAfterSeconds: 2,
		},
		Degrade: DegradeConfig{
			WriteTimeoutMS:          1000,
			WindowMS:                5000,
			TimeoutBatchesThreshold: 5,
			RecoverySuccessSamples:  3,
			RecoveryQuietMS:         1500,
			ScanIntervalMS:          100,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.Redis.Addr == "" {
		return errors.New("redis.addr is required")
	}
	if c.Redis.Prefix == "" {
		c.Redis.Prefix = "lq:"
	}
	if c.MaxBodyBytes <= 0 {
		return errors.New("max_body_bytes must be positive")
	}
	if len(c.Tenants) == 0 {
		return errors.New("at least one tenant is required")
	}
	if c.Backpressure.EnterBacklogBatches <= 0 {
		return errors.New("backpressure.enter_backlog_batches must be positive")
	}
	if c.Backpressure.ExitBacklogBatches < 0 || c.Backpressure.ExitBacklogBatches >= c.Backpressure.EnterBacklogBatches {
		return errors.New("backpressure.exit_backlog_batches must be non-negative and lower than the enter threshold")
	}
	if c.Backpressure.Ratio <= 0 || c.Backpressure.Ratio > 1 {
		return errors.New("backpressure.ratio must be in (0,1]")
	}
	if c.Backpressure.PollIntervalMS < 50 {
		return errors.New("backpressure.poll_interval_ms must be at least 50")
	}
	if c.Degrade.WriteTimeoutMS <= 0 || c.Degrade.WindowMS <= 0 || c.Degrade.ScanIntervalMS <= 0 {
		return errors.New("degrade timeout, window and scan interval must be positive")
	}
	if c.Degrade.TimeoutBatchesThreshold <= 0 || c.Degrade.RecoverySuccessSamples <= 0 {
		return errors.New("degrade thresholds must be positive")
	}
	for id, t := range c.Tenants {
		if err := ValidateTenant(t); err != nil {
			return fmt.Errorf("tenant %q: %w", id, err)
		}
	}
	return nil
}

func ValidateTenant(t TenantConfig) error {
	if t.LogsPerSecond <= 0 {
		return errors.New("logs_per_second must be positive")
	}
	if t.BytesPerSecond <= 0 {
		return errors.New("bytes_per_second must be positive")
	}
	switch t.DropPolicy {
	case "", DropPolicyReject:
	case DropPolicyDrop:
	default:
		return fmt.Errorf("unsupported drop_policy %q", t.DropPolicy)
	}
	if t.MaxPendingBatches < 0 || t.MaxPendingBytes < 0 {
		return errors.New("degraded pending limits must be non-negative")
	}
	return nil
}

func NormalizeTenant(t *TenantConfig) {
	if t.DropPolicy == "" {
		t.DropPolicy = DropPolicyReject
	}
	if t.MaxPendingBatches == 0 {
		t.MaxPendingBatches = 20
	}
	if t.MaxPendingBytes == 0 {
		t.MaxPendingBytes = 8 << 20
	}
}

func (c Config) TenantIDs() []string {
	ids := make([]string, 0, len(c.Tenants))
	for id := range c.Tenants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (c Config) BackpressurePollInterval() time.Duration {
	return time.Duration(c.Backpressure.PollIntervalMS) * time.Millisecond
}

func (c Config) DegradeScanInterval() time.Duration {
	return time.Duration(c.Degrade.ScanIntervalMS) * time.Millisecond
}

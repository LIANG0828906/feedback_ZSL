package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is deliberately small enough to run on a 2-core / 1.25 GiB host.
// Session limits bound memory, while the short backend timeout bounds the
// amount of work that can pile up behind one slow fragment.
type Config struct {
	UDPListen          string  `json:"udp_listen"`
	TCPListen          string  `json:"tcp_listen"`
	MetricsListen      string  `json:"metrics_listen"`
	BackendURL         string  `json:"backend_url"`
	DBPath             string  `json:"db_path"`
	CertFile           string  `json:"cert_file"`
	KeyFile            string  `json:"key_file"`
	MaxSessions        int     `json:"max_sessions"`
	MaxSessionInflight int     `json:"max_session_inflight"`
	MigrationWindowMS  int64   `json:"migration_window_ms"`
	RTTThresholdMS     float64 `json:"rtt_threshold_ms"`
	BacklogThreshold   int     `json:"backlog_threshold"`
	RateLimitPerSec    float64 `json:"rate_limit_per_sec"`
	BackendTimeoutMS   int64   `json:"backend_timeout_ms"`
	FeedbackRecoveryN  int     `json:"feedback_recovery_samples"`
	AllowPeerOverride  bool    `json:"allow_peer_override"`
	MaxFragmentBytes   int64   `json:"max_fragment_bytes"`
	MaxBodyBytes       int64   `json:"max_body_bytes"`
	ShutdownTimeoutMS  int64   `json:"shutdown_timeout_ms"`
}

func Default() Config {
	return Config{
		UDPListen:          "0.0.0.0:4433",
		TCPListen:          "0.0.0.0:8443",
		MetricsListen:      "127.0.0.1:19090",
		BackendURL:         "http://127.0.0.1:18080",
		DBPath:             "data/edgegateway.db",
		CertFile:           "data/cert.pem",
		KeyFile:            "data/key.pem",
		MaxSessions:        64,
		MaxSessionInflight: 8,
		MigrationWindowMS:  8000,
		RTTThresholdMS:     80,
		BacklogThreshold:   4,
		RateLimitPerSec:    12,
		BackendTimeoutMS:   3000,
		FeedbackRecoveryN:  5,
		AllowPeerOverride:  true,
		MaxFragmentBytes:   4096,
		MaxBodyBytes:       16384,
		ShutdownTimeoutMS:  5000,
	}
}

func (c Config) Validate() error {
	var errs []error
	if c.UDPListen == "" {
		errs = append(errs, errors.New("udp_listen is required"))
	}
	if c.TCPListen == "" {
		errs = append(errs, errors.New("tcp_listen is required"))
	}
	if c.BackendURL == "" {
		errs = append(errs, errors.New("backend_url is required"))
	}
	if c.DBPath == "" {
		errs = append(errs, errors.New("db_path is required"))
	}
	if c.MaxSessions <= 0 || c.MaxSessions > 512 {
		errs = append(errs, errors.New("max_sessions must be in 1..512"))
	}
	if c.MaxSessionInflight <= 0 || c.MaxSessionInflight > 64 {
		errs = append(errs, errors.New("max_session_inflight must be in 1..64"))
	}
	if c.MigrateWindow() <= 0 {
		errs = append(errs, errors.New("migration_window_ms must be positive"))
	}
	if c.RTTThresholdMS <= 0 {
		errs = append(errs, errors.New("rtt_threshold_ms must be positive"))
	}
	if c.BacklogThreshold <= 0 {
		errs = append(errs, errors.New("backlog_threshold must be positive"))
	}
	if c.RateLimitPerSec <= 0 {
		errs = append(errs, errors.New("rate_limit_per_sec must be positive"))
	}
	if c.BackendTimeout() <= 0 {
		errs = append(errs, errors.New("backend_timeout_ms must be positive"))
	}
	if c.MaxFragmentBytes <= 0 || c.MaxFragmentBytes > 1024*1024 {
		errs = append(errs, errors.New("max_fragment_bytes must be in 1..1048576"))
	}
	if c.MaxBodyBytes < c.MaxFragmentBytes || c.MaxBodyBytes > 4*1024*1024 {
		errs = append(errs, errors.New("max_body_bytes must be >= max_fragment_bytes and <= 4MiB"))
	}
	return errors.Join(errs...)
}

func (c Config) MigrateWindow() time.Duration {
	return time.Duration(c.MigrationWindowMS) * time.Millisecond
}

func (c Config) BackendTimeout() time.Duration {
	return time.Duration(c.BackendTimeoutMS) * time.Millisecond
}

func (c Config) ShutdownTimeout() time.Duration {
	return time.Duration(c.ShutdownTimeoutMS) * time.Millisecond
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	return cfg, cfg.Validate()
}

func WriteExample(path string) error {
	cfg, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(cfg, '\n'), 0o644)
}

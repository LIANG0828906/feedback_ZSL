// Package config defines the local-only runtime configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const DefaultAlgorithmVersion = "env-synth-v1"

type Config struct {
	Seed                int64  `json:"seed"`
	DevicePort          int    `json:"device_port"`
	UIPort              int    `json:"ui_port"`
	DBPath              string `json:"db_path"`
	SampleDir           string `json:"sample_dir"`
	SelectedBranch      string `json:"selected_branch"`
	CommitWindowPackets int    `json:"commit_window_packets"`
	Workers             int    `json:"workers"`
	BufferPerWorker     int    `json:"buffer_per_worker"`
	DeviceTimeoutMS     int64  `json:"device_timeout_ms"`
	ParamVersion        string `json:"param_version"`
	AlgorithmVersion    string `json:"algorithm_version"`
}

func Default() Config {
	return Config{
		Seed:                424242,
		DevicePort:          18081,
		UIPort:              18080,
		DBPath:              filepath.Join("data", "checkpoints.db"),
		SampleDir:           filepath.Join("data", "samples"),
		SelectedBranch:      "mobile-ms",
		CommitWindowPackets: 4,
		Workers:             3,
		BufferPerWorker:     2,
		DeviceTimeoutMS:     500,
		ParamVersion:        "local-p1",
		AlgorithmVersion:    DefaultAlgorithmVersion,
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, cfg.Validate()
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.DevicePort < 0 || c.DevicePort > 65535 {
		return fmt.Errorf("device_port must be between 0 and 65535")
	}
	if c.UIPort < 0 || c.UIPort > 65535 {
		return fmt.Errorf("ui_port must be between 0 and 65535")
	}
	if c.DevicePort != 0 && c.DevicePort == c.UIPort {
		return fmt.Errorf("device_port and ui_port must differ")
	}
	if c.DBPath == "" || c.SampleDir == "" {
		return fmt.Errorf("db_path and sample_dir are required")
	}
	if c.SelectedBranch == "" {
		return fmt.Errorf("selected_branch is required")
	}
	if c.CommitWindowPackets <= 0 {
		return fmt.Errorf("commit_window_packets must be positive")
	}
	if c.Workers <= 0 || c.Workers > 64 {
		return fmt.Errorf("workers must be in [1,64]")
	}
	if c.BufferPerWorker <= 0 || c.BufferPerWorker > 1024 {
		return fmt.Errorf("buffer_per_worker must be in [1,1024]")
	}
	if c.DeviceTimeoutMS <= 0 {
		return fmt.Errorf("device_timeout_ms must be positive")
	}
	if c.ParamVersion == "" || c.AlgorithmVersion == "" {
		return fmt.Errorf("parameter and algorithm versions are required")
	}
	return nil
}

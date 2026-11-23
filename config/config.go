package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Address         string `json:"address"`
	SamplesDir      string `json:"samples_dir"`
	CheckpointDB    string `json:"checkpoint_db"`
	SavesDir        string `json:"saves_dir"`
	ArtifactsDir    string `json:"artifacts_dir"`
	Workers         int    `json:"workers"`
	BufferPerWorker int    `json:"buffer_per_worker"`
}

func Default() Config {
	return Config{
		Address:         "127.0.0.1:8787",
		SamplesDir:      "samples",
		CheckpointDB:    filepath.Join("data", "checkpoints.db"),
		SavesDir:        filepath.Join("data", "saves"),
		ArtifactsDir:    filepath.Join("artifacts", "runs"),
		Workers:         4,
		BufferPerWorker: 8,
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Address == "" {
		return fmt.Errorf("address is required")
	}
	if c.SamplesDir == "" {
		return fmt.Errorf("samples_dir is required")
	}
	if c.CheckpointDB == "" {
		return fmt.Errorf("checkpoint_db is required")
	}
	if c.SavesDir == "" {
		return fmt.Errorf("saves_dir is required")
	}
	if c.ArtifactsDir == "" {
		return fmt.Errorf("artifacts_dir is required")
	}
	if c.Workers <= 0 {
		return fmt.Errorf("workers must be positive")
	}
	if c.BufferPerWorker <= 0 {
		return fmt.Errorf("buffer_per_worker must be positive")
	}
	return nil
}

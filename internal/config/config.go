package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"roomreverb/internal/model"
)

type PrecisionProfile struct {
	Decimals   int     `json:"decimals"`
	LengthTol  float64 `json:"length_tol"`
	CurveTolDB float64 `json:"curve_tol_db"`
	SPLTolDB   float64 `json:"spl_tol_db"`
}

type Config struct {
	Seed              int64                       `json:"seed"`
	LevelPath         string                      `json:"level_path"`
	DatabasePath      string                      `json:"database_path"`
	ListenAddr        string                      `json:"listen_addr"`
	PrecisionProfile  string                      `json:"precision_profile"`
	Profiles          map[string]PrecisionProfile `json:"profiles"`
	MaxParallelism    int                         `json:"max_parallelism"`
	SampleBufferLimit int                         `json:"sample_buffer_limit"`
	CommitOrder       []string                    `json:"commit_order"`
	AlgorithmVersion  string                      `json:"algorithm_version"`
	ParameterVersion  string                      `json:"parameter_version"`
	StrictScope       bool                        `json:"strict_scope"`
}

func Default() Config {
	return Config{
		Seed:              42,
		LevelPath:         "generated/level.json",
		DatabasePath:      "data/workbench.sqlite",
		ListenAddr:        "127.0.0.1:8088",
		PrecisionProfile:  "standard",
		MaxParallelism:    4,
		SampleBufferLimit: 64,
		CommitOrder:       []string{"R1", "R2", "R3", "R4", "R5", "R6"},
		AlgorithmVersion:  "topology-dsp-v1",
		ParameterVersion:  "params-v1",
		StrictScope:       true,
		Profiles: map[string]PrecisionProfile{
			"coarse":   {Decimals: 3, LengthTol: 0.25, CurveTolDB: 0.08, SPLTolDB: 0.12},
			"standard": {Decimals: 6, LengthTol: 0.05, CurveTolDB: 0.004, SPLTolDB: 0.008},
			"tight":    {Decimals: 9, LengthTol: 0.01, CurveTolDB: 0.0008, SPLTolDB: 0.0015},
		},
	}
}

func (c Config) Profile() (PrecisionProfile, *model.Error) {
	p, ok := c.Profiles[c.PrecisionProfile]
	if !ok {
		return PrecisionProfile{}, model.NewError(model.CodePrec, c.PrecisionProfile, "unknown precision profile")
	}
	return p, nil
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, model.NewError(model.CodeSchema, "config", err.Error())
	}
	if cfg.MaxParallelism <= 0 {
		cfg.MaxParallelism = 1
	}
	if cfg.SampleBufferLimit <= 0 {
		cfg.SampleBufferLimit = 1
	}
	return cfg, nil
}

func (c Config) SaveExample(path string) error {
	data, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

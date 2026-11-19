package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	GitRepo          string
	ManifestPath     string
	DevBranch        string
	ProdBranch       string
	DevNamespace     string
	ProdNamespace    string
	DevKubeconfig    string
	ProdKubeconfig   string
	DatabaseURL      string
	HTTPAddr         string
	Interval         time.Duration
	AdmissionTimeout time.Duration
	AdmissionPoll    time.Duration
}

func Load() (Config, error) {
	config := Config{
		GitRepo:          env("DRIFT_GIT_REPO", "./lab/repo.git"),
		ManifestPath:     env("DRIFT_MANIFEST_PATH", "manifests"),
		DevBranch:        env("DRIFT_BRANCH_DEV", "cluster-dev"),
		ProdBranch:       env("DRIFT_BRANCH_PROD", "cluster-prod"),
		DevNamespace:     env("DRIFT_NAMESPACE_DEV", "cluster-dev"),
		ProdNamespace:    env("DRIFT_NAMESPACE_PROD", "cluster-prod"),
		DevKubeconfig:    os.Getenv("DRIFT_KUBECONFIG_DEV"),
		ProdKubeconfig:   os.Getenv("DRIFT_KUBECONFIG_PROD"),
		DatabaseURL:      env("DRIFT_DATABASE_URL", "postgres://drift@127.0.0.1:5432/drift315?sslmode=disable"),
		HTTPAddr:         env("DRIFT_HTTP_ADDR", ":8080"),
		Interval:         5 * time.Second,
		AdmissionTimeout: 20 * time.Second,
		AdmissionPoll:    time.Second,
	}
	var err error
	if config.Interval, err = durationEnv("DRIFT_INTERVAL", config.Interval); err != nil {
		return Config{}, err
	}
	if config.AdmissionTimeout, err = durationEnv("DRIFT_ADMISSION_TIMEOUT", config.AdmissionTimeout); err != nil {
		return Config{}, err
	}
	if config.AdmissionPoll, err = durationEnv("DRIFT_ADMISSION_POLL", config.AdmissionPoll); err != nil {
		return Config{}, err
	}
	if config.GitRepo == "" {
		return Config{}, fmt.Errorf("DRIFT_GIT_REPO is required")
	}
	if config.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DRIFT_DATABASE_URL is required")
	}
	return config, nil
}

func env(name, fallback string) string {
	if value, found := os.LookupEnv(name); found {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return parsed, nil
}

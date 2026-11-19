package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Listen           string
	Backend          string
	Region           string
	AccessKey        string
	SecretKey        string
	BackendAccessKey string
	BackendSecretKey string
	DataFile         string
	TempDir          string
	PartRetention    time.Duration
	ReaperInterval   time.Duration
	ClockSkew        time.Duration
	MaxObjectBytes   int64
	MaxListKeys      int
	DisableReaper    bool
	LogRequests      bool
}

func Load(args []string) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("s3gw", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", env("S3GW_LISTEN", "127.0.0.1:9100"), "HTTP listen address")
	fs.StringVar(&c.Backend, "backend", env("S3GW_BACKEND", "http://127.0.0.1:9000"), "MinIO S3 endpoint")
	fs.StringVar(&c.Region, "region", env("S3GW_REGION", "us-east-1"), "S3 signing region")
	fs.StringVar(&c.AccessKey, "access-key", env("S3GW_ACCESS_KEY", "gateway"), "client access key")
	fs.StringVar(&c.SecretKey, "secret-key", env("S3GW_SECRET_KEY", "gateway-secret"), "client secret key")
	fs.StringVar(&c.BackendAccessKey, "backend-access-key", env("S3GW_BACKEND_ACCESS_KEY", "minioadmin"), "MinIO access key")
	fs.StringVar(&c.BackendSecretKey, "backend-secret-key", env("S3GW_BACKEND_SECRET_KEY", "minioadmin"), "MinIO secret key")
	fs.StringVar(&c.DataFile, "data", env("S3GW_DATA", filepath.Join("data", "gateway.db")), "SQLite database path")
	fs.StringVar(&c.TempDir, "temp-dir", env("S3GW_TEMP_DIR", ""), "temporary directory (default: next to database)")
	fs.DurationVar(&c.PartRetention, "part-retention", durationEnv("S3GW_PART_RETENTION", 14*24*time.Hour), "incomplete multipart retention")
	fs.DurationVar(&c.ReaperInterval, "reaper-interval", durationEnv("S3GW_REAPER_INTERVAL", time.Minute), "multipart reaper interval")
	fs.DurationVar(&c.ClockSkew, "clock-skew", durationEnv("S3GW_CLOCK_SKEW", 15*time.Minute), "allowed request clock skew (0 disables)")
	fs.Int64Var(&c.MaxObjectBytes, "max-object-bytes", 64<<20, "maximum accepted request/stored object size")
	fs.IntVar(&c.MaxListKeys, "max-list-keys", 1000, "maximum keys returned by list operations")
	fs.BoolVar(&c.DisableReaper, "disable-reaper", false, "disable background multipart reaping")
	fs.BoolVar(&c.LogRequests, "log-requests", false, "log one line per request")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.Listen == "" || c.Backend == "" || c.AccessKey == "" || c.SecretKey == "" {
		return c, fmt.Errorf("listen, backend, access-key and secret-key are required")
	}
	if c.PartRetention < time.Second {
		return c, fmt.Errorf("part-retention must be at least 1s")
	}
	if c.ReaperInterval <= 0 {
		return c, fmt.Errorf("reaper-interval must be positive")
	}
	if c.MaxObjectBytes <= 0 {
		return c, fmt.Errorf("max-object-bytes must be positive")
	}
	if c.TempDir == "" {
		c.TempDir = filepath.Join(filepath.Dir(c.DataFile), "tmp")
	}
	return c, nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func durationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

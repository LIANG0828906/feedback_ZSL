package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultStripeSize     int64 = 1 << 20
	DefaultQuotaBytes     int64 = 1 << 30
	DefaultMaxObjectBytes int64 = 64 << 20
)

type Config struct {
	ListenAddr          string `json:"listen_addr"`
	DataRoot            string `json:"data_root"`
	RedisAddr           string `json:"redis_addr"`
	RedisPassword       string `json:"redis_password"`
	RedisDB             int    `json:"redis_db"`
	RedisNamespace      string `json:"redis_namespace"`
	SQLitePath          string `json:"sqlite_path"`
	StripeSize          int64  `json:"stripe_size"`
	DataBlocks          int    `json:"data_blocks"`
	ParityBlocks        int    `json:"parity_blocks"`
	WritebackQuotaBytes int64  `json:"writeback_quota_bytes"`
	RepairConcurrency   int    `json:"repair_concurrency"`
	AutoRepair          bool   `json:"auto_repair"`
	RepairPollMS        int    `json:"repair_poll_ms"`
	RepairDelayMS       int    `json:"repair_delay_ms"`
	MaxObjectBytes      int64  `json:"max_object_bytes"`
	Timezone            string `json:"timezone"`
	GOMAXPROCS          int    `json:"gomaxprocs"`
}

func Default() Config {
	return Config{
		ListenAddr:          "127.0.0.1:18080",
		DataRoot:            "./var/archive",
		RedisAddr:           "127.0.0.1:6379",
		RedisDB:             0,
		RedisNamespace:      "archive:v1",
		SQLitePath:          "./var/archive/archive.db",
		StripeSize:          DefaultStripeSize,
		DataBlocks:          4,
		ParityBlocks:        2,
		WritebackQuotaBytes: DefaultQuotaBytes,
		RepairConcurrency:   2,
		AutoRepair:          true,
		RepairPollMS:        1000,
		RepairDelayMS:       0,
		MaxObjectBytes:      DefaultMaxObjectBytes,
		Timezone:            "Asia/Shanghai",
		GOMAXPROCS:          2,
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("decode config: %w", err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	setStr := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	setInt := func(key string, dst *int) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	setInt64 := func(key string, dst *int64) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*dst = n
			}
		}
	}
	setBool := func(key string, dst *bool) {
		if v := os.Getenv(key); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}
	setStr("ARCHIVE_LISTEN_ADDR", &cfg.ListenAddr)
	setStr("ARCHIVE_DATA_ROOT", &cfg.DataRoot)
	setStr("ARCHIVE_REDIS_ADDR", &cfg.RedisAddr)
	setStr("ARCHIVE_REDIS_PASSWORD", &cfg.RedisPassword)
	setInt("ARCHIVE_REDIS_DB", &cfg.RedisDB)
	setStr("ARCHIVE_REDIS_NAMESPACE", &cfg.RedisNamespace)
	setStr("ARCHIVE_SQLITE_PATH", &cfg.SQLitePath)
	setInt64("ARCHIVE_STRIPE_SIZE", &cfg.StripeSize)
	setInt64("ARCHIVE_WRITEBACK_QUOTA_BYTES", &cfg.WritebackQuotaBytes)
	setInt("ARCHIVE_REPAIR_CONCURRENCY", &cfg.RepairConcurrency)
	setBool("ARCHIVE_AUTO_REPAIR", &cfg.AutoRepair)
	setInt("ARCHIVE_REPAIR_POLL_MS", &cfg.RepairPollMS)
	setInt("ARCHIVE_REPAIR_DELAY_MS", &cfg.RepairDelayMS)
	setInt64("ARCHIVE_MAX_OBJECT_BYTES", &cfg.MaxObjectBytes)
	setStr("ARCHIVE_TIMEZONE", &cfg.Timezone)
	setInt("ARCHIVE_GOMAXPROCS", &cfg.GOMAXPROCS)
}

func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.DataRoot == "" {
		return errors.New("data_root is required")
	}
	if c.RedisAddr == "" {
		return errors.New("redis_addr is required")
	}
	if c.RedisNamespace == "" {
		return errors.New("redis_namespace is required")
	}
	if c.SQLitePath == "" {
		return errors.New("sqlite_path is required")
	}
	if c.DataBlocks != 4 || c.ParityBlocks != 2 {
		return fmt.Errorf("this build requires 4+2 erasure coding, got %d+%d", c.DataBlocks, c.ParityBlocks)
	}
	if c.StripeSize < 4096 || c.StripeSize > 16<<20 {
		return fmt.Errorf("stripe_size must be between 4096 and 16777216")
	}
	if c.StripeSize%int64(c.DataBlocks) != 0 {
		return fmt.Errorf("stripe_size must be divisible by data_blocks")
	}
	if c.WritebackQuotaBytes <= 0 {
		return errors.New("writeback_quota_bytes must be positive")
	}
	if c.RepairConcurrency < 1 || c.RepairConcurrency > 2 {
		return errors.New("repair_concurrency must be 1 or 2")
	}
	if c.MaxObjectBytes <= 0 {
		return errors.New("max_object_bytes must be positive")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	return nil
}

func (c Config) AbsDataRoot() (string, error) {
	return filepath.Abs(c.DataRoot)
}

func (c Config) AbsSQLitePath() (string, error) {
	if filepath.IsAbs(c.SQLitePath) {
		return c.SQLitePath, nil
	}
	return filepath.Abs(c.SQLitePath)
}

func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

func (c Config) Today(now time.Time) string {
	return now.In(c.Location()).Format("2006-01-02")
}

func EnvOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

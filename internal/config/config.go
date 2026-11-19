package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultLeaseTTLMs = 6000
	MinLeaseTTLMs     = 3000
)

type Coordinator struct {
	GRPCAddress           string   `json:"grpc_address"`
	HTTPAddress           string   `json:"http_address"`
	EtcdEndpoints         []string `json:"etcd_endpoints"`
	EtcdDialTimeoutMs     int      `json:"etcd_dial_timeout_ms"`
	KeyPrefix             string   `json:"key_prefix"`
	DefaultLeaseTTLMs     int64    `json:"default_lease_ttl_ms"`
	MinLeaseTTLMs         int64    `json:"min_lease_ttl_ms"`
	DefaultWaitTimeoutMs  int64    `json:"default_wait_timeout_ms"`
	SweepIntervalMs       int      `json:"sweep_interval_ms"`
	QueuePollIntervalMs   int      `json:"queue_poll_interval_ms"`
	BatchCount            int      `json:"batch_count"`
	BatchPrefix           string   `json:"batch_prefix"`
	BatchWidth            int      `json:"batch_width"`
	ReleaseEventListLimit int      `json:"release_event_list_limit"`
}

type Worker struct {
	WorkerID        string `json:"worker_id"`
	GRPCAddress     string `json:"grpc_address"`
	DatabasePath    string `json:"database_path"`
	TasksFile       string `json:"tasks_file"`
	Seed            int64  `json:"seed"`
	BatchCount      int    `json:"batch_count"`
	TasksPerBatch   int    `json:"tasks_per_batch"`
	LeaseTTLMs      int64  `json:"lease_ttl_ms"`
	WaitTimeoutMs   int64  `json:"wait_timeout_ms"`
	RenewIntervalMs int64  `json:"renew_interval_ms"`
	BatchWorkMS     int64  `json:"batch_work_ms"`
	RetryDelayMS    int64  `json:"retry_delay_ms"`
	MaxRuntimeMS    int64  `json:"max_runtime_ms"`
}

func LoadCoordinator(path string) (Coordinator, error) {
	cfg := Coordinator{
		GRPCAddress:           "127.0.0.1:17070",
		HTTPAddress:           "127.0.0.1:19090",
		EtcdEndpoints:         []string{"127.0.0.1:2379"},
		EtcdDialTimeoutMs:     5000,
		KeyPrefix:             "/settlement/v1",
		DefaultLeaseTTLMs:     DefaultLeaseTTLMs,
		MinLeaseTTLMs:         MinLeaseTTLMs,
		DefaultWaitTimeoutMs:  15000,
		SweepIntervalMs:       150,
		QueuePollIntervalMs:   100,
		BatchCount:            20,
		BatchPrefix:           "batch-",
		BatchWidth:            3,
		ReleaseEventListLimit: 100,
	}
	if err := decodeFile(path, &cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Coordinator) Validate() error {
	if c.GRPCAddress == "" || c.HTTPAddress == "" {
		return errors.New("grpc_address and http_address are required")
	}
	if len(c.EtcdEndpoints) == 0 {
		return errors.New("etcd_endpoints is required")
	}
	if c.MinLeaseTTLMs < MinLeaseTTLMs {
		return fmt.Errorf("min_lease_ttl_ms must be >= %d", MinLeaseTTLMs)
	}
	if c.DefaultLeaseTTLMs < c.MinLeaseTTLMs {
		return fmt.Errorf("default_lease_ttl_ms must be >= min_lease_ttl_ms")
	}
	if c.DefaultWaitTimeoutMs < 0 {
		return errors.New("default_wait_timeout_ms must be >= 0")
	}
	if c.KeyPrefix == "" || !strings.HasPrefix(c.KeyPrefix, "/") {
		return errors.New("key_prefix must start with /")
	}
	c.KeyPrefix = strings.TrimRight(c.KeyPrefix, "/")
	if c.BatchCount < 0 || c.BatchWidth < 1 || c.BatchPrefix == "" {
		return errors.New("invalid batch configuration")
	}
	if c.SweepIntervalMs < 20 {
		c.SweepIntervalMs = 20
	}
	if c.QueuePollIntervalMs < 20 {
		c.QueuePollIntervalMs = 20
	}
	if c.ReleaseEventListLimit <= 0 {
		c.ReleaseEventListLimit = 100
	}
	return nil
}

func LoadWorker(path string) (Worker, error) {
	cfg := Worker{
		GRPCAddress:     "127.0.0.1:17070",
		Seed:            28120261119,
		BatchCount:      20,
		TasksPerBatch:   100,
		LeaseTTLMs:      DefaultLeaseTTLMs,
		WaitTimeoutMs:   1500,
		RenewIntervalMs: 2000,
		BatchWorkMS:     250,
		RetryDelayMS:    250,
		MaxRuntimeMS:    110000,
	}
	if err := decodeFile(path, &cfg); err != nil {
		return cfg, err
	}
	cfg.WorkerID = strings.TrimSpace(cfg.WorkerID)
	if cfg.WorkerID == "" {
		return cfg, errors.New("worker_id is required")
	}
	if cfg.LeaseTTLMs < MinLeaseTTLMs {
		return cfg, fmt.Errorf("lease_ttl_ms must be >= %d", MinLeaseTTLMs)
	}
	if cfg.RenewIntervalMs <= 0 || cfg.RenewIntervalMs*2 >= cfg.LeaseTTLMs {
		return cfg, errors.New("renew_interval_ms must be > 0 and less than half lease_ttl_ms")
	}
	if cfg.BatchCount <= 0 || cfg.TasksPerBatch <= 0 {
		return cfg, errors.New("batch_count and tasks_per_batch must be positive")
	}
	base := filepath.Dir(path)
	cfg.DatabasePath = resolvePath(base, cfg.DatabasePath)
	cfg.TasksFile = resolvePath(base, cfg.TasksFile)
	if cfg.DatabasePath == "" {
		return cfg, errors.New("database_path is required")
	}
	return cfg, nil
}

func resolvePath(base, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(filepath.Join(base, value))
}

func decodeFile(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func BatchID(prefix string, width, index int) string {
	return fmt.Sprintf("%s%0*d", prefix, width, index)
}

func ExpectedBatchIDs(c Coordinator) []string {
	ids := make([]string, 0, c.BatchCount)
	for i := 1; i <= c.BatchCount; i++ {
		ids = append(ids, BatchID(c.BatchPrefix, c.BatchWidth, i))
	}
	return ids
}

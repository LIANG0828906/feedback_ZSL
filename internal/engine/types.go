package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"textureworkbench/internal/tex"
)

type BlockState string

const (
	BlockPending    BlockState = "pending"
	BlockProcessing BlockState = "processing"
	BlockCommitted  BlockState = "committed"
	BlockFailed     BlockState = "failed"
)

var legalTransitions = map[BlockState]map[BlockState]bool{
	BlockPending: {
		BlockProcessing: true,
	},
	BlockProcessing: {
		BlockCommitted: true,
		BlockFailed:    true,
	},
	BlockCommitted: {},
	BlockFailed: {
		BlockProcessing: true,
	},
}

func Transition(from, to BlockState) error {
	if legalTransitions[from][to] {
		return nil
	}
	return &tex.Error{
		Code:    tex.CodeState,
		Message: fmt.Sprintf("illegal block transition %s -> %s", from, to),
		Details: map[string]string{"from": string(from), "to": string(to)},
	}
}

type Block struct {
	Index              int        `json:"index"`
	Row                int        `json:"row"`
	Col                int        `json:"col"`
	Rect               tex.Rect   `json:"rect"`
	State              BlockState `json:"state"`
	Attempts           int        `json:"attempts"`
	Retries            int        `json:"retries"`
	DurationMS         float64    `json:"duration_ms"`
	CacheHit           bool       `json:"cache_hit"`
	Invalidated        bool       `json:"invalidated"`
	InvalidationReason string     `json:"invalidation_reason,omitempty"`
	Failure            string     `json:"failure,omitempty"`
	OutputHash         string     `json:"output_hash,omitempty"`
	ErrorHash          string     `json:"error_hash,omitempty"`
	DescriptorVersion  string     `json:"descriptor_version"`
	ParameterVersion   string     `json:"parameter_version"`
	AlgorithmVersion   string     `json:"algorithm_version"`
	Output             []uint16   `json:"-"`
	Errors             []int32    `json:"-"`
}

type Config struct {
	BlockWidth            int    `json:"block_width"`
	BlockHeight           int    `json:"block_height"`
	MaxWorkers            int    `json:"max_workers"`
	BufferPixelsPerWorker int    `json:"buffer_pixels_per_worker"`
	QueueDepth            int    `json:"queue_depth"`
	ParameterVersion      string `json:"parameter_version"`
	SessionKey            string `json:"session_key"`
	CheckpointRoot        string `json:"checkpoint_root"`
}

func (c Config) Normalize() Config {
	if c.BlockWidth == 0 {
		c.BlockWidth = 32
	}
	if c.BlockHeight == 0 {
		c.BlockHeight = 32
	}
	if c.MaxWorkers == 0 {
		c.MaxWorkers = 4
	}
	if c.BufferPixelsPerWorker == 0 {
		c.BufferPixelsPerWorker = c.BlockWidth * c.BlockHeight
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = 2
	}
	if c.ParameterVersion == "" {
		c.ParameterVersion = "p1"
	}
	if c.CheckpointRoot == "" {
		c.CheckpointRoot = "data/checkpoints"
	}
	return c
}

func (c Config) Validate() error {
	if c.BlockWidth <= 0 || c.BlockHeight <= 0 {
		return tex.ErrPara("block_width", "block dimensions must be positive")
	}
	if c.BlockWidth > 4096 || c.BlockHeight > 4096 {
		return tex.ErrPara("block_width", "block dimensions exceed 4096")
	}
	if c.MaxWorkers <= 0 || c.MaxWorkers > 64 {
		return tex.ErrPara("max_workers", "max_workers must be in [1,64]")
	}
	blockPixels := c.BlockWidth * c.BlockHeight
	if c.BufferPixelsPerWorker < blockPixels {
		return tex.ErrPara("buffer_pixels_per_worker", fmt.Sprintf("buffer must hold one block (%d pixels)", blockPixels))
	}
	if c.BufferPixelsPerWorker > 64*1024*1024 {
		return tex.ErrPara("buffer_pixels_per_worker", "buffer_pixels_per_worker exceeds the 64M pixel safety limit")
	}
	if c.QueueDepth <= 0 || c.QueueDepth > 1024 {
		return tex.ErrPara("queue_depth", "queue_depth must be in [1,1024]")
	}
	if strings.TrimSpace(c.ParameterVersion) == "" {
		return tex.ErrPara("parameter_version", "required field is missing")
	}
	if strings.TrimSpace(c.CheckpointRoot) == "" {
		return tex.ErrPara("checkpoint_root", "required field is missing")
	}
	return nil
}

type PathStats struct {
	Path              string  `json:"path"`
	BlockRecomputes   int64   `json:"block_recomputes"`
	SuccessfulCommits int64   `json:"successful_commits"`
	CacheHits         int64   `json:"cache_hits"`
	FailedAttempts    int64   `json:"failed_attempts"`
	Retries           int64   `json:"retries"`
	BufferQueueWaits  int64   `json:"buffer_queue_waits"`
	BufferWaitReason  string  `json:"buffer_wait_reason,omitempty"`
	ConversionTimeMS  float64 `json:"conversion_time_ms"`
	CheckpointTimeMS  float64 `json:"checkpoint_time_ms"`
	TotalTimeMS       float64 `json:"total_time_ms"`
	HistoryReset      bool    `json:"history_reset"`
}

type RestoreInfo struct {
	Attempted         bool     `json:"attempted"`
	Used              bool     `json:"used"`
	Invalidated       bool     `json:"invalidated"`
	Reasons           []string `json:"reasons,omitempty"`
	RestoredBlocks    int      `json:"restored_blocks"`
	PersistedSequence int      `json:"persisted_sequence"`
}

type InvalidatedBlock struct {
	Index             int        `json:"index"`
	Rect              tex.Rect   `json:"rect"`
	State             BlockState `json:"state"`
	Reason            string     `json:"reason"`
	DescriptorVersion string     `json:"descriptor_version"`
	ParameterVersion  string     `json:"parameter_version"`
	OutputHash        string     `json:"output_hash,omitempty"`
}

type GenerationInfo struct {
	DescriptorVersion string             `json:"descriptor_version"`
	ParameterVersion  string             `json:"parameter_version"`
	AlgorithmVersion  string             `json:"algorithm_version"`
	InvalidatedAt     string             `json:"invalidated_at,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	Blocks            []InvalidatedBlock `json:"blocks,omitempty"`
}

type SessionSnapshot struct {
	SessionID         string           `json:"session_id"`
	ImageID           string           `json:"image_id"`
	ImagePath         string           `json:"image_path"`
	Width             int              `json:"width"`
	Height            int              `json:"height"`
	SourceBitDepth    int              `json:"source_bit_depth"`
	SourceLayout      tex.Layout       `json:"source_layout"`
	Descriptor        tex.Descriptor   `json:"descriptor"`
	DescriptorVersion string           `json:"descriptor_version"`
	ParameterVersion  string           `json:"parameter_version"`
	AlgorithmVersion  string           `json:"algorithm_version"`
	ContentHash       string           `json:"content_hash"`
	Config            Config           `json:"config"`
	Blocks            []Block          `json:"blocks"`
	Progress          Progress         `json:"progress"`
	Running           bool             `json:"running"`
	Paused            bool             `json:"paused"`
	Cancelled         bool             `json:"cancelled"`
	Stats             PathStats        `json:"stats"`
	Restore           RestoreInfo      `json:"restore"`
	Generations       []GenerationInfo `json:"generations"`
	Mapping           tex.Mapping      `json:"mapping"`
	Histogram         *tex.Histogram   `json:"histogram,omitempty"`
	QuantStats        *tex.QuantStats  `json:"quant_stats,omitempty"`
	Complete          bool             `json:"complete"`
	ExportReady       bool             `json:"export_ready"`
}

type Progress struct {
	Committed  int `json:"committed"`
	Total      int `json:"total"`
	Failed     int `json:"failed"`
	Processing int `json:"processing"`
	Pending    int `json:"pending"`
}

func hashStringParts(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func jsonHash(value any) string {
	data, _ := json.Marshal(value)
	return tex.HashBytes(data)
}

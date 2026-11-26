package engine

import (
	"context"
	"sync"
	"time"

	"maskqc/checkpoint"
	"maskqc/model"
	"maskqc/topology"
)

type Mode string

const (
	ModeIncremental Mode = "incremental"
	ModeResume      Mode = "resume"
	ModeFull        Mode = "full"
)

type Config struct {
	BlockSize            int
	MaxParallel          int
	PixelBufferPerWorker int
	QueueCapacity        int
	Tolerance            topology.Tolerance
	ResidualDigest       string
}

func DefaultConfig() Config {
	return Config{
		BlockSize:            16,
		MaxParallel:          4,
		PixelBufferPerWorker: 16 * 16 * 8,
		QueueCapacity:        8,
		Tolerance:            topology.DefaultTolerance(),
		ResidualDigest:       model.HashStrings("residual", model.ConversionVersion, "default"),
	}
}

func (c Config) normalized() (Config, error) {
	if c.BlockSize <= 0 {
		c.BlockSize = 16
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = 4
	}
	if c.PixelBufferPerWorker <= 0 {
		c.PixelBufferPerWorker = c.BlockSize * c.BlockSize * 8
	}
	if c.QueueCapacity <= 0 {
		c.QueueCapacity = 8
	}
	if c.Tolerance == (topology.Tolerance{}) {
		c.Tolerance = topology.DefaultTolerance()
	}
	if c.ResidualDigest == "" {
		c.ResidualDigest = model.HashStrings("residual", model.ConversionVersion, "default")
	}
	if c.BlockSize > 256 {
		return c, &model.Error{
			Code: model.CodeParam, Path: "config.blockSize", Field: "blockSize",
			Message: "block size is outside the supported range", Allowed: "1..256",
		}
	}
	if c.MaxParallel > 256 {
		return c, &model.Error{
			Code: model.CodeParam, Path: "config.maxParallel", Field: "maxParallel",
			Message: "maximum parallel degree is outside the supported range", Allowed: "1..256",
		}
	}
	blockPixels := c.BlockSize * c.BlockSize
	if c.PixelBufferPerWorker < blockPixels {
		return c, &model.Error{
			Code: model.CodeParam, Path: "config.pixelBufferPerWorker", Field: "pixelBufferPerWorker",
			Message: "per-worker pixel buffer is smaller than one block", Allowed: ">=blockSize*blockSize",
		}
	}
	if c.QueueCapacity > 1<<20 {
		return c, &model.Error{
			Code: model.CodeParam, Path: "config.queueCapacity", Field: "queueCapacity",
			Message: "queue capacity is too large", Allowed: "1..1048576",
		}
	}
	return c, nil
}

type Participant struct {
	StrokeID       string `json:"strokeId"`
	Seq            int    `json:"seq"`
	Priority       int    `json:"priority"`
	CompositeOrder int    `json:"compositeOrder"`
	ComponentKey   string `json:"componentKey"`
	Basis          string `json:"basis"`
}

type BlockComponent struct {
	StrokeID        string `json:"strokeId"`
	BlockIndex      int    `json:"blockIndex"`
	BlockX          int    `json:"blockX"`
	BlockY          int    `json:"blockY"`
	CacheKey        string `json:"cacheKey"`
	ContentHash     string `json:"contentHash"`
	ComponentDigest string `json:"componentDigest"`
	Coverage        []byte `json:"-"`
}

type BlockResult struct {
	Index           int              `json:"index"`
	BlockX          int              `json:"blockX"`
	BlockY          int              `json:"blockY"`
	Width           int              `json:"width"`
	Height          int              `json:"height"`
	Rect            model.Rect       `json:"rect"`
	ContentHash     string           `json:"contentHash"`
	CacheKey        string           `json:"cacheKey"`
	OrderVersion    string           `json:"orderVersion"`
	CompositeDigest string           `json:"compositeDigest"`
	Mask            []byte           `json:"-"`
	StrokeIDs       []string         `json:"strokeIds"`
	Participants    []Participant    `json:"participants"`
	Components      []BlockComponent `json:"components"`
	Reused          bool             `json:"reused"`
	QueueWaitMicros int64            `json:"queueWaitMicros"`
	PixelWaitMicros int64            `json:"pixelWaitMicros"`
	CommitSequence  int              `json:"commitSequence"`
}

type QueueEvent struct {
	BlockIndex int    `json:"blockIndex"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
	WaitMicros int64  `json:"waitMicros"`
}

type BudgetReport struct {
	MaxParallel          int          `json:"maxParallel"`
	PixelBufferPerWorker int          `json:"pixelBufferPerWorker"`
	QueueCapacity        int          `json:"queueCapacity"`
	MaxObservedParallel  int          `json:"maxObservedParallel"`
	MaxObservedPixels    int          `json:"maxObservedPixels"`
	Events               []QueueEvent `json:"events"`
}

type GraphNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	ObjectID string `json:"objectId,omitempty"`
	CacheKey string `json:"cacheKey,omitempty"`
}

type GraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type InvalidationReport struct {
	StrokeID          string   `json:"strokeId"`
	OldSignature      string   `json:"oldSignature"`
	NewSignature      string   `json:"newSignature"`
	AffectedStrokes   []string `json:"affectedStrokes"`
	InvalidatedBlocks []int    `json:"invalidatedBlocks"`
	ReusedBlocks      []int    `json:"reusedBlocks"`
	Downstream        []string `json:"downstream"`
	Reasons           []string `json:"reasons"`
}

type RunResult struct {
	Mode               Mode                `json:"mode"`
	ProjectID          string              `json:"projectId"`
	Width              int                 `json:"width"`
	Height             int                 `json:"height"`
	CandidateHash      string              `json:"candidateHash"`
	AccumulatedHash    string              `json:"accumulatedHash"`
	Accumulated        []byte              `json:"-"`
	Analysis           *topology.Analysis  `json:"analysis"`
	Issues             model.Errors        `json:"issues"`
	Blocks             []BlockResult       `json:"blocks"`
	Order              []string            `json:"order"`
	OrderVersion       string              `json:"orderVersion"`
	CommitSequence     []int               `json:"commitSequence"`
	ReusedBlocks       []int               `json:"reusedBlocks"`
	InvalidatedBlocks  []int               `json:"invalidatedBlocks"`
	Budget             BudgetReport        `json:"budget"`
	Graph              DependencyGraph     `json:"graph"`
	Invalidation       *InvalidationReport `json:"invalidation,omitempty"`
	CheckpointInvalid  bool                `json:"checkpointInvalid"`
	CheckpointReason   string              `json:"checkpointReason,omitempty"`
	ConversionResidual string              `json:"conversionResidualDigest"`
	StrokeFailures     []string            `json:"strokeFailures,omitempty"`
	ElapsedMillis      int64               `json:"elapsedMillis"`
}

type Mismatch struct {
	BlockIndex int
	X          int
	Y          int
	Reason     string
}

type blockJob struct {
	index    int
	rect     model.Rect
	width    int
	height   int
	area     int
	enqueued time.Time
}

type blockOutcome struct {
	result *BlockResult
	err    error
}

type strokeFailure struct {
	strokeID string
	err      error
}

type schedulerResult struct {
	blocks   map[int]*BlockResult
	failures map[string]error
	budget   BudgetReport
}

type engineState struct {
	mu       sync.Mutex
	last     *RunResult
	failHook func(model.Stroke) error
}

type runContext struct {
	engine  *Engine
	ctx     context.Context
	mode    Mode
	stamp   checkpoint.Stamp
	restore *checkpoint.RestoreResult
}

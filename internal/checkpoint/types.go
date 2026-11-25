package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"qualitybench/internal/core"
)

type RunMeta struct {
	RunID              string `json:"run_id"`
	ImageID            string `json:"image_id"`
	AlgorithmVersion   string `json:"algorithm_version"`
	ChainFingerprint   string `json:"chain_fingerprint"`
	ProjectFingerprint string `json:"project_fingerprint"`
	TruthFingerprint   string `json:"truth_fingerprint"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	Channels           int    `json:"channels"`
	BlockSize          int    `json:"block_size"`
	StepCount          int    `json:"step_count"`
	Status             string `json:"status"`
}

type Artifact struct {
	RunID            string     `json:"run_id"`
	ImageID          string     `json:"image_id"`
	Step             int        `json:"step"`
	Block            core.Rect  `json:"block"`
	State            core.State `json:"state"`
	CacheKey         string     `json:"cache_key"`
	InputHash        string     `json:"input_hash"`
	OutputHash       string     `json:"output_hash"`
	OutputDigest     string     `json:"output_digest"`
	ChainFingerprint string     `json:"chain_fingerprint"`
	AlgorithmVersion string     `json:"algorithm_version"`
	Pixels           []byte     `json:"-"`
	CommitSeq        int64      `json:"commit_seq"`
}

type StepSummary struct {
	Step         int    `json:"step"`
	CacheKey     string `json:"cache_key"`
	InputHash    string `json:"input_hash"`
	OutputHash   string `json:"output_hash"`
	OutputDigest string `json:"output_digest"`
}

type BlockManifest struct {
	RunID            string        `json:"run_id"`
	ImageID          string        `json:"image_id"`
	Block            core.Rect     `json:"block"`
	ChainFingerprint string        `json:"chain_fingerprint"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Steps            []StepSummary `json:"steps"`
	ContentHash      string        `json:"content_hash"`
	CommitSeq        int64         `json:"commit_seq"`
}

func (m BlockManifest) ComputeContentHash() string {
	data, _ := json.Marshal(struct {
		ImageID          string        `json:"image_id"`
		Block            core.Rect     `json:"block"`
		ChainFingerprint string        `json:"chain_fingerprint"`
		AlgorithmVersion string        `json:"algorithm_version"`
		Steps            []StepSummary `json:"steps"`
	}{
		ImageID: m.ImageID, Block: m.Block, ChainFingerprint: m.ChainFingerprint,
		AlgorithmVersion: m.AlgorithmVersion, Steps: m.Steps,
	})
	return core.HashParts("manifest-v1", string(data))
}

func (a Artifact) ComputeOutputDigest() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v2|%s|%d|%d,%d,%d,%d|%s|%s|%s|%s|%s|",
		a.ImageID, a.Step,
		a.Block.X, a.Block.Y, a.Block.W, a.Block.H,
		a.CacheKey, a.InputHash, a.OutputHash, a.ChainFingerprint, a.AlgorithmVersion)
	_, _ = h.Write(a.Pixels)
	return hex.EncodeToString(h.Sum(nil))
}

func PixelHash(pixels []byte) string {
	return core.HashParts("pixels", string(pixels))
}

func (a Artifact) Validate() error {
	if a.Step < 0 {
		return fmt.Errorf("negative step")
	}
	if a.Block.Empty() {
		return fmt.Errorf("empty block")
	}
	if a.OutputHash == "" {
		return fmt.Errorf("missing output hash")
	}
	if a.OutputDigest == "" {
		return fmt.Errorf("missing output digest")
	}
	if a.OutputDigest != a.ComputeOutputDigest() {
		return fmt.Errorf("output digest mismatch")
	}
	if a.CacheKey == "" {
		return fmt.Errorf("missing cache key")
	}
	if a.AlgorithmVersion == "" {
		return fmt.Errorf("missing algorithm version")
	}
	return nil
}

type Store interface {
	EnsureRun(ctx context.Context, meta RunMeta, reset bool) error
	GetRunMeta(ctx context.Context, runID, imageID string) (RunMeta, bool, error)
	SetRunStatus(ctx context.Context, runID, imageID, status string) error
	CommitArtifact(ctx context.Context, artifact Artifact) (Artifact, error)
	CommitBlockManifest(ctx context.Context, manifest BlockManifest) (BlockManifest, error)
	LoadArtifacts(ctx context.Context, runID, imageID string) ([]Artifact, error)
	LoadBlockManifests(ctx context.Context, runID, imageID string) ([]BlockManifest, error)
	LookupCache(ctx context.Context, cacheKey, algorithmVersion string) (Artifact, bool, error)
	InvalidateRun(ctx context.Context, runID, imageID, reason string) error
	ListImages(ctx context.Context, runID string) ([]string, error)
	Close() error
}

func BlockIndex(width, blockSize, x, y int) int {
	cols := width / blockSize
	return (y/blockSize)*cols + x/blockSize
}

func Grid(width, height, blockSize int) []core.Rect {
	var blocks []core.Rect
	for y := 0; y < height; y += blockSize {
		for x := 0; x < width; x += blockSize {
			blocks = append(blocks, core.Rect{X: x, Y: y, W: blockSize, H: blockSize})
		}
	}
	return blocks
}

func BlockRef(index int, rect core.Rect) core.BlockRef {
	return core.BlockRef{Index: index, X: rect.X, Y: rect.Y, W: rect.W, H: rect.H}
}

func BlockKey(step int, rect core.Rect) string {
	return strconv.Itoa(step) + "|" + strconv.Itoa(rect.X) + "," + strconv.Itoa(rect.Y)
}

package runner

import (
	"context"
	"fmt"
	"time"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
	"qualitybench/internal/dataset"
	"qualitybench/internal/pipeline"
)

type Config struct {
	RunID                string
	Mode                 pipeline.Mode
	Workers              int
	MaxThreadBufferBytes int64
	BufferCapacityBytes  int64
	PersistentCache      bool
	StopAfter            int
	TaskDelay            time.Duration
	Fault                func(step int, block core.Rect) error
	OnStepComplete       func(imageID string, step int, frame *core.Frame)
	OnProgress           func(imageID string, completed, total int)
}

func NewEngine(ds *dataset.Dataset, imageID string, store checkpoint.Store, cfg Config) (*pipeline.Engine, error) {
	spec, ok := ds.Project.Image(imageID)
	if !ok {
		return nil, core.Schema("image_id", "unknown image %q", imageID)
	}
	chain, ok := ds.Project.Chain(imageID)
	if !ok {
		return nil, core.Schema("chain", "no chain for image %q", imageID)
	}
	base, ok := ds.Baseline[imageID]
	if !ok {
		return nil, core.Schema("baseline", "no baseline for image %q", imageID)
	}
	return pipeline.New(base, chain, pipeline.Options{
		RunID:                cfg.RunID,
		ImageID:              imageID,
		ImageSeed:            spec.Seed,
		ProjectFingerprint:   ds.ProjectFP,
		TruthFingerprint:     ds.TruthFP,
		AlgorithmVersion:     ds.Project.AlgorithmVersion,
		BlockSize:            ds.Project.BlockSize,
		Workers:              cfg.Workers,
		MaxThreadBufferBytes: cfg.MaxThreadBufferBytes,
		BufferCapacityBytes:  cfg.BufferCapacityBytes,
		PersistentCache:      cfg.PersistentCache,
		StopAfter:            cfg.StopAfter,
		TaskDelay:            cfg.TaskDelay,
		AttributionShare:     ds.Project.AttributionShare,
		Fault:                cfg.Fault,
		Store:                store,
		OnStepComplete: func(step int, frame *core.Frame) {
			if cfg.OnStepComplete != nil {
				cfg.OnStepComplete(imageID, step, frame)
			}
		},
		OnProgress: func(completed, total int) {
			if cfg.OnProgress != nil {
				cfg.OnProgress(imageID, completed, total)
			}
		},
	})
}

func RunImages(ctx context.Context, ds *dataset.Dataset, store checkpoint.Store, cfg Config,
	imageIDs []string) ([]*pipeline.Result, error) {
	if len(imageIDs) == 0 {
		for _, image := range ds.Project.Images {
			imageIDs = append(imageIDs, image.ID)
		}
	}
	results := make([]*pipeline.Result, 0, len(imageIDs))
	for _, imageID := range imageIDs {
		eng, err := NewEngine(ds, imageID, store, cfg)
		if err != nil {
			return results, err
		}
		result, err := eng.Run(ctx, cfg.Mode)
		if result != nil {
			results = append(results, result)
		}
		if err != nil {
			return results, fmt.Errorf("image %s: %w", imageID, err)
		}
	}
	return results, nil
}

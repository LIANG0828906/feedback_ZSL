package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"iconbench/internal/checkpoint"
	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/sample"
	"iconbench/internal/segment"
)

var ErrInterrupted = errors.New("pipeline interrupted after checkpoint budget")

type Pipeline struct {
	Manifest *model.Manifest
	DataDir  string
	Store    *checkpoint.Store
	Pool     *ResourcePool
}

type RunOptions struct {
	MaxCommits int
}

type ImageResult struct {
	Record            model.ImageManifest       `json:"record"`
	Params            model.Params              `json:"params"`
	Width             int                       `json:"width"`
	Height            int                       `json:"height"`
	Mask              []byte                    `json:"-"`
	Labels            []int32                   `json:"-"`
	Stats             []model.RegionStat        `json:"stats"`
	TruthMask         []byte                    `json:"-"`
	TruthLabels       []int32                   `json:"-"`
	TruthStats        []model.RegionStat        `json:"truth_stats"`
	Report            *imaging.RegionReport     `json:"report"`
	Metrics           model.DivergenceMetrics   `json:"metrics"`
	MaskHash          string                    `json:"mask_hash"`
	Sequence          []checkpoint.CommitRecord `json:"sequence"`
	Queue             []model.QueueRecord       `json:"queue_records"`
	SnapshotReset     bool                      `json:"snapshot_reset"`
	SnapshotViolation *checkpoint.Violation     `json:"snapshot_violation,omitempty"`
}

type PersistedResult struct {
	ImageID      string                  `json:"image_id"`
	ParamVersion int64                   `json:"param_version"`
	MaskHash     string                  `json:"mask_hash"`
	Stats        []model.RegionStat      `json:"stats"`
	Report       *imaging.RegionReport   `json:"report"`
	Metrics      model.DivergenceMetrics `json:"metrics"`
}

func (p *Pipeline) RunImage(ctx context.Context, imageID string, params model.Params, opts RunOptions) (*ImageResult, error) {
	if err := model.ValidateParams(params); err != nil {
		return nil, err
	}
	rec, err := p.findImage(imageID)
	if err != nil {
		return nil, err
	}
	img, _, err := sample.LoadImage(p.DataDir, rec)
	if err != nil {
		return nil, err
	}
	rgb := imaging.ToRGB(img)
	truth, tw, th, err := sample.DecodeReferenceMask(rec)
	if err != nil {
		return nil, err
	}
	if tw != rgb.Width || th != rgb.Height {
		return nil, &model.Error{Code: model.CodeMeta, Field: "reference_mask", Expected: fmt.Sprintf("%dx%d", rgb.Width, rgb.Height), Actual: fmt.Sprintf("%dx%d", tw, th)}
	}
	truthLabels, truthStats := imaging.ConnectedComponents(truth, tw, th)
	blocks := BuildBlocks(rgb.Width, rgb.Height, p.Manifest.Budget.TileSize)

	var violation *checkpoint.Violation
	reset := false
	committed := map[string]checkpoint.LayerRecord{}
	if p.Store != nil {
		v, err := p.Store.ValidateSnapshot(ctx, imageID, params)
		if err != nil {
			return nil, err
		}
		if v != nil {
			violation = v
			reset = true
			if err := p.Store.ResetImage(ctx, imageID); err != nil {
				return nil, err
			}
		}
		if err := p.Store.InitImage(imageID, len(blocks), p.Manifest.Budget.TileSize, rgb.Width, rgb.Height, params); err != nil {
			return nil, err
		}
		if _, err := p.Store.ResetInterrupted(ctx, imageID); err != nil {
			return nil, err
		}
		committed, err = p.Store.CommittedLayers(ctx, imageID)
		if err != nil {
			return nil, err
		}
	}

	arts := make([][]*Artifact, len(checkpoint.Layers))
	layerIndex := map[string]int{}
	for i, layer := range checkpoint.Layers {
		arts[i] = make([]*Artifact, len(blocks))
		layerIndex[layer] = i
	}
	commits := 0

	computeGradient := func(b Block) (*Artifact, error) {
		crop := CropRGB(rgb, b)
		smoothed := imaging.GaussianBlurRGB(crop, params.SmoothKernel)
		grad := imaging.GradientMagnitude(smoothed, params.GradientOperator)
		return &Artifact{Gradient: grad}, nil
	}
	if err := p.processLayer(ctx, rec.ID, params, blocks, checkpoint.LayerGradient, arts[layerIndex[checkpoint.LayerGradient]], committed, opts.MaxCommits, &commits, computeGradient); err != nil {
		return &ImageResult{SnapshotReset: reset, SnapshotViolation: violation}, err
	}

	computeMarkers := func(b Block) (*Artifact, error) {
		crop := CropRGB(rgb, b)
		mr := segment.BuildMarkers(crop, params)
		return &Artifact{Markers: &mr}, nil
	}
	if err := p.processLayer(ctx, rec.ID, params, blocks, checkpoint.LayerMarkers, arts[layerIndex[checkpoint.LayerMarkers]], committed, opts.MaxCommits, &commits, computeMarkers); err != nil {
		return &ImageResult{SnapshotReset: reset, SnapshotViolation: violation}, err
	}

	computeMerge := func(b Block) (*Artifact, error) {
		gradArt := arts[layerIndex[checkpoint.LayerGradient]][b.Index]
		markerArt := arts[layerIndex[checkpoint.LayerMarkers]][b.Index]
		if gradArt == nil || markerArt == nil || markerArt.Markers == nil {
			return nil, &model.Error{Code: model.CodeSnapshot, Layer: checkpoint.LayerFloodMerge, BlockX: b.X, BlockY: b.Y, Field: "dependency", Message: "missing gradient or marker artifact"}
		}
		mr := *markerArt.Markers
		samples, initial := segment.Flood(gradArt.Gradient, mr, b.Width, b.Height, params.FloodThreshold)
		merged, err := segment.MergeRegions(initial, gradArt.Gradient, mr, b.Width, b.Height, params, 0)
		if err != nil {
			return nil, err
		}
		raw := segment.BuildRawMask(initial, merged, mr)
		return &Artifact{
			FloodSamples:     samples,
			QueueSampleRatio: float64(samples) / float64(b.Width*b.Height),
			InitialLabels:    initial,
			Labels:           merged.Labels,
			Regions:          merged.Regions,
			Edges:            merged.Edges,
			MergeLog:         merged.MergeLog,
			RawMask:          raw,
		}, nil
	}
	if err := p.processLayer(ctx, rec.ID, params, blocks, checkpoint.LayerFloodMerge, arts[layerIndex[checkpoint.LayerFloodMerge]], committed, opts.MaxCommits, &commits, computeMerge); err != nil {
		return &ImageResult{SnapshotReset: reset, SnapshotViolation: violation}, err
	}

	rawMask := assembleMask(arts[layerIndex[checkpoint.LayerFloodMerge]], blocks, rgb.Width, rgb.Height, func(a *Artifact) []byte { return a.RawMask })
	cleanMask := imaging.OpenClose(rawMask, rgb.Width, rgb.Height, params.MorphRadius)

	computeStats := func(b Block) (*Artifact, error) {
		tileMask := sliceMask(cleanMask, rgb.Width, b)
		hash := HashBytes(tileMask)
		_, localStats := imaging.ConnectedComponents(tileMask, b.Width, b.Height)
		art := &Artifact{Stats: &BlockStats{CleanMask: tileMask, PixelCount: countNonZero(tileMask), MaskHash: hash, LocalStats: localStats}}
		if mergeArt := arts[layerIndex[checkpoint.LayerFloodMerge]][b.Index]; mergeArt != nil {
			art.Edges = mergeArt.Edges
		}
		return art, nil
	}
	if err := p.processLayer(ctx, rec.ID, params, blocks, checkpoint.LayerStats, arts[layerIndex[checkpoint.LayerStats]], committed, opts.MaxCommits, &commits, computeStats); err != nil {
		return &ImageResult{SnapshotReset: reset, SnapshotViolation: violation}, err
	}

	labels, stats := imaging.ConnectedComponents(cleanMask, rgb.Width, rgb.Height)
	report := imaging.MatchRegions(rec.ID, stats, truthStats, labels, truthLabels, rgb.Width, rgb.Height, p.Manifest.Tolerances)
	metrics := imaging.MaskMetrics(cleanMask, truth)
	metrics.RegionCountExpected = len(truthStats)
	metrics.RegionCountActual = len(stats)
	metrics.RegionCountDelta = len(stats) - len(truthStats)
	metrics.MaxCentroidErrorPx = report.MaxCentroidErrorPx
	metrics.MaxAreaRelativeError = report.MaxAreaRelativeError
	maskHash := HashBytes(cleanMask)

	var sequence []checkpoint.CommitRecord
	if p.Store != nil {
		sequence, err = p.Store.Sequence(ctx, rec.ID)
		if err != nil {
			return nil, err
		}
		snap := PersistedResult{
			ImageID: rec.ID, ParamVersion: params.ParamVersion, MaskHash: maskHash,
			Stats: stats, Report: report, Metrics: metrics,
		}
		b, _ := json.Marshal(snap)
		if err := p.Store.SaveResult(ctx, rec.ID, params.ParamVersion, maskHash, string(b)); err != nil {
			return nil, err
		}
	}

	queue := []model.QueueRecord{}
	if p.Pool != nil {
		queue = p.Pool.QueueRecords()
	}
	return &ImageResult{
		Record: rec, Params: params, Width: rgb.Width, Height: rgb.Height,
		Mask: cleanMask, Labels: labels, Stats: stats,
		TruthMask: truth, TruthLabels: truthLabels, TruthStats: truthStats,
		Report: report, Metrics: metrics, MaskHash: maskHash,
		Sequence: sequence, Queue: queue,
		SnapshotReset: reset, SnapshotViolation: violation,
	}, nil
}

func (p *Pipeline) findImage(id string) (model.ImageManifest, error) {
	for _, rec := range p.Manifest.Images {
		if rec.ID == id {
			return rec, nil
		}
	}
	return model.ImageManifest{}, &model.Error{Code: model.CodeNotFound, Message: "image not found: " + id}
}

func (p *Pipeline) processLayer(
	ctx context.Context,
	imageID string,
	params model.Params,
	blocks []Block,
	layer string,
	results []*Artifact,
	committed map[string]checkpoint.LayerRecord,
	maxCommits int,
	commitCount *int,
	compute func(Block) (*Artifact, error),
) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(blocks))

	for _, b := range blocks {
		key := checkpoint.Key(b.Index, layer)
		if rec, ok := committed[key]; ok {
			var art Artifact
			if err := json.Unmarshal([]byte(rec.ArtifactJSON), &art); err != nil {
				return &model.Error{Code: model.CodeSnapshot, Layer: layer, BlockX: b.X, BlockY: b.Y, Field: "artifact_json", Message: err.Error()}
			}
			results[b.Index] = &art
			continue
		}

		wg.Add(1)
		go func(b Block) {
			defer wg.Done()
			pixels := b.Width * b.Height
			qRec := model.QueueRecord{BlockIndex: b.Index, Layer: layer}
			err := p.Pool.Run(ctx, qRec, pixels, func() error {
				if p.Store != nil {
					state, stateErr := p.Store.GetState(imageID, b.Index, layer)
					if stateErr != nil {
						return stateErr
					}
					if state == checkpoint.StateFailed {
						if err := p.Store.Transition(ctx, imageID, b.Index, layer, checkpoint.StateFailed, checkpoint.StatePending); err != nil {
							return err
						}
					}
					if err := p.Store.Transition(ctx, imageID, b.Index, layer, checkpoint.StatePending, checkpoint.StateProcessing); err != nil {
						return err
					}
				}
				art, err := compute(b)
				if err != nil {
					if p.Store != nil {
						_ = p.Store.FailLayer(ctx, imageID, b.Index, layer, err.Error())
					}
					return err
				}
				art.SchemaVersion = model.SchemaVersion
				art.AlgorithmVersion = model.AlgorithmVersion
				art.ImageID = imageID
				art.Block = b
				art.Layer = layer
				art.Params = params
				results[b.Index] = art
				return nil
			})
			if err != nil {
				errCh <- err
			}
		}(b)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	for _, b := range blocks {
		key := checkpoint.Key(b.Index, layer)
		if _, ok := committed[key]; ok {
			continue
		}
		art := results[b.Index]
		if art == nil {
			return &model.Error{Code: model.CodeSnapshot, Layer: layer, BlockX: b.X, BlockY: b.Y, Field: "artifact", Message: "missing computed artifact"}
		}
		rawJSON, err := json.Marshal(art)
		if err != nil {
			return err
		}
		hash := HashBytes(rawJSON)
		summary := summaryJSON(layer, art)
		rag := ""
		if len(art.Edges) > 0 {
			b, _ := json.Marshal(art.Edges)
			rag = string(b)
		}
		if p.Store != nil {
			rec := checkpoint.LayerRecord{
				ImageID: imageID, BlockIndex: b.Index, BlockX: b.X, BlockY: b.Y, Layer: layer,
				ArtifactJSON: string(rawJSON), SummaryJSON: summary, RAGJSON: rag, ContentHash: hash,
				ParamVersion: params.ParamVersion, ParamHash: params.CanonicalHash(),
				AlgorithmVersion: model.AlgorithmVersion, SchemaVersion: model.SchemaVersion,
			}
			if err := p.Store.CommitLayer(ctx, rec); err != nil {
				return err
			}
			*commitCount++
			if maxCommits > 0 && *commitCount >= maxCommits {
				return ErrInterrupted
			}
		}
	}
	return nil
}

func assembleMask(arts []*Artifact, blocks []Block, width, height int, pick func(*Artifact) []byte) []byte {
	out := make([]byte, width*height)
	for _, b := range blocks {
		a := arts[b.Index]
		if a == nil {
			continue
		}
		tile := pick(a)
		for y := 0; y < b.Height; y++ {
			for x := 0; x < b.Width; x++ {
				v := tile[y*b.Width+x]
				out[(b.Y0+y)*width+(b.X0+x)] = v
			}
		}
	}
	return out
}

func sliceMask(mask []byte, width int, b Block) []byte {
	out := make([]byte, b.Width*b.Height)
	for y := 0; y < b.Height; y++ {
		srcStart := (b.Y0+y)*width + b.X0
		dstStart := y * b.Width
		copy(out[dstStart:dstStart+b.Width], mask[srcStart:srcStart+b.Width])
	}
	return out
}

func countNonZero(m []byte) int {
	n := 0
	for _, v := range m {
		if v != 0 {
			n++
		}
	}
	return n
}

func summaryJSON(layer string, a *Artifact) string {
	summary := map[string]any{"layer": layer}
	switch layer {
	case checkpoint.LayerGradient:
		summary["samples"] = len(a.Gradient)
	case checkpoint.LayerMarkers:
		if a.Markers != nil {
			summary["markers"] = len(a.Markers.Markers)
			summary["fg_threshold"] = a.Markers.FGThreshold
			summary["otsu_threshold"] = a.Markers.OtsuThreshold
		}
	case checkpoint.LayerFloodMerge:
		summary["flood_samples"] = a.FloodSamples
		summary["regions"] = len(a.Regions)
		summary["rag_edges"] = len(a.Edges)
		summary["merge_events"] = len(a.MergeLog)
		summary["foreground_pixels"] = countNonZero(a.RawMask)
	case checkpoint.LayerStats:
		if a.Stats != nil {
			summary["pixel_count"] = a.Stats.PixelCount
			summary["local_components"] = len(a.Stats.LocalStats)
			summary["mask_hash"] = a.Stats.MaskHash
		}
	}
	b, _ := json.Marshal(summary)
	return string(b)
}

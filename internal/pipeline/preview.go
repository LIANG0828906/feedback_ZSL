package pipeline

import (
	"fmt"
	"time"

	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/segment"
)

type PreviewResult struct {
	ImageID              string             `json:"image_id"`
	Params               model.Params       `json:"params"`
	Width                int                `json:"width"`
	Height               int                `json:"height"`
	DownsampleFactor     int                `json:"downsample_factor"`
	FloodSamples         int                `json:"flood_samples"`
	ExactLatticePixels   int                `json:"exact_lattice_pixels"`
	PreviewLatticePixels int                `json:"preview_lattice_pixels"`
	SampleReduction      float64            `json:"sample_reduction"`
	ElapsedMS            int64              `json:"elapsed_ms"`
	DelayBudgetMS        int64              `json:"delay_budget_ms"`
	WithinDelayBudget    bool               `json:"within_delay_budget"`
	Basis                string             `json:"basis"`
	StatsPlaceholder     bool               `json:"stats_placeholder"`
	Stats                []model.RegionStat `json:"stats"`
	Mask                 []byte             `json:"-"`
}

func RunPreview(imageID string, rgb *imaging.RGBImage, params model.Params, budget model.Budget, factor int) (*PreviewResult, error) {
	if err := model.ValidateParams(params); err != nil {
		return nil, err
	}
	if factor <= 0 {
		factor = 2
	}
	started := time.Now()
	small := imaging.DownsampleAverage(rgb, factor)
	res, err := segment.Segment(small, params)
	if err != nil {
		return nil, err
	}
	smallRadius := params.MorphRadius / factor
	smallClean := imaging.OpenClose(res.RawMask, small.Width, small.Height, smallRadius)
	mask := imaging.UpsampleNearestMask(smallClean, small.Width, small.Height, rgb.Width, rgb.Height, factor)
	_, stats := imaging.ConnectedComponents(mask, rgb.Width, rgb.Height)
	elapsed := time.Since(started).Milliseconds()
	budgetMS := budget.PreviewDelayMS
	if budgetMS <= 0 {
		budgetMS = 250
	}
	basis := fmt.Sprintf("drag-preview area-average factor=%d; priority-flood lattice %d -> %d pixels; actual pops=%d; delay budget=%dms; operator=%s; markers=residual-plane-otsu",
		factor, rgb.Width*rgb.Height, small.Width*small.Height, res.FloodSamples, budgetMS, params.GradientOperator)
	return &PreviewResult{
		ImageID: imageID, Params: params, Width: rgb.Width, Height: rgb.Height,
		DownsampleFactor: factor, FloodSamples: res.FloodSamples,
		ExactLatticePixels: rgb.Width * rgb.Height, PreviewLatticePixels: small.Width * small.Height,
		SampleReduction: 1 - float64(res.FloodSamples)/float64(rgb.Width*rgb.Height),
		ElapsedMS:       elapsed, DelayBudgetMS: budgetMS, WithinDelayBudget: elapsed <= budgetMS,
		Basis: basis, StatsPlaceholder: true, Stats: stats, Mask: mask,
	}, nil
}

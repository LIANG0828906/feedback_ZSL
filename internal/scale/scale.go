package scale

import (
	"fmt"

	"mediaforge/internal/domain"
)

type Decision struct {
	Path             string              `json:"path"`
	BlockRows        int                 `json:"blockRows"`
	DownsampleFactor int                 `json:"downsampleFactor"`
	Reason           string              `json:"reason"`
	Reasons          []string            `json:"reasons"`
	SourcePixels     int64               `json:"sourcePixels"`
	TargetPixels     int64               `json:"targetPixels"`
	AudioSamples     int64               `json:"audioSamples"`
	EstimatedBytes   int64               `json:"estimatedBytes"`
	EstimatedMillis  int64               `json:"estimatedMillis"`
	VariantCount     int                 `json:"variantCount"`
	MemoryBudget     int64               `json:"memoryBudget"`
	TimeBudgetMs     int                 `json:"timeBudgetMs"`
	Preview          *domain.PreviewInfo `json:"preview,omitempty"`
}

type BatchDecision struct {
	Path               string              `json:"path"`
	VariantCount       int                 `json:"variantCount"`
	SourcePixels       int64               `json:"sourcePixels"`
	TargetPixels       int64               `json:"targetPixels"`
	AudioSamples       int64               `json:"audioSamples"`
	EstimatedBytes     int64               `json:"estimatedBytes"`
	MaxEstimatedMillis int64               `json:"maxEstimatedMillis"`
	MemoryBudget       int64               `json:"memoryBudget"`
	PerVariantMillis   int                 `json:"perVariantMillis"`
	Reasons            []string            `json:"reasons"`
	Variants           map[string]Decision `json:"variants"`
}

func Decide(rp *domain.ResolvedProject) BatchDecision {
	p := rp.Project
	b := p.Budget
	out := BatchDecision{
		Path: "fine", VariantCount: len(rp.Variants), MemoryBudget: b.MemoryBytes,
		PerVariantMillis: b.PerVariantMillis, Variants: map[string]Decision{},
	}
	for _, v := range rp.Variants {
		img := rp.Sources[v.ImageSourceID]
		aud := rp.Sources[v.AudioSourceID]
		sourcePixels := int64(img.Width) * int64(img.Height)
		targetPixels := int64(v.Recipe.TargetWidth) * int64(v.Recipe.TargetHeight)
		audioSamples := aud.SamplesPerChannel * int64(aud.AudioChannels)
		estimated := sourcePixels*8 + targetPixels*int64(v.Recipe.ImageChannels)*4 +
			audioSamples*8 + targetPixels*8
		estimatedMillis := sourcePixels/25000 + audioSamples/100000 + targetPixels/50000 + 1
		out.SourcePixels += sourcePixels
		out.TargetPixels += targetPixels
		out.AudioSamples += audioSamples
		out.EstimatedBytes += estimated
		if estimatedMillis > out.MaxEstimatedMillis {
			out.MaxEstimatedMillis = estimatedMillis
		}
	}
	out.Reasons = []string{
		fmt.Sprintf("total source pixels=%d threshold=%d", out.SourcePixels, b.FineMaxPixels),
		fmt.Sprintf("total audio samples=%d threshold=%d", out.AudioSamples, b.FineMaxSamples),
		fmt.Sprintf("variant count=%d threshold=%d", out.VariantCount, b.FineMaxVariants),
		fmt.Sprintf("estimated bytes=%d memory budget=%d (fine requires <=50%%)",
			out.EstimatedBytes, b.MemoryBytes),
		fmt.Sprintf("max estimated variant time=%dms budget=%dms",
			out.MaxEstimatedMillis, b.PerVariantMillis),
	}
	useFine := out.SourcePixels <= b.FineMaxPixels &&
		out.AudioSamples <= b.FineMaxSamples &&
		out.VariantCount <= b.FineMaxVariants &&
		out.EstimatedBytes <= b.MemoryBytes/2 &&
		out.MaxEstimatedMillis <= int64(b.PerVariantMillis)
	if useFine {
		out.Path = "fine"
	} else {
		out.Path = "adaptive"
	}
	for _, v := range rp.Variants {
		img := rp.Sources[v.ImageSourceID]
		aud := rp.Sources[v.AudioSourceID]
		decision := Decision{
			Path:         out.Path,
			SourcePixels: int64(img.Width) * int64(img.Height),
			TargetPixels: int64(v.Recipe.TargetWidth) * int64(v.Recipe.TargetHeight),
			AudioSamples: aud.SamplesPerChannel * int64(aud.AudioChannels),
			VariantCount: len(rp.Variants),
			MemoryBudget: b.MemoryBytes,
			TimeBudgetMs: b.PerVariantMillis,
			Reasons:      append([]string(nil), out.Reasons...),
		}
		if useFine {
			decision.BlockRows = v.Recipe.TargetHeight
			decision.DownsampleFactor = 1
			decision.EstimatedBytes = decision.SourcePixels*8 + decision.TargetPixels*int64(v.Recipe.ImageChannels)*4 +
				decision.AudioSamples*8
			decision.EstimatedMillis = decision.SourcePixels/25000 + decision.AudioSamples/100000 +
				decision.TargetPixels/50000 + 1
			decision.Reason = fmt.Sprintf(
				"fine: sourcePixels %d <= %d, audioSamples %d <= %d, variants %d <= %d, estimatedBytes %d <= %d, estimatedMs %d <= %d",
				decision.SourcePixels, b.FineMaxPixels, decision.AudioSamples, b.FineMaxSamples,
				decision.VariantCount, b.FineMaxVariants, decision.EstimatedBytes, b.MemoryBytes/2,
				decision.EstimatedMillis, b.PerVariantMillis)
		} else {
			rowPixels := b.AdaptiveBlockPixels
			if rowPixels > b.MaxPixelsPerWorker {
				rowPixels = b.MaxPixelsPerWorker
			}
			rowWidth := int64(v.Recipe.TargetWidth * v.Recipe.ImageChannels)
			blockRows := int(rowPixels / rowWidth)
			if blockRows < 1 {
				blockRows = 1
			}
			if blockRows > v.Recipe.TargetHeight {
				blockRows = v.Recipe.TargetHeight
			}
			maxDim := img.Width
			if img.Height > maxDim {
				maxDim = img.Height
			}
			down := (maxDim + 1023) / 1024
			if down < 1 {
				down = 1
			}
			if down > b.MaxDownsample {
				down = b.MaxDownsample
			}
			decision.BlockRows = blockRows
			decision.DownsampleFactor = down
			decision.EstimatedBytes = decision.SourcePixels*8 + int64(blockRows*v.Recipe.TargetWidth*v.Recipe.ImageChannels)*2 +
				decision.AudioSamples*8
			decision.EstimatedMillis = decision.SourcePixels/25000 + decision.AudioSamples/100000 +
				decision.TargetPixels/50000 + 1
			decision.Reason = fmt.Sprintf(
				"adaptive: budget exceeded; blockRows=%d (pixel cap=%d), previewDownsample=%dx, estimatedMs=%d/%d, output remains exact",
				blockRows, rowPixels, down, decision.EstimatedMillis, b.PerVariantMillis)
		}
		out.Variants[v.ID] = decision
	}
	return out
}

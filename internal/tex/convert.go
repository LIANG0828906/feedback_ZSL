package tex

import (
	"fmt"
	"math"
)

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Pixels() int {
	return r.W * r.H
}

type ChannelStats struct {
	Count      int64   `json:"count"`
	NonZero    int64   `json:"nonzero"`
	SumAbs     int64   `json:"sum_abs"`
	MaxAbs     int64   `json:"max_abs"`
	SumSquares int64   `json:"sum_squares"`
	MeanAbs    float64 `json:"mean_abs"`
	RMSE       float64 `json:"rmse"`
}

type QuantStats struct {
	ChannelStats
	ByChannel  []ChannelStats `json:"by_channel"`
	SourceBits int            `json:"source_bit_depth"`
	TargetBits int            `json:"target_bit_depth"`
}

type Histogram struct {
	Bins        int        `json:"bins"`
	Channels    []string   `json:"channels"`
	Counts      [][]uint64 `json:"counts"`
	ChannelMin  []uint16   `json:"channel_min"`
	ChannelMax  []uint16   `json:"channel_max"`
	ChannelMean []float64  `json:"channel_mean"`
}

type Converted struct {
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	BitDepth          int        `json:"bit_depth"`
	Layout            Layout     `json:"layout"`
	Pixels            []uint16   `json:"-"`
	Errors            []int32    `json:"-"`
	QuantStats        QuantStats `json:"quant_stats"`
	Histogram         Histogram  `json:"histogram"`
	Mapping           Mapping    `json:"mapping"`
	DescriptorVersion string     `json:"descriptor_version"`
	AlgorithmVersion  string     `json:"algorithm_version"`
}

type ConvertOptions struct {
	// Fault, when set, is called before converting a pixel block. Returning an
	// error fails only that block and is used to exercise recovery paths.
	Fault func(rect Rect) error
}

func Quantize(value uint16, sourceBits, targetBits int) (uint16, int32) {
	if sourceBits == targetBits {
		return value, 0
	}
	sourceMax := int64(255)
	targetMax := int64(255)
	if sourceBits == 16 {
		sourceMax = 65535
	}
	if targetBits == 16 {
		targetMax = 65535
	}
	rounded := (int64(value)*targetMax + sourceMax/2) / sourceMax
	back := (rounded*sourceMax + targetMax/2) / targetMax
	return uint16(rounded), int32(back - int64(value))
}

func blockBounds(img *Image, rect Rect) error {
	if rect.W <= 0 || rect.H <= 0 {
		return ErrPara("rect", "block width and height must be positive")
	}
	if rect.X < 0 || rect.Y < 0 || rect.X+rect.W > img.Width || rect.Y+rect.H > img.Height {
		return ErrPara("rect", fmt.Sprintf("block [%d,%d %dx%d] is outside image %dx%d", rect.X, rect.Y, rect.W, rect.H, img.Width, img.Height))
	}
	return nil
}

func ConvertBlock(img *Image, desc Descriptor, rect Rect, opts ConvertOptions) (*Converted, error) {
	desc = desc.Normalize()
	if err := img.Validate(); err != nil {
		return nil, err
	}
	if err := desc.ValidateForImage(img); err != nil {
		return nil, err
	}
	if err := blockBounds(img, rect); err != nil {
		return nil, err
	}
	if opts.Fault != nil {
		if err := opts.Fault(rect); err != nil {
			return nil, err
		}
	}
	mapping, err := BuildMapping(desc)
	if err != nil {
		return nil, err
	}
	targetChannels := desc.TargetLayout.ChannelCount()
	sourceChannels := img.Channels()
	pixelCount := rect.W * rect.H
	out := &Converted{
		Width:             rect.W,
		Height:            rect.H,
		BitDepth:          desc.TargetBitDepth,
		Layout:            desc.TargetLayout,
		Pixels:            make([]uint16, pixelCount*targetChannels),
		Errors:            make([]int32, pixelCount*targetChannels),
		Mapping:           mapping,
		DescriptorVersion: desc.Version(),
		AlgorithmVersion:  AlgorithmVersion,
	}
	for yy := 0; yy < rect.H; yy++ {
		dy := rect.Y + yy
		for xx := 0; xx < rect.W; xx++ {
			dx := rect.X + xx
			srcOffset := (dy*img.Width + dx) * sourceChannels
			dstOffset := (yy*rect.W + xx) * targetChannels
			for targetIndex := 0; targetIndex < targetChannels; targetIndex++ {
				sourceIndex := mapping.TargetToSource[targetIndex]
				var sourceValue uint16
				if sourceIndex >= 0 {
					sourceValue = img.Pixels[srcOffset+sourceIndex]
				} else if desc.Fill == FillRepeatAlpha && mapping.AlphaSource != nil {
					sourceValue = img.Pixels[srcOffset+mapping.AlphaSource.Index]
				}
				value, quantError := Quantize(sourceValue, img.BitDepth, desc.TargetBitDepth)
				out.Pixels[dstOffset+targetIndex] = value
				out.Errors[dstOffset+targetIndex] = quantError
			}
		}
	}
	out.QuantStats = ComputeQuantStats(out.Errors, targetChannels, img.BitDepth, desc.TargetBitDepth)
	out.Histogram = BuildHistogram(out.Pixels, targetChannels, desc.TargetLayout, desc.TargetBitDepth)
	return out, nil
}

func ConvertFull(img *Image, desc Descriptor, opts ConvertOptions) (*Converted, error) {
	return ConvertBlock(img, desc, Rect{X: 0, Y: 0, W: img.Width, H: img.Height}, opts)
}

func ComputeQuantStats(errors []int32, channels, sourceBits, targetBits int) QuantStats {
	stats := QuantStats{
		SourceBits: sourceBits,
		TargetBits: targetBits,
		ByChannel:  make([]ChannelStats, channels),
	}
	for i, value := range errors {
		abs := value
		if abs < 0 {
			abs = -abs
		}
		channel := i % channels
		stats.Count++
		stats.SumAbs += int64(abs)
		stats.SumSquares += int64(value) * int64(value)
		abs64 := int64(abs)
		if abs64 > stats.MaxAbs {
			stats.MaxAbs = abs64
		}
		if abs != 0 {
			stats.NonZero++
		}
		cs := &stats.ByChannel[channel]
		cs.Count++
		cs.SumAbs += int64(abs)
		cs.SumSquares += int64(value) * int64(value)
		if abs64 > cs.MaxAbs {
			cs.MaxAbs = abs64
		}
		if abs != 0 {
			cs.NonZero++
		}
	}
	finishStats := func(s *ChannelStats) {
		if s.Count == 0 {
			return
		}
		s.MeanAbs = float64(s.SumAbs) / float64(s.Count)
		s.RMSE = math.Sqrt(float64(s.SumSquares) / float64(s.Count))
	}
	finishStats(&stats.ChannelStats)
	for i := range stats.ByChannel {
		finishStats(&stats.ByChannel[i])
	}
	return stats
}

func BuildHistogram(pixels []uint16, channels int, layout Layout, bitDepth int) Histogram {
	const bins = 256
	max := uint32(255)
	if bitDepth == 16 {
		max = 65535
	}
	hist := Histogram{
		Bins:        bins,
		Channels:    layout.Channels(),
		Counts:      make([][]uint64, channels),
		ChannelMin:  make([]uint16, channels),
		ChannelMax:  make([]uint16, channels),
		ChannelMean: make([]float64, channels),
	}
	for c := 0; c < channels; c++ {
		hist.Counts[c] = make([]uint64, bins)
		hist.ChannelMin[c] = ^uint16(0)
	}
	for i, value := range pixels {
		c := i % channels
		bucket := int((uint32(value)*uint32(bins-1) + max/2) / max)
		hist.Counts[c][bucket]++
		if value < hist.ChannelMin[c] {
			hist.ChannelMin[c] = value
		}
		if value > hist.ChannelMax[c] {
			hist.ChannelMax[c] = value
		}
		hist.ChannelMean[c] += float64(value)
	}
	pixelCount := len(pixels) / channels
	for c := 0; c < channels; c++ {
		if pixelCount > 0 {
			hist.ChannelMean[c] /= float64(pixelCount)
		}
		if hist.ChannelMin[c] == ^uint16(0) {
			hist.ChannelMin[c] = 0
		}
	}
	return hist
}

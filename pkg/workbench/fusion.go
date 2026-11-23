package workbench

import "math"

type fusionWeights struct {
	reliability []float64
	weight      []float64
}

// exposureWeights combines a smooth mid-tone reliability window with a
// configurable bias and sharpness. Overexposed and underexposed samples fall
// towards zero weight instead of contaminating the radiance estimate.
func exposureWeights(codes []float64, cfg ProcessConfig) fusionWeights {
	reliability, weights := exposureWeightsFixed(codes, cfg)
	return fusionWeights{
		reliability: append([]float64(nil), reliability[:len(codes)]...),
		weight:      append([]float64(nil), weights[:len(codes)]...),
	}
}

func exposureWeightsFixed(codes []float64, cfg ProcessConfig) ([9]float64, [9]float64) {
	n := len(codes)
	var reliability [9]float64
	var weight [9]float64
	var sum float64
	for i, code := range codes {
		x := clamp(code, 0, 1)
		distance := math.Abs(2*x - 1)
		base := 1 - math.Pow(distance, 4)
		if base < 0 {
			base = 0
		}
		reliable := math.Pow(base+1e-6, cfg.WeightSharpness)
		bias := math.Exp(6 * cfg.WeightBias * (x - 0.5))
		w := reliable * bias
		reliability[i] = reliable
		weight[i] = w
		sum += w
	}
	if sum < 1e-12 {
		mid := n / 2
		for i := 0; i < n; i++ {
			weight[i] = 0
		}
		weight[mid] = 1
		sum = 1
	}
	for i := 0; i < n; i++ {
		weight[i] /= sum
	}
	return reliability, weight
}

func linearizeCode(code uint16, bitDepth int, gamma, exposure float64) float64 {
	maxQ := float64(maxCode(bitDepth))
	if maxQ <= 0 || exposure <= 0 {
		return 0
	}
	x := float64(code) / maxQ
	return math.Pow(x, gamma) / exposure
}

func fuseExposurePixel(scene *Scene, x, y int, cfg ProcessConfig) float64 {
	idx := y*scene.Meta.Width + x
	n := len(scene.Exposures)
	var codes [9]float64
	var radiances [9]float64
	maxQ := float64(maxCode(scene.Meta.BitDepth))
	for i, frame := range scene.Exposures {
		code := frame.Data[idx]
		codes[i] = float64(code) / maxQ
		radiances[i] = linearizeCode(code, scene.Meta.BitDepth, scene.Meta.ResponseGamma, frame.ExposureTime)
	}
	_, weights := exposureWeightsFixed(codes[:n], cfg)
	var fused float64
	for i := 0; i < n; i++ {
		fused += weights[i] * radiances[i]
	}
	return fused
}

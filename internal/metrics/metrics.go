package metrics

import (
	"math"

	"audioeval/internal/model"
)

const (
	PeakSample = 32767
	PSNRCapUDb = int64(300_000_000)
	SSIMOneQ16 = int64(1 << 16)
)

func WindowStarts(frameLength, window, hop int) []int {
	if window <= 0 || hop <= 0 || window > frameLength {
		return nil
	}
	last := frameLength - window
	starts := make([]int, 0, last/hop+2)
	for start := 0; start <= last; start += hop {
		starts = append(starts, start)
	}
	if len(starts) == 0 || starts[len(starts)-1] != last {
		starts = append(starts, last)
	}
	return starts
}

func ComputeWindow(channel, start int, x, y []int16) model.WindowStat {
	stat := model.WindowStat{Channel: channel, Start: start, Length: len(x)}
	for i := range x {
		xv := int64(x[i])
		yv := int64(y[i])
		diff := xv - yv
		stat.SumX += xv
		stat.SumY += yv
		stat.SumXX += xv * xv
		stat.SumYY += yv * yv
		stat.SumXY += xv * yv
		stat.SSE += diff * diff
	}
	return stat
}

func ComputeWindowStrided(channel, start int, x, y []int16, stride, count int) model.WindowStat {
	stat := model.WindowStat{Channel: channel, Start: start, Length: count}
	for i := 0; i < stat.Length; i++ {
		xv := int64(x[i*stride])
		yv := int64(y[i*stride])
		diff := xv - yv
		stat.SumX += xv
		stat.SumY += yv
		stat.SumXX += xv * xv
		stat.SumYY += yv * yv
		stat.SumXY += xv * yv
		stat.SSE += diff * diff
	}
	return stat
}

func PSNRMicroDB(sse int64, count int) int64 {
	if count <= 0 {
		return 0
	}
	if sse <= 0 {
		return PSNRCapUDb
	}
	ratio := float64(PeakSample*PeakSample) * float64(count) / float64(sse)
	value := 10 * math.Log10(ratio) * 1_000_000
	if value > float64(PSNRCapUDb) {
		return PSNRCapUDb
	}
	return int64(math.Floor(value))
}

func SSIMQ16(stat model.WindowStat) int64 {
	if stat.Length <= 0 {
		return 0
	}
	n := float64(stat.Length)
	meanX := float64(stat.SumX) / n
	meanY := float64(stat.SumY) / n
	varX := float64(stat.SumXX)/n - meanX*meanX
	varY := float64(stat.SumYY)/n - meanY*meanY
	cov := float64(stat.SumXY)/n - meanX*meanY
	if varX < 0 {
		varX = 0
	}
	if varY < 0 {
		varY = 0
	}
	const (
		c1 = (0.01 * 65535.0) * (0.01 * 65535.0)
		c2 = (0.03 * 65535.0) * (0.03 * 65535.0)
	)
	numerator := (2*meanX*meanY + c1) * (2*cov + c2)
	denominator := (meanX*meanX + meanY*meanY + c1) * (varX + varY + c2)
	if denominator == 0 {
		return SSIMOneQ16
	}
	value := math.Floor(numerator / denominator * float64(SSIMOneQ16))
	if value < -float64(SSIMOneQ16) {
		return -SSIMOneQ16
	}
	if value > float64(SSIMOneQ16) {
		return SSIMOneQ16
	}
	return int64(value)
}

func NoiseStdQ16(sse, count int64) int64 {
	if count <= 0 {
		return 0
	}
	value := math.Sqrt(float64(sse)/float64(count)) * 65536
	return int64(math.Round(value))
}

func ResidualSSE(signal []int16) (sse int64, count int64) {
	for i := 1; i < len(signal); i++ {
		diff := int64(signal[i]) - int64(signal[i-1])
		sse += diff * diff
	}
	return sse, int64(max(0, len(signal)-1))
}

func CoreStats(channel int, x, y []int16) model.ChannelCoreStat {
	stat := model.ChannelCoreStat{Channel: channel, Count: int64(len(x))}
	for i := range x {
		xv := int64(x[i])
		yv := int64(y[i])
		diff := xv - yv
		stat.SumX += xv
		stat.SumY += yv
		stat.SumXX += xv * xv
		stat.SumYY += yv * yv
		stat.SumXY += xv * yv
		stat.SSE += diff * diff
	}
	return stat
}

func CoreStatsStrided(channel int, x, y []int16, stride, count int) model.ChannelCoreStat {
	stat := model.ChannelCoreStat{Channel: channel, Count: int64(count)}
	for i := 0; i < int(stat.Count); i++ {
		xv := int64(x[i*stride])
		yv := int64(y[i*stride])
		diff := xv - yv
		stat.SumX += xv
		stat.SumY += yv
		stat.SumXX += xv * xv
		stat.SumYY += yv * yv
		stat.SumXY += xv * yv
		stat.SSE += diff * diff
	}
	return stat
}

func BlockQuality(core model.ChannelCoreStat) model.BlockQuality {
	window := model.WindowStat{
		Channel: core.Channel,
		Length:  int(core.Count),
		SumX:    core.SumX,
		SumY:    core.SumY,
		SumXX:   core.SumXX,
		SumYY:   core.SumYY,
		SumXY:   core.SumXY,
		SSE:     core.SSE,
	}
	return model.BlockQuality{
		PSNRUDb: PSNRMicroDB(core.SSE, int(core.Count)),
		SSIMQ16: SSIMQ16(window),
	}
}

// ErrorUpperBound derives a conservative sample-domain L2 bound using only
// observed quantization step, frame geometry, analysis window and declared
// truth noise-floor standard deviation. It deliberately does not accept or
// inspect expected SNR.
func ErrorUpperBound(quantStep int64, frameLength, frameHop, window int, noiseFloorStdQ16 int64) int64 {
	if quantStep <= 0 || frameLength <= 0 || frameHop <= 0 || window <= 0 {
		return 0
	}
	quantVariance := float64(quantStep*quantStep) * float64(2*frameLength-1) /
		float64(12*frameLength*frameLength)
	noiseStd := float64(noiseFloorStdQ16) / 65536.0
	base := math.Sqrt(float64(window) * (quantVariance + noiseStd*noiseStd))
	hopTerm := float64(quantStep) * float64(max(0, frameLength-frameHop)) / float64(2*frameLength)
	bound := int64(math.Ceil(base + hopTerm))
	if bound < 1 {
		bound = 1
	}
	return bound
}

func MergeNoise(originalSSE, reconstructionSSE, count int64) (int64, int64) {
	return NoiseStdQ16(originalSSE, count), NoiseStdQ16(reconstructionSSE, count)
}

type WholeFrameMetric struct {
	PSNRUDb                   int64
	SSIMQ16                   int64
	OriginalNoiseStdQ16       int64
	ReconstructionNoiseStdQ16 int64
	WindowCount               int
}

func EvaluateWholeFrame(x, y []int16, channels, window, hop int) WholeFrameMetric {
	var psnrSum, ssimSum int64
	var windows int
	var originalResidualSSE, reconstructionResidualSSE, residualCount int64
	channelCount := channels
	if channelCount <= 0 {
		channelCount = 1
	}
	frameLength := len(x) / channelCount
	for ch := 0; ch < channelCount; ch++ {
		xChannel := make([]int16, frameLength)
		yChannel := make([]int16, frameLength)
		for i := 0; i < frameLength; i++ {
			xChannel[i] = x[i*channelCount+ch]
			yChannel[i] = y[i*channelCount+ch]
		}
		for _, start := range WindowStarts(frameLength, window, hop) {
			stat := ComputeWindow(ch, start, xChannel[start:start+window], yChannel[start:start+window])
			psnrSum += PSNRMicroDB(stat.SSE, stat.Length)
			ssimSum += SSIMQ16(stat)
			windows++
		}
		xSSE, count := ResidualSSE(xChannel)
		ySSE, _ := ResidualSSE(yChannel)
		originalResidualSSE += xSSE
		reconstructionResidualSSE += ySSE
		residualCount += count
	}
	result := WholeFrameMetric{WindowCount: windows}
	if windows > 0 {
		result.PSNRUDb = psnrSum / int64(windows)
		result.SSIMQ16 = ssimSum / int64(windows)
	}
	result.OriginalNoiseStdQ16, result.ReconstructionNoiseStdQ16 = MergeNoise(originalResidualSSE, reconstructionResidualSSE, residualCount)
	return result
}

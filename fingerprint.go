package audiofp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
)

type Fingerprint struct {
	AlgorithmVersion string `json:"algorithm_version"`
	FrameIndex       int    `json:"frame_index"`
	StartSample      int    `json:"start_sample"`
	FrameLength      int    `json:"frame_length"`
	FrameShift       int    `json:"frame_shift"`
	SampleRate       int    `json:"sample_rate"`
	BandCount        int    `json:"band_count"`
	Bands            []int  `json:"bands"`
	Energy           byte   `json:"energy"`
	PeakBand         int    `json:"peak_band"`
	FeatureVersion   string `json:"feature_version"`
	ParamVersion     string `json:"param_version"`
}

// Digest hashes every fingerprint field. JSON struct order is stable.
func (f Fingerprint) Digest() string {
	data, err := json.Marshal(f)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (f Fingerprint) ShortDigest() string {
	d := f.Digest()
	if len(d) > 16 {
		return d[:16]
	}
	return d
}

// FrameCount returns the number of complete analysis frames.
func FrameCount(sampleCount int, p Params) int {
	if sampleCount < p.FrameLength || p.FrameShift <= 0 {
		return 0
	}
	return (sampleCount-p.FrameLength)/p.FrameShift + 1
}

func FrameStart(frameIndex int, p Params) int {
	return frameIndex * p.FrameShift
}

// ExtractFrameAt extracts directly from the source sample slice. Callers that
// need an explicit bounded worker buffer should copy into a fixed buffer and
// call ExtractFrameBuffer.
func ExtractFrameAt(samples []float64, frameIndex, sampleRate int, p Params) (Fingerprint, error) {
	start := FrameStart(frameIndex, p)
	if start < 0 || start+p.FrameLength > len(samples) {
		return Fingerprint{}, newError(CodeRange, "frame_index",
			"frame %d range [%d,%d) is outside %d samples", frameIndex, start, start+p.FrameLength, len(samples))
	}
	frame := samples[start : start+p.FrameLength]
	return ExtractFrameBuffer(frame, frameIndex, start, sampleRate, p), nil
}

// ExtractFrameBuffer computes the deterministic spectrum fingerprint from an
// owned sample buffer of exactly p.FrameLength samples.
func ExtractFrameBuffer(frame []float64, frameIndex, startSample, sampleRate int, p Params) Fingerprint {
	if len(frame) != p.FrameLength {
		// Pool and frame planning guarantee this; keep the function total for
		// the valid domain and surface programming mistakes loudly in tests.
		panic("audiofp: frame buffer length does not match params")
	}

	nFFT := nextPow2(p.FrameLength)
	re := make([]float64, nFFT)
	im := make([]float64, nFFT)

	var sumSquares float64
	for i, x := range frame {
		sumSquares += x * x
		window := 0.5 * (1.0 - math.Cos(2.0*math.Pi*float64(i)/float64(p.FrameLength-1)))
		re[i] = x * window
	}
	rms := math.Sqrt(sumSquares / float64(p.FrameLength))
	fft(re, im)

	edges := melBandEdges(p.BandCount, sampleRate, nFFT)
	powers := make([]float64, p.BandCount)
	for k := 1; k <= nFFT/2; k++ {
		freq := float64(k) * float64(sampleRate) / float64(nFFT)
		power := re[k]*re[k] + im[k]*im[k]
		band := bandForFrequency(freq, edges)
		if band >= 0 {
			powers[band] += power
		}
	}

	maxDB := math.Inf(-1)
	bandDB := make([]float64, p.BandCount)
	for b, power := range powers {
		db := 10.0 * math.Log10(power+1e-20)
		bandDB[b] = db
		if db > maxDB {
			maxDB = db
		}
	}

	bands := make([]int, p.BandCount)
	peakBand := 0
	var peakByte byte
	for b, db := range bandDB {
		relative := (db - (maxDB - 55.0)) / 55.0
		relative = math.Max(0, math.Min(1, relative))
		value := byte(math.Round(relative * 255.0))
		bands[b] = int(value)
		if value > peakByte {
			peakByte = value
			peakBand = b
		}
	}

	rmsDB := 20.0 * math.Log10(math.Max(rms, 1e-7))
	energy := (rmsDB + 60.0) / 60.0
	energy = math.Max(0, math.Min(1, energy))

	return Fingerprint{
		AlgorithmVersion: AlgorithmVersion,
		FrameIndex:       frameIndex,
		StartSample:      startSample,
		FrameLength:      p.FrameLength,
		FrameShift:       p.FrameShift,
		SampleRate:       sampleRate,
		BandCount:        p.BandCount,
		Bands:            bands,
		Energy:           byte(math.Round(energy * 255.0)),
		PeakBand:         peakBand,
		FeatureVersion:   p.FeatureVersion(),
		ParamVersion:     p.Version(),
	}
}

func melBandEdges(count, sampleRate, nFFT int) []float64 {
	low := hzToMel(50)
	maxHz := math.Min(7200, float64(sampleRate)/2.0*0.98)
	high := hzToMel(maxHz)
	edges := make([]float64, count+1)
	for i := 0; i <= count; i++ {
		mel := low + (high-low)*float64(i)/float64(count)
		edges[i] = melToHz(mel)
	}
	// Keep the final edge inclusive of Nyquist-near bins.
	edges[count] = float64(sampleRate) / 2.0
	return edges
}

func hzToMel(hz float64) float64 {
	return 2595.0 * math.Log10(1.0+hz/700.0)
}

func melToHz(mel float64) float64 {
	return 700.0 * (math.Pow(10.0, mel/2595.0) - 1.0)
}

func bandForFrequency(freq float64, edges []float64) int {
	if freq < edges[0] || freq > edges[len(edges)-1] {
		return -1
	}
	for b := 0; b < len(edges)-1; b++ {
		if freq >= edges[b] && (freq < edges[b+1] || (b == len(edges)-2 && freq <= edges[b+1])) {
			return b
		}
	}
	return -1
}

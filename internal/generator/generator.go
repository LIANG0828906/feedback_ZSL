package generator

import (
	"math"

	"audiocheck/internal/model"
)

type FrameTruth struct {
	Frame                 int     `json:"frame"`
	StartSample           int     `json:"start_sample"`
	PulseSample           int     `json:"pulse_sample"`
	PulseOffset           int     `json:"pulse_offset"`
	QuantStep             int64   `json:"quant_step"`
	ExpectedSNRDBFixed    int64   `json:"expected_snr_db_fixed"`
	ExpectedNoiseStdFixed int64   `json:"expected_noise_std_fixed"`
	AnalyticSignalEnergy  int64   `json:"analytic_signal_energy"`
	AnalyticNoiseEnergy   float64 `json:"analytic_noise_energy"`
}

type Truth struct {
	GeneratorVersion string       `json:"generator_version"`
	Seed             int64        `json:"seed"`
	Fingerprint      string       `json:"fingerprint"`
	Frames           []FrameTruth `json:"frames"`
}

var qualitySteps = map[int]int64{
	0: 48,
	1: 24,
	2: 12,
	3: 6,
}

func QuantStepForGear(gear int) int64 {
	return qualitySteps[gear]
}

func Generate(m model.Manifest) (*model.Sequence, *Truth, []model.FrameQuantRecord, error) {
	if err := m.ValidateSchema(); err != nil {
		return nil, nil, nil, err
	}
	if err := m.ValidateParams(); err != nil {
		return nil, nil, nil, err
	}

	total := m.SamplesPerChannel()
	planarOriginal := make([][]int32, m.Channels)
	for ch := 0; ch < m.Channels; ch++ {
		planarOriginal[ch] = make([]int32, total)
		for t := 0; t < total; t++ {
			planarOriginal[ch][t] = synthesizeSample(m, ch, t)
		}
	}

	q := QuantStepForGear(m.QualityGear)
	planarRecon, weights, quantRecords := encodeFrames(m, planarOriginal, q)

	original := interleave(planarOriginal, m.Channels, total)
	reconstructed := interleave(planarRecon, m.Channels, total)

	seq := &model.Sequence{
		Manifest:      m,
		Original:      original,
		Reconstructed: reconstructed,
		QuantSteps:    make([]int64, m.FrameCount),
	}
	for f := range seq.QuantSteps {
		seq.QuantSteps[f] = q
	}
	if err := model.ValidateSequence(m, seq.Original, seq.Reconstructed); err != nil {
		return nil, nil, nil, err
	}

	truth := buildTruth(m, planarOriginal, weights, q)
	records := buildDomainErrors(m, planarOriginal, planarRecon, q, quantRecords)
	return seq, truth, records, nil
}

func pulseOffset(m model.Manifest, f int) int {
	hop := m.HopSize
	if hop <= 2 {
		return 0
	}
	quarter := hop / 4
	if quarter < 1 {
		quarter = 1
	}
	step := hop / 8
	if step < 1 {
		step = 1
	}
	off := quarter + (f%4)*step
	if off >= hop-1 {
		off = hop / 2
	}
	return off
}

func synthesizeSample(m model.Manifest, ch, t int) int32 {
	sr := float64(m.SampleRate)
	tt := float64(t) / sr
	seed := uint64(m.Seed)

	low := 8500.0 * math.Sin(2*math.Pi*110.0*tt+0.37*float64(ch))
	texture := 1900.0 * math.Sin(2*math.Pi*2750.0*tt+1.1*float64(ch))
	texture += 750.0 * deterministicNoise(seed, t, ch)

	pulse := 0.0
	for f := 0; f < m.FrameCount; f++ {
		pos := f*m.HopSize + pulseOffset(m, f)
		d := t - pos
		if d >= 0 && d <= 7 {
			pulse += 14500.0 * math.Exp(-float64(d)/1.35)
		}
		if t < pos {
			break
		}
	}

	v := math.Round(low + texture + pulse)
	peak := float64(m.PeakAmplitude())
	if v > peak {
		v = peak
	}
	if v < -peak {
		v = -peak
	}
	return int32(v)
}

func deterministicNoise(seed uint64, t, ch int) float64 {
	x := seed ^ uint64(t)*0x9e3779b97f4a7c15 ^ uint64(ch+1)*0xbf58476d1ce4e5b9
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return (float64(x>>11)/float64(1<<53))*2.0 - 1.0
}

type rawQuant struct {
	coeffs   []model.CoeffError
	rmsFixed int64
	maxFixed int64
}

func encodeFrames(m model.Manifest, original [][]int32, q int64) ([][]int32, [][]int, []rawQuant) {
	total := m.SamplesPerChannel()
	reconFloat := make([][]float64, m.Channels)
	weights := make([][]int, m.Channels)
	for ch := 0; ch < m.Channels; ch++ {
		reconFloat[ch] = make([]float64, total)
		weights[ch] = make([]int, total)
	}
	raw := make([]rawQuant, m.FrameCount*m.Channels)

	for f := 0; f < m.FrameCount; f++ {
		start := f * m.HopSize
		for ch := 0; ch < m.Channels; ch++ {
			frame := make([]float64, m.FrameSize)
			for n := 0; n < m.FrameSize; n++ {
				frame[n] = float64(original[ch][start+n])
			}
			before := dct(frame)
			after, coeffErrs, rmsFixed, maxFixed := quantizeCoefficients(before, q)
			decoded := idct(after)
			for n := 0; n < m.FrameSize; n++ {
				reconFloat[ch][start+n] += decoded[n]
				weights[ch][start+n]++
			}
			raw[f*m.Channels+ch] = rawQuant{coeffs: coeffErrs, rmsFixed: rmsFixed, maxFixed: maxFixed}
		}
	}

	recon := make([][]int32, m.Channels)
	peak := float64(m.PeakAmplitude())
	for ch := 0; ch < m.Channels; ch++ {
		recon[ch] = make([]int32, total)
		for t := 0; t < total; t++ {
			v := reconFloat[ch][t] / float64(weights[ch][t])
			v = math.Round(v)
			if v > peak {
				v = peak
			}
			if v < -peak {
				v = -peak
			}
			recon[ch][t] = int32(v)
		}
	}
	return recon, weights, raw
}

func quantizeCoefficients(before []float64, q int64) ([]float64, []model.CoeffError, int64, int64) {
	bits := model.CoefficientFracBits
	scale := float64(int64(1) << bits)
	stepFixed := q << bits
	out := make([]float64, len(before))
	errs := make([]model.CoeffError, len(before))
	var sumSq float64
	var maxAbs int64
	for i, v := range before {
		beforeFixed := int64(math.Round(v * scale))
		qFloat := float64(stepFixed)
		k := math.Round(float64(beforeFixed) / qFloat)
		afterFixed := int64(k) * stepFixed
		errFixed := afterFixed - beforeFixed
		out[i] = float64(afterFixed) / scale
		errs[i] = model.CoeffError{
			Index:       i,
			BeforeFixed: beforeFixed,
			AfterFixed:  afterFixed,
			ErrorFixed:  errFixed,
			StepFixed:   stepFixed,
		}
		absErr := errFixed
		if absErr < 0 {
			absErr = -absErr
		}
		if absErr > maxAbs {
			maxAbs = absErr
		}
		sumSq += float64(errFixed) * float64(errFixed)
	}
	rms := int64(math.Round(math.Sqrt(sumSq / float64(len(before)))))
	return out, errs, rms, maxAbs
}

func dct(x []float64) []float64 {
	n := len(x)
	out := make([]float64, n)
	root := math.Sqrt(2.0 / float64(n))
	for k := 0; k < n; k++ {
		s := 0.0
		for i := 0; i < n; i++ {
			s += x[i] * math.Cos(math.Pi*float64(k)*(float64(i)+0.5)/float64(n))
		}
		alpha := root
		if k == 0 {
			alpha = 1.0 / math.Sqrt(float64(n))
		}
		out[k] = alpha * s
	}
	return out
}

func idct(x []float64) []float64 {
	n := len(x)
	out := make([]float64, n)
	root := math.Sqrt(2.0 / float64(n))
	for i := 0; i < n; i++ {
		s := x[0] / math.Sqrt(float64(n))
		for k := 1; k < n; k++ {
			s += root * x[k] * math.Cos(math.Pi*float64(k)*(float64(i)+0.5)/float64(n))
		}
		out[i] = s
	}
	return out
}

func interleave(planar [][]int32, channels, total int) []int32 {
	out := make([]int32, total*channels)
	for t := 0; t < total; t++ {
		for ch := 0; ch < channels; ch++ {
			out[t*channels+ch] = planar[ch][t]
		}
	}
	return out
}

func buildTruth(m model.Manifest, original [][]int32, weights [][]int, q int64) *Truth {
	frames := make([]FrameTruth, 0, m.FrameCount)
	for f := 0; f < m.FrameCount; f++ {
		start := f * m.HopSize
		count := m.FrameSize * m.Channels
		var signalEnergy int64
		noiseEnergy := 0.0
		for ch := 0; ch < m.Channels; ch++ {
			for n := 0; n < m.FrameSize; n++ {
				v := int64(original[ch][start+n])
				signalEnergy += v * v
				w := float64(weights[ch][start+n])
				if w < 1 {
					w = 1
				}
				noiseEnergy += float64(q*q) / 12.0 / w
				noiseEnergy += 1.0 / 12.0
			}
		}
		noiseStd := math.Sqrt(noiseEnergy / float64(count))
		snr := math.Inf(1)
		if noiseEnergy > 0 {
			snr = 10.0 * math.Log10(float64(signalEnergy)/noiseEnergy)
		}
		off := pulseOffset(m, f)
		frames = append(frames, FrameTruth{
			Frame:                 f,
			StartSample:           start,
			PulseSample:           start + off,
			PulseOffset:           off,
			QuantStep:             q,
			ExpectedSNRDBFixed:    fixedDB(snr),
			ExpectedNoiseStdFixed: int64(math.Round(noiseStd * float64(model.MetricScale))),
			AnalyticSignalEnergy:  signalEnergy,
			AnalyticNoiseEnergy:   noiseEnergy,
		})
	}
	truth := &Truth{GeneratorVersion: model.GeneratorVersion, Seed: m.Seed, Frames: frames}
	fp, _ := model.HashCanonical(truth)
	truth.Fingerprint = fp
	return truth
}

func fixedDB(v float64) int64 {
	if math.IsInf(v, 1) || v > 180 {
		return 180 * model.MetricScale
	}
	if v < -180 {
		return -180 * model.MetricScale
	}
	return int64(math.Round(v * float64(model.MetricScale)))
}

func buildDomainErrors(m model.Manifest, original, recon [][]int32, q int64, raw []rawQuant) []model.FrameQuantRecord {
	records := make([]model.FrameQuantRecord, 0, m.FrameCount)
	stepFixed := q << model.CoefficientFracBits
	for f := 0; f < m.FrameCount; f++ {
		start := f * m.HopSize
		rec := model.FrameQuantRecord{
			Frame:               f,
			StartSample:         start,
			QuantStep:           q,
			QuantStepFixed:      stepFixed,
			CoefficientFracBits: model.CoefficientFracBits,
		}
		for ch := 0; ch < m.Channels; ch++ {
			rq := raw[f*m.Channels+ch]
			rec.CoeffErrors = append(rec.CoeffErrors, rq.coeffs...)
			rec.CoefficientCount += len(rq.coeffs)
			if rq.maxFixed > rec.CoeffMaxAbsErrorFixed {
				rec.CoeffMaxAbsErrorFixed = rq.maxFixed
			}
			rec.CoeffRMSErrorFixed = maxInt64(rec.CoeffRMSErrorFixed, rq.rmsFixed)
			for n := 0; n < m.FrameSize; n++ {
				e := int64(recon[ch][start+n]) - int64(original[ch][start+n])
				rec.SampleErrorCount++
				rec.SampleErrorSum += e
				rec.SampleErrorSumSq += e * e
				abs := int(e)
				if abs < 0 {
					abs = -abs
				}
				if abs > rec.MaxAbsSampleError {
					rec.MaxAbsSampleError = abs
				}
			}
		}
		if next := f + 1; next < m.FrameCount {
			b := next * m.HopSize
			for ch := 0; ch < m.Channels; ch++ {
				before := recon[ch][b-1] - original[ch][b-1]
				after := recon[ch][b] - original[ch][b]
				jump := int64(after) - int64(before)
				rec.BoundaryJumps = append(rec.BoundaryJumps, model.BoundaryJump{
					BoundarySample: b,
					FrameA:         f,
					FrameB:         next,
					ErrorBefore:    before,
					ErrorAfter:     after,
					Jump:           jump,
				})
				overlap := m.FrameSize - m.HopSize
				if overlap < 0 {
					overlap = 0
				}
				rec.DiffusionPaths = append(rec.DiffusionPaths, model.DiffusionPath{
					FromFrame:      f,
					ToFrame:        next,
					BoundarySample: b,
					OverlapSamples: overlap,
					Coupling:       float64(overlap) / float64(m.FrameSize),
					BoundaryJump:   jump,
					Direction:      "time_adjacent_forward",
				})
			}
		}
		records = append(records, rec)
	}
	return records
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

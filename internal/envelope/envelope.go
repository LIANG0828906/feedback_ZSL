package envelope

import (
	"context"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
	"prosodyalign/internal/sched"
)

const AlgorithmVersion = "envelope-int-v1"

func frameCount(n, length, hop int) int {
	if n < length || hop <= 0 {
		return 0
	}
	return (n-length)/hop + 1
}

func phonemeAt(p *model.Project, sample int64) int {
	for i := range p.Phonemes {
		if sample >= p.Phonemes[i].StartSample && sample < p.Phonemes[i].EndSample {
			return i
		}
	}
	if len(p.Phonemes) > 0 && sample >= p.Phonemes[len(p.Phonemes)-1].EndSample {
		return len(p.Phonemes) - 1
	}
	return 0
}

func absInt16(v int16) int64 {
	if v < 0 {
		return -int64(v)
	}
	return int64(v)
}

func rawFrameLoudness(pcm []int16, start, length int) int64 {
	end := start + length
	if end > len(pcm) {
		end = len(pcm)
	}
	var sum int64
	for i := start; i < end; i++ {
		sum += absInt16(pcm[i])
	}
	return sum
}

type frameBlock struct {
	first, last int
	startSample int
	samples     int
}

func planBlocks(count, length, hop, bufferCap int) []frameBlock {
	var blocks []frameBlock
	first := 0
	for first < count {
		last := first
		for last+1 < count {
			nextFirst := first
			nextLast := last + 1
			span := length
			if nextLast > nextFirst {
				span = length + (nextLast-nextFirst)*hop
			}
			if span > bufferCap {
				break
			}
			last = nextLast
		}
		start := first * hop
		end := last*hop + length
		blocks = append(blocks, frameBlock{first: first, last: last, startSample: start, samples: end - start})
		first = last + 1
	}
	return blocks
}

func Extract(p *model.Project, sch *sched.Scheduler) ([]model.FrameResult, sched.Metrics, error) {
	params := p.EnvelopeParams
	length, hop := params.FrameLengthSamples, params.HopSamples
	if length <= 0 || hop <= 0 {
		return nil, sched.Metrics{}, errs.Schema(string(p.SourceVersion), "$.envelope", "frame length and hop must be positive")
	}
	count := frameCount(len(p.PCM), length, hop)
	if count == 0 {
		return nil, sched.Metrics{}, errs.Env(0, 0, "PCM is shorter than one analysis frame")
	}
	raw := make([]int64, count)

	if sch == nil {
		for i := 0; i < count; i++ {
			raw[i] = rawFrameLoudness(p.PCM, i*hop, length)
		}
	} else {
		capSamples := sch.BufferCap()
		blocks := planBlocks(count, length, hop, capSamples)
		chunks := make([]sched.Chunk, 0, len(blocks))
		for bi, block := range blocks {
			block := block
			chunks = append(chunks, sched.Chunk{
				ID: bi, Samples: block.samples,
				Run: func() error {
					for i := block.first; i <= block.last; i++ {
						raw[i] = rawFrameLoudness(p.PCM, i*hop, length)
					}
					return nil
				},
			})
		}
		if err := sch.RunChunks(context.Background(), chunks); err != nil {
			return nil, sch.Metrics(), err
		}
	}

	smoothed := smooth(raw, params.SmoothWindow)
	var maxSmoothed int64
	for _, v := range smoothed {
		if v > maxSmoothed {
			maxSmoothed = v
		}
	}
	scale := params.FixedPointScale
	if scale <= 0 {
		scale = 1_000_000
	}

	frames := make([]model.FrameResult, count)
	for i := 0; i < count; i++ {
		start := int64(i * hop)
		center := start + int64(length/2)
		envValue := int64(0)
		if maxSmoothed > 0 {
			envValue = smoothed[i] * scale / maxSmoothed
		}
		phonemeIndex := phonemeAt(p, center)
		if envValue < 0 || envValue > scale {
			return nil, metricsOrZero(sch), errs.Env(i, phonemeIndex, "normalized envelope value outside declared fixed-point domain")
		}
		frames[i] = model.FrameResult{
			FrameIndex: i, StartSample: start, CenterSample: center,
			RawLoudness: raw[i], SmoothedLoudness: smoothed[i], EnvelopeFixed: envValue,
			PhonemeIndex: phonemeIndex,
		}
	}
	return frames, metricsOrZero(sch), nil
}

func metricsOrZero(sch *sched.Scheduler) sched.Metrics {
	if sch == nil {
		return sched.Metrics{}
	}
	return sch.Metrics()
}

func smooth(raw []int64, window int) []int64 {
	n := len(raw)
	if window <= 1 || n == 0 {
		return append([]int64(nil), raw...)
	}
	if window%2 == 0 {
		window++
	}
	radius := window / 2
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		lo := i - radius
		if lo < 0 {
			lo = 0
		}
		hi := i + radius
		if hi >= n {
			hi = n - 1
		}
		var sum int64
		for j := lo; j <= hi; j++ {
			sum += raw[j]
		}
		out[i] = sum / int64(hi-lo+1)
	}
	return out
}

func ValidateFixedPoint(frames []model.FrameResult, scale int64) error {
	for _, f := range frames {
		if f.EnvelopeFixed < 0 || f.EnvelopeFixed > scale {
			return errs.Env(f.FrameIndex, f.PhonemeIndex, "envelope value is negative or exceeds fixed-point domain")
		}
	}
	return nil
}

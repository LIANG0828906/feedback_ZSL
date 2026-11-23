// Package samplegen deterministically generates sample media projects.
// Every project consists of a manifest (audio timeline declaration,
// captions, reading-assist parameters, analytic ground truth) and a PCM
// envelope file. Ground truth is constructed analytically here and only
// here; the pipeline never reads it except to measure deviations.
package samplegen

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"captionx/internal/model"
)

// Default geometry for generated projects.
const (
	SampleRate  = 16000
	FrameLength = 512
	FrameHop    = 160
)

// rng is a tiny deterministic xorshift64* PRNG so sample content does not
// depend on stdlib PRNG stability guarantees.
type rng struct{ s uint64 }

func newRNG(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9e3779b97f4a7c15
	}
	return &rng{s: s}
}

func (r *rng) next() uint64 {
	r.s ^= r.s >> 12
	r.s ^= r.s << 25
	r.s ^= r.s >> 27
	return r.s * 0x2545F4914F6CDD1D
}

func (r *rng) intn(n int64) int64 { return int64(r.next() % uint64(n)) }

// cjkTable is the syllable pool used to synthesise caption text.
var cjkTable = []rune("的一是不了人我在有他这中大来上国个到说们为子和你地出道也时年得就那要下以生会自着去之过家学对可她里后小么心多天而能好都然没日于起还发成事只作当想看文无开手十用主行方又如前所本见经头面公同三已老从动两长知民样现分将外但身些与的高意进法此月正点们向理合名位应世二儿少即条联系统计间场新由全工果因四第山界内金及门风题其至平使五美万北商海书走电六车话七信八东九百感")

// AudioFile is the on-disk PCM envelope document.
type AudioFile struct {
	SampleRate int64   `json:"sampleRate"`
	Samples    []int16 `json:"samples"`
}

// Options controls generation.
type Options struct {
	OutDir string
	Count  int
	Seed   int64
}

// Generate writes Count projects under opts.OutDir as project-1..N.
func Generate(opts Options) ([]string, error) {
	if opts.Count <= 0 {
		opts.Count = 3
	}
	if opts.Seed == 0 {
		opts.Seed = 20261124
	}
	var dirs []string
	for k := 0; k < opts.Count; k++ {
		dir := filepath.Join(opts.OutDir, fmt.Sprintf("project-%d", k+1))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := genProject(dir, opts.Seed+int64(k)*7919, k+1); err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

// alignHop rounds v up to the next hop multiple.
func alignHop(v int64) int64 {
	return (v + FrameHop - 1) / FrameHop * FrameHop
}

// desiredLen is the analytic display duration for nChars characters at the
// project speech rate (characters per second, rational num/den).
func desiredLen(nChars, rateNum, rateDen int64) int64 {
	// ceil(nChars * SampleRate * den / num)
	num := nChars * SampleRate * rateDen
	return (num + rateNum - 1) / rateNum
}

func genProject(dir string, seed int64, idx int) error {
	r := newRNG(seed)

	rateNum, rateDen := int64(6), int64(1) // 6 chars/sec narration
	maxNum, maxDen := int64(10), int64(1)  // readability ceiling 10 chars/sec

	nSeg := 3 + r.intn(3) // 3..5 segments

	type capPlan struct {
		text     string
		weights  []int64
		explicit *model.Window
	}
	type segPlan struct {
		caps   []capPlan
		length int64
	}

	// Plan captions first so segments are sized to fit them.
	var segs []segPlan
	capID := 1
	for s := 0; s < int(nSeg); s++ {
		nCaps := 1 + r.intn(2) // 1..2 captions per segment
		var caps []capPlan
		var used int64
		for c := 0; c < int(nCaps); c++ {
			nCh := 6 + r.intn(11) // 6..16 runes
			runes := make([]rune, nCh)
			weights := make([]int64, nCh)
			for i := range runes {
				runes[i] = cjkTable[r.intn(int64(len(cjkTable)))]
				weights[i] = 1 + r.intn(3) // 1..3
			}
			dl := desiredLen(nCh, rateNum, rateDen)
			var explicit *model.Window
			if r.intn(10) < 3 { // ~30% get an explicit window
				explicit = &model.Window{Start: used, End: used + dl} // rebased later
			}
			caps = append(caps, capPlan{text: string(runes), weights: weights, explicit: explicit})
			used += dl
		}
		tail := alignHop(SampleRate/10 + r.intn(SampleRate/2)) // 0.1..0.6s tail
		segs = append(segs, segPlan{caps: caps, length: alignHop(used) + tail})
	}

	// Lay segments out on the timeline with silence gaps.
	var manifest model.Manifest
	manifest.ProjectID = fmt.Sprintf("project-%d", idx)
	manifest.Seed = seed
	manifest.Frame = model.FrameParams{SampleRate: SampleRate, FrameLength: FrameLength, FrameHop: FrameHop}
	manifest.Reading = model.ReadingParams{
		SpeechRateNum:  rateNum,
		SpeechRateDen:  rateDen,
		HighlightMode:  model.HighlightWeighted,
		MaxReadRateNum: maxNum,
		MaxReadRateDen: maxDen,
	}
	manifest.ToleranceSamples = FrameHop // declared window tolerance
	manifest.SegmentChunkSamples = 2 * SampleRate

	cursor := alignHop(SampleRate / 2) // 0.5s lead-in silence
	for _, sp := range segs {
		seg := model.SegmentManifest{ID: len(manifest.Segments), Start: cursor, End: cursor + sp.length}
		manifest.Segments = append(manifest.Segments, seg)
		inner := cursor
		for _, cp := range sp.caps {
			nCh := int64(len([]rune(cp.text)))
			dl := desiredLen(nCh, rateNum, rateDen)
			cap := model.CaptionManifest{
				ID:        capID,
				Text:      cp.text,
				SegmentID: seg.ID,
			}
			var win model.Window
			if cp.explicit != nil {
				win = model.Window{Start: inner, End: inner + dl}
				cap.ExplicitWindow = &win
			} else {
				win = model.Window{Start: inner, End: inner + dl}
			}
			manifest.Captions = append(manifest.Captions, cap)
			manifest.Reading.Weights = append(manifest.Reading.Weights, cp.weights)

			// Analytic ground truth: same integer rule the pipeline
			// must reproduce, evaluated on the true boundaries.
			truth := model.CaptionTruth{CaptionID: capID, Start: win.Start, End: win.End}
			var wsum int64
			for _, w := range cp.weights {
				wsum += w
			}
			var cum int64
			for i := 0; i < int(nCh); i++ {
				truth.CharStarts = append(truth.CharStarts, win.Start+win.Len()*cum/wsum)
				cum += cp.weights[i]
			}
			manifest.Truth.Captions = append(manifest.Truth.Captions, truth)
			manifest.Truth.TotalReadSamples += win.Len()

			inner += dl
			capID++
		}
		cursor = seg.End + alignHop(SampleRate/4+r.intn(SampleRate/2)) // 0.25..0.75s gap
	}
	manifest.TotalSamples = cursor

	// Synthesise the PCM envelope from the declared segments.
	samples := make([]int16, manifest.TotalSamples)
	for i := range samples {
		// low deterministic noise floor
		samples[i] = int16(120 + r.intn(160))
	}
	for _, seg := range manifest.Segments {
		segR := newRNG(seed + int64(seg.ID)*104729)
		f1 := 180 + float64(segR.intn(120))
		f2 := 2*f1 + 30 + float64(segR.intn(80))
		attack := int64(SampleRate / 50)       // 20ms
		release := int64(SampleRate * 3 / 100) // 30ms
		for t := seg.Start; t < seg.End; t++ {
			pos := t - seg.Start
			env := 0.85
			if pos < attack {
				env = 0.85 * float64(pos) / float64(attack)
			}
			if rem := seg.End - t; rem < release {
				if e := 0.85 * float64(rem) / float64(release); e < env {
					env = e
				}
			}
			ph := float64(t) / SampleRate
			v := env * (0.55*math.Sin(2*math.Pi*f1*ph) + 0.30*math.Sin(2*math.Pi*f2*ph) + 0.10*math.Sin(2*math.Pi*3*f1*ph))
			v += float64(segR.intn(120)) / 32767.0
			samples[t] = int16(v * 30000)
		}
	}

	mBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mBytes, 0o644); err != nil {
		return err
	}
	aBytes, err := json.Marshal(AudioFile{SampleRate: SampleRate, Samples: samples})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "audio.json"), aBytes, 0o644)
}

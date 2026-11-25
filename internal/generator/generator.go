package generator

import (
	"math"
	"math/rand"

	"prosodyalign/internal/model"
	"prosodyalign/internal/schema"
)

const (
	FixedSeed  int64 = 424242
	SampleRate       = 16000
)

type Truth struct {
	DialogID         string             `json:"dialog_id"`
	Seed             int64              `json:"seed"`
	EnvelopeFixed    []int64            `json:"envelope_fixed"`
	RawLoudness      []int64            `json:"raw_loudness"`
	AccentSample     int64              `json:"accent_sample"`
	LoudnessPeak     int64              `json:"loudness_peak_sample"`
	PitchPeak        int64              `json:"pitch_peak_sample"`
	AddedSamples     []int64            `json:"added_samples"`
	CapTriggered     []bool             `json:"cap_triggered"`
	StretchedRanges  []model.Range      `json:"stretched_ranges"`
	ExpectedSegments []model.SegmentFit `json:"-"`
}

type Dialog struct {
	Project *model.Project              `json:"project"`
	Docs    map[model.SchemaName][]byte `json:"docs"`
	Truth   Truth                       `json:"truth"`
}

type Suite struct {
	Seed    int64     `json:"seed"`
	Dialogs []*Dialog `json:"dialogs"`
}

type phonemeSpec struct {
	kind         model.PhonemeKind
	durationMs   int64
	weightPer10k int64
	stretchable  bool
	capSamples   *int64
}

func int64Ptr(v int64) *int64 { return &v }

func DefaultSpecs() []phonemeSpec {
	bindingCap := int64(1200)
	return []phonemeSpec{
		{model.Silence, 80, 0, false, nil},
		{model.Vowel, 180, 2000, true, nil},
		{model.Consonant, 80, 600, false, nil},
		{model.Vowel, 220, 3000, true, &bindingCap},
		{model.Silence, 100, 0, false, nil},
		{model.Consonant, 120, 1000, true, nil},
		{model.Consonant, 90, 400, false, nil},
	}
}

func GenerateSuite(seed int64, count int) (*Suite, error) {
	if count <= 0 {
		count = 4
	}
	r := rand.New(rand.NewSource(seed))
	suite := &Suite{Seed: seed, Dialogs: make([]*Dialog, 0, count)}
	for d := 0; d < count; d++ {
		dialog, err := generateDialog(seed+int64(d)*1009, d, r)
		if err != nil {
			return nil, err
		}
		suite.Dialogs = append(suite.Dialogs, dialog)
	}
	return suite, nil
}

func generateDialog(seed int64, ordinal int, r *rand.Rand) (*Dialog, error) {
	id := "dialog-" + string(rune('a'+ordinal))
	specs := DefaultSpecs()
	phonemes := make([]model.Phoneme, 0, len(specs))
	var cursor int64
	for i, spec := range specs {
		dur := spec.durationMs * (SampleRate / 1000)
		ph := model.Phoneme{
			Index: i, ID: id + "-p" + itoa(i), StartSample: cursor, EndSample: cursor + dur,
			Kind: spec.kind, Stretchable: spec.stretchable, WeightPer10k: spec.weightPer10k,
			MaxStretchSamples: spec.capSamples,
		}
		mid := dur / 2
		baseF0 := int64(105000 + r.Intn(25000))
		switch spec.kind {
		case model.Vowel:
			ph.F0 = []model.F0Point{
				{OffsetSample: 0, MilliHz: baseF0},
				{OffsetSample: mid, MilliHz: baseF0 + 45000},
				{OffsetSample: dur - 16, MilliHz: baseF0 + 8000},
			}
		case model.Consonant:
			ph.F0 = []model.F0Point{
				{OffsetSample: 0, MilliHz: baseF0 - 15000},
				{OffsetSample: mid, MilliHz: baseF0},
				{OffsetSample: dur - 16, MilliHz: baseF0 - 5000},
			}
		default:
			ph.F0 = []model.F0Point{{OffsetSample: 0, MilliHz: 90000}, {OffsetSample: mid, MilliHz: 92000}}
		}
		if i == 3 {
			ph.F0[1].MilliHz = 180000
		}
		phonemes = append(phonemes, ph)
		cursor += dur
	}
	totalSamples := cursor
	accent := phonemes[3].StartSample + (phonemes[3].DurationSamples() / 2)
	pcm := synthesizePCM(phonemes, totalSamples, accent, ordinal)

	envParams := model.DefaultEnvelopeParams(SampleRate)
	project := &model.Project{
		SchemaVersion: model.SchemaV3, SourceVersion: "generator.seed-" + itoa64(seed),
		ID: id, Seed: seed, SampleRateHz: SampleRate, SampleCount: int(totalSamples), PCM: pcm,
		DeclaredTargetDuration: totalSamples + 3600,
		DeclaredMaxStretch:     2400,
		MinSilenceHoldSamples:  800,
		AccentToleranceSamples: 480,
		EnvelopeParams:         envParams,
		Phonemes:               phonemes,
	}
	project.ContentHash = model.CanonicalContentHash(project)

	docs := make(map[model.SchemaName][]byte, 3)
	for _, v := range []model.SchemaName{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, _, err := schema.Export(project, v)
		if err != nil {
			return nil, err
		}
		docs[v] = data
	}

	truth := buildTruth(project, accent)
	return &Dialog{Project: project, Docs: docs, Truth: truth}, nil
}

func amplitudeFor(ph model.Phoneme, sample, accent int64, ordinal int) float64 {
	switch ph.Kind {
	case model.Silence:
		return 0
	case model.Consonant:
		base := 7000 + float64(ph.WeightPer10k)*7
		if ph.Stretchable {
			base += 1500
		}
		return base
	default:
		base := 9000 + float64(ph.WeightPer10k)*5
		d := float64(sample - accent)
		width := 420 + float64(ordinal%2)*80
		gauss := math.Exp(-(d * d) / (2 * width * width))
		return base * (1 + 0.38*gauss)
	}
}

func synthesizePCM(phs []model.Phoneme, n, accent int64, ordinal int) []int16 {
	pcm := make([]int16, n)
	pi := 0
	phase := 0.0
	for s := int64(0); s < n; s++ {
		for pi < len(phs)-1 && s >= phs[pi].EndSample {
			pi++
		}
		ph := phs[pi]
		amp := amplitudeFor(ph, s, accent, ordinal)
		d := float64(s - accent)
		f0 := 118.0 + 47.0*math.Exp(-(d*d)/(2*520*520))
		if ph.Kind == model.Silence {
			f0 = 90
		}
		phase += 2 * math.Pi * f0 / float64(SampleRate)
		v := amp * math.Sin(phase)
		if v > 32767 {
			v = 32767
		}
		if v < -32768 {
			v = -32768
		}
		pcm[s] = int16(math.Round(v))
	}
	return pcm
}

func buildTruth(p *model.Project, accent int64) Truth {
	length := p.EnvelopeParams.FrameLengthSamples
	hop := p.EnvelopeParams.HopSamples
	count := (len(p.PCM)-length)/hop + 1
	raw := make([]int64, count)
	for i := 0; i < count; i++ {
		start := i * hop
		var sum int64
		for j := start; j < start+length; j++ {
			v := p.PCM[j]
			if v < 0 {
				sum += -int64(v)
			} else {
				sum += int64(v)
			}
		}
		raw[i] = sum
	}
	smoothed := smoothTruth(raw, p.EnvelopeParams.SmoothWindow)
	var max int64
	for _, v := range smoothed {
		if v > max {
			max = v
		}
	}
	env := make([]int64, count)
	for i, v := range smoothed {
		if max > 0 {
			env[i] = v * p.EnvelopeParams.FixedPointScale / max
		}
	}

	added := make([]int64, len(p.Phonemes))
	capped := make([]bool, len(p.Phonemes))
	added[1] = 1600
	added[3] = 1200
	added[5] = 800
	capped[3] = true
	ranges := make([]model.Range, len(p.Phonemes))
	cursor := p.Phonemes[0].StartSample
	for i, ph := range p.Phonemes {
		if i > 0 {
			gap := ph.StartSample - p.Phonemes[i-1].EndSample
			if gap > 0 {
				cursor += gap
			}
		}
		dur := ph.DurationSamples() + added[i]
		ranges[i] = model.Range{StartSample: cursor, EndSample: cursor + dur}
		cursor += dur
	}

	return Truth{
		DialogID: p.ID, Seed: p.Seed, EnvelopeFixed: env, RawLoudness: raw,
		AccentSample: accent, LoudnessPeak: accent, PitchPeak: accent,
		AddedSamples: added, CapTriggered: capped, StretchedRanges: ranges,
	}
}

func smoothTruth(raw []int64, window int) []int64 {
	n := len(raw)
	if window <= 1 {
		return raw
	}
	if window%2 == 0 {
		window++
	}
	r := window / 2
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		lo, hi := i-r, i+r
		if lo < 0 {
			lo = 0
		}
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

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func itoa64(i int64) string { return itoa(int(i)) }

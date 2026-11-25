package generator

import (
	"encoding/json"
	"math/rand"

	"presentkernel/internal/domain"
	"presentkernel/internal/engine"
	"presentkernel/internal/quant"
	"presentkernel/internal/schema"
)

const SamplesPerFrameAt24 = 2000

type SampleSet struct {
	Seed          int64                           `json:"seed"`
	Canonical     *domain.Project                 `json:"canonical"`
	V1            []byte                          `json:"-"`
	V2            []byte                          `json:"-"`
	V3            []byte                          `json:"-"`
	Truth         []byte                          `json:"-"`
	TruthManifest *TruthFile                      `json:"truth_manifest"`
	ExportReports map[string]*schema.ExportResult `json:"-"`
}

type TruthFile struct {
	Seed              int64                     `json:"seed"`
	ProjectID         string                    `json:"project_id"`
	TargetFPS         int                       `json:"target_fps"`
	SampleRateHz      int                       `json:"sample_rate_hz"`
	QuantParamVersion string                    `json:"quant_param_version"`
	AlgorithmVersion  string                    `json:"algorithm_version"`
	Partitions        []*domain.PartitionResult `json:"partitions"`
}

type phonemeSpec struct {
	symbol   string
	category string
	frames   int
}

type segmentSpec struct {
	text      string
	phonemes  []phonemeSpec
	overrides []domain.TransitionOverride
}

type dialogueSpec struct {
	id       string
	speaker  string
	segments []segmentSpec
}

func GenerateSampleSet() (*SampleSet, error) {
	return Generate(domain.Seed)
}

func Generate(seed int64) (*SampleSet, error) {
	rng := rand.New(rand.NewSource(seed))
	specs := seededSpecs()
	p := &domain.Project{
		SchemaVersion:             domain.SchemaV3,
		ProjectID:                 "game-adept-dialogue-" + formatSeed(seed),
		Seed:                      seed,
		SampleRateHz:              48000,
		TargetFPS:                 24,
		SourceFPS:                 24,
		MinReadableFrames:         2,
		TransitionToleranceFrames: 2,
		RoundingMode:              string(quant.RoundHalfUp),
		WeightScale:               quant.ScalePermyriad,
		VolumeScale:               quant.ScalePermyriad,
		DefaultVolume:             7200,
		CategoryTable:             domain.StandardCategories(),
	}

	for di, ds := range specs {
		d := domain.Dialogue{ID: ds.id, Speaker: ds.speaker}
		for si, ss := range ds.segments {
			seg := domain.Segment{Index: si, ID: domain.MakePartitionID(ds.id, si), Text: ss.text}
			seg.CharWeights = makeWeights(ss.text, rng)
			cursorFrame := 0
			cursorSample := int64(0)
			for pi, ps := range ss.phonemes {
				startFrame := cursorFrame
				endFrame := cursorFrame + ps.frames
				startSample := cursorSample
				endSample := int64(endFrame) * SamplesPerFrameAt24
				seg.Phonemes = append(seg.Phonemes, domain.Phoneme{
					Index: pi, Symbol: ps.symbol, CategoryCode: ps.category,
					StartSample: startSample, EndSample: endSample,
					StartFrame: startFrame, EndFrame: endFrame,
				})
				cursorFrame = endFrame
				cursorSample = endSample
			}
			seg.DurationFrames = cursorFrame
			seg.DurationSamples = cursorSample
			seg.TransitionOverrides = append([]domain.TransitionOverride(nil), ss.overrides...)
			d.Segments = append(d.Segments, seg)
		}
		_ = di
		p.Dialogues = append(p.Dialogues, d)
	}

	truthResults, err := engine.BuildAll(p, engine.Options{FPS: p.TargetFPS})
	if err != nil {
		return nil, err
	}
	truth := &TruthFile{
		Seed: seed, ProjectID: p.ProjectID, TargetFPS: p.TargetFPS, SampleRateHz: p.SampleRateHz,
		QuantParamVersion: quant.ParamVersion, AlgorithmVersion: domain.AlgorithmVersion,
		Partitions: truthResults,
	}
	truthBytes, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}

	set := &SampleSet{Seed: seed, Canonical: p, Truth: truthBytes, TruthManifest: truth, ExportReports: map[string]*schema.ExportResult{}}
	for _, v := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		res, err := schema.Export(v, p)
		if err != nil {
			return nil, err
		}
		set.ExportReports[v] = res
		switch v {
		case domain.SchemaV1:
			set.V1 = res.Data
		case domain.SchemaV2:
			set.V2 = res.Data
		case domain.SchemaV3:
			set.V3 = res.Data
		}
	}
	return set, nil
}

func makeWeights(text string, rng *rand.Rand) []int {
	runes := []rune(text)
	n := len(runes)
	raw := make([]int, n)
	sum := 0
	for i := range runes {
		raw[i] = 45 + rng.Intn(120)
		sum += raw[i]
	}
	permille := make([]int, n)
	assigned := 0
	type rem struct{ i, v int }
	rems := []rem{}
	for i, x := range raw {
		num := x * 1000
		q := num / sum
		r := num % sum
		if q < 15 {
			q = 15
		}
		permille[i] = q
		assigned += q
		rems = append(rems, rem{i, r})
	}
	for assigned < 1000 {
		best := 0
		for i := 1; i < len(rems); i++ {
			if rems[i].v > rems[best].v {
				best = i
			}
		}
		permille[rems[best].i]++
		assigned++
		rems[best].v = -1
	}
	for assigned > 1000 {
		best := 0
		for i := 1; i < n; i++ {
			if permille[i] > permille[best] {
				best = i
			}
		}
		if permille[best] <= 15 {
			best = -1
			for i := range permille {
				if permille[i] > 15 {
					best = i
					break
				}
			}
		}
		permille[best]--
		assigned--
	}
	out := make([]int, n)
	for i, v := range permille {
		out[i] = v * 10
	}
	return out
}

func formatSeed(seed int64) string {
	const hex = "0123456789abcdef"
	if seed == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for seed > 0 {
		i--
		b[i] = hex[seed&15]
		seed >>= 4
	}
	return string(b[i:])
}

func seededSpecs() []dialogueSpec {
	return []dialogueSpec{
		{id: "D0", speaker: "Mira", segments: []segmentSpec{
			{
				text: "Stay close.",
				phonemes: []phonemeSpec{
					{"sil", "SIL", 2}, {"s", "SZ", 3}, {"t", "TDN", 3}, {"ey", "E", 5},
					{"iy", "I", 4}, {"k", "KG", 3}, {"l", "L", 4}, {"ow", "O", 5},
					{"uw", "U", 4}, {"sil", "SIL", 3},
				},
				overrides: []domain.TransitionOverride{{
					AtFrame: 8, FromCategory: "TDN", ToCategory: "E", BridgeCategory: "TR",
					OffsetFrames: 1, Reason: "non-continuous families alveolar -> vowel-mid",
				}},
			},
			{
				text: "Signal weak.",
				phonemes: []phonemeSpec{
					{"sil", "SIL", 3}, {"s", "SZ", 3}, {"ih", "I", 4}, {"g", "KG", 4},
					{"n", "TDN", 4}, {"l", "L", 4}, {"sil", "SIL", 2}, {"w", "U", 5},
					{"iy", "I", 4}, {"k", "KG", 4}, {"sil", "SIL", 5},
				},
			},
		}},
		{id: "D1", speaker: "Narrator", segments: []segmentSpec{
			{
				text: "The gate opens.",
				phonemes: []phonemeSpec{
					{"sil", "SIL", 3}, {"dh", "TDN", 3}, {"ah", "E", 4}, {"g", "KG", 4},
					{"ey", "E", 4}, {"t", "TDN", 3}, {"sil", "SIL", 2}, {"ow", "O", 5},
					{"p", "MBP", 4}, {"eh", "E", 4}, {"n", "TDN", 3}, {"z", "SZ", 3},
					{"sil", "SIL", 6},
				},
			},
			{
				text: "Dawn breaks.",
				phonemes: []phonemeSpec{
					{"sil", "SIL", 3}, {"d", "TDN", 3}, {"ao", "O", 5}, {"n", "TDN", 3},
					{"sil", "SIL", 2}, {"b", "MBP", 4}, {"r", "L", 4}, {"ey", "E", 5},
					{"k", "KG", 4}, {"s", "SZ", 3}, {"sil", "SIL", 6},
				},
			},
		}},
		{id: "D2", speaker: "Captain", segments: []segmentSpec{
			{
				text: "Three shields left.",
				phonemes: []phonemeSpec{
					{"sil", "SIL", 3}, {"th", "FV", 4}, {"r", "L", 4}, {"iy", "I", 5},
					{"sh", "SZ", 4}, {"iy", "I", 5}, {"l", "L", 4}, {"d", "TDN", 3},
					{"z", "SZ", 3}, {"sil", "SIL", 2}, {"l", "L", 3}, {"eh", "E", 4},
					{"f", "FV", 4}, {"t", "TDN", 3}, {"sil", "SIL", 6},
				},
			},
			{
				text: "Hold the ridge!",
				phonemes: []phonemeSpec{
					{"sil", "SIL", 3}, {"hh", "O", 5}, {"l", "L", 4}, {"d", "TDN", 3},
					{"dh", "TDN", 3}, {"ah", "E", 3}, {"r", "L", 4}, {"iy", "I", 4},
					{"jh", "SZ", 5}, {"d", "TDN", 4}, {"sil", "SIL", 10},
				},
			},
		}},
	}
}

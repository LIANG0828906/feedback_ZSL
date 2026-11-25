package dialoguecal

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
)

type Fixture struct {
	Seed     int64           `json:"seed"`
	Project  *Project        `json:"project"`
	Frames   []Frame         `json:"frames"`
	V1JSON   json.RawMessage `json:"v1_json"`
	V2JSON   json.RawMessage `json:"v2_json"`
	V3JSON   json.RawMessage `json:"v3_json"`
	Truth    *TruthSet       `json:"truth"`
	Critical *CriticalSuite  `json:"critical"`
}

func dbX100FromPermille(permille int) int {
	db := 20 * math.Log10(float64(permille)/1000.0)
	return int(math.Round(db * 100))
}

func gainDBMicroFromPermille(permille int) int64 {
	db := 20 * math.Log10(float64(permille)/1000.0)
	return int64(math.Round(db * 100000))
}

// GenerateFixture builds the deterministic fixed-seed dialogue. The generator
// owns both the source representations and the independent float truth.
func GenerateFixture(seed int64) (*Fixture, error) {
	rng := rand.New(rand.NewSource(seed))
	const sampleRate = 16000
	const segmentSamples = 6400
	const segmentCount = 4
	totalSamples := segmentSamples * segmentCount

	gains := []int{1000, 1125, 875, 1250}
	targets := []int{100, 126, 79, 158}
	segments := make([]Segment, 0, segmentCount)
	for i := 0; i < segmentCount; i++ {
		segments = append(segments, Segment{
			ID: fmt.Sprintf("seg-%d", i), Index: i,
			StartSample: i * segmentSamples, EndSample: (i + 1) * segmentSamples,
			GainPermille: gains[i], TargetLevelPermille: targets[i],
		})
	}

	durations := []int{1600, 1600, 1600, 1920, 1280, 1600, 1600, 1600, 1600, 1600, 1600, 1600, 1600, 1600, 1600, 1600}
	classes := []PhonemeClass{
		ClassVowel, ClassConsonant, ClassVowel, ClassVowel,
		ClassConsonant, ClassVowel, ClassSilence, ClassVowel,
		ClassConsonant, ClassVowel, ClassVowel, ClassSilence,
		ClassVowel, ClassConsonant, ClassVowel, ClassVowel,
	}
	weights := []int{1000, 950, 1050, 900, 1000, 1100, 1000, 980, 960, 1040, 1080, 1000, 990, 940, 1020, 1060}
	phonemes := make([]Phoneme, 0, len(durations))
	cursor := 0
	for i, duration := range durations {
		start, end := cursor, cursor+duration
		cursor = end
		f0Start, f0End := 0, 0
		if classes[i] == ClassVowel {
			f0Start = 105 + (i%5)*17
			f0End = f0Start + (i%3-1)*14
		}
		segmentIndex := start / segmentSamples
		if segmentIndex >= segmentCount {
			segmentIndex = segmentCount - 1
		}
		phonemes = append(phonemes, Phoneme{
			ID: fmt.Sprintf("ph-%02d", i), SegmentID: fmt.Sprintf("seg-%d", segmentIndex),
			SyllableID:  fmt.Sprintf("syl-%d", i/2),
			StartSample: start, EndSample: end, Class: classes[i],
			F0StartHzQ8: Q8(f0Start * 256), F0EndHzQ8: Q8(f0End * 256),
			WeightPermille: weights[i],
		})
	}

	pcm := make([]int16, totalSamples)
	for i, ph := range phonemes {
		duration := ph.EndSample - ph.StartSample
		phase := 0.0
		for t := 0; t < duration; t++ {
			idx := ph.StartSample + t
			switch ph.Class {
			case ClassSilence:
				pcm[idx] = 0
			case ClassConsonant:
				noise := float64(rng.Intn(4201) - 2100)
				envelope := math.Min(1, float64(t)/60) * math.Min(1, float64(duration-t)/60)
				pcm[idx] = int16(noise * envelope)
			default:
				progress := float64(t) / float64(duration)
				f0 := float64(ph.F0StartHzQ8)/256.0 + (float64(ph.F0EndHzQ8-ph.F0StartHzQ8)/256.0)*progress
				phase += 2 * math.Pi * f0 / float64(sampleRate)
				amplitude := 10500 + float64((i*37)%3500)
				envelope := math.Min(1, float64(t)/80) * math.Min(1, float64(duration-t)/80)
				sample := amplitude * envelope * (0.72*math.Sin(phase) + 0.18*math.Sin(2*phase) + 0.10*math.Sin(3*phase))
				pcm[idx] = int16(math.Round(sample))
			}
		}
	}

	p := &Project{
		ProjectID: "fixed-seed-dialogue", Seed: seed, SampleRateHz: sampleRate, PCM: pcm,
		FramesPerSecond: 50, FrameShiftSamples: 320, FrameWindowSamples: 320,
		EnergyCapPermille: 250, Segments: segments, Phonemes: phonemes,
	}
	if err := NormalizeProject(p); err != nil {
		return nil, err
	}
	frames, err := BuildFrames(p)
	if err != nil {
		return nil, err
	}
	v1, err := MarshalProjectAs(p, SchemaV1)
	if err != nil {
		return nil, err
	}
	v2, err := MarshalProjectAs(p, SchemaV2)
	if err != nil {
		return nil, err
	}
	v3, err := MarshalProjectAs(p, SchemaV3)
	if err != nil {
		return nil, err
	}
	truth := GenerateTruth(p, frames)
	critical := GenerateCriticalSuite()
	return &Fixture{Seed: seed, Project: p, Frames: frames, V1JSON: v1, V2JSON: v2, V3JSON: v3, Truth: truth, Critical: critical}, nil
}

func MarshalProjectAs(p *Project, schema string) ([]byte, error) {
	switch schema {
	case SchemaV1:
		return json.MarshalIndent(projectToV1(p), "", "  ")
	case SchemaV2:
		return json.MarshalIndent(projectToV2(p), "", "  ")
	case SchemaV3:
		return json.MarshalIndent(projectToV3(p), "", "  ")
	default:
		return nil, newError(CodeSchemaUnknown, "schema", "unknown export schema %q", schema)
	}
}

func projectToV1(p *Project) dtoV1 {
	doc := dtoV1{
		Schema: SchemaV1, DialogueID: p.ProjectID, Seed: p.Seed, SampleRateHz: p.SampleRateHz,
		FramesPerSecond: p.FramesPerSecond, FrameShiftMs: p.FrameShiftSamples * 1000 / p.SampleRateHz,
		FrameWindowMs: p.FrameWindowSamples * 1000 / p.SampleRateHz, EnergyCapPermille: p.EnergyCapPermille,
		PCMBase64: encodePCM(p.PCM),
	}
	for _, s := range p.Segments {
		watermark := s.WatermarkDBX100
		doc.Segments = append(doc.Segments, dtoV1Segment{
			ID: s.ID, StartMs: s.StartSample * 1000 / p.SampleRateHz, EndMs: s.EndSample * 1000 / p.SampleRateHz,
			GainPermille: s.GainPermille, TargetLoudnessDBX100: dbX100FromPermille(s.TargetLevelPermille),
			CalibrationWatermarkDBX100: nil, // V1 intentionally omits the optional watermark.
		})
		_ = watermark
	}
	for _, ph := range p.Phonemes {
		doc.PhonemesFlat = append(doc.PhonemesFlat, dtoV1Phoneme{
			ID: ph.ID, SyllableID: ph.SyllableID,
			StartMs: ph.StartSample * 1000 / p.SampleRateHz, EndMs: ph.EndSample * 1000 / p.SampleRateHz,
			Class: string(ph.Class), F0StartHzX100: int(int64(ph.F0StartHzQ8)/Q8Scale) * 100, F0EndHzX100: int(int64(ph.F0EndHzQ8)/Q8Scale) * 100,
			WeightPermille: ph.WeightPermille,
		})
	}
	return doc
}

func projectToV2(p *Project) dtoV2 {
	const ticksPerSecond = 1000
	sampleToTick := func(samples int) int { return samples * ticksPerSecond / p.SampleRateHz }
	doc := dtoV2{
		Schema: SchemaV2, DialogueID: p.ProjectID, Seed: p.Seed, SampleRateHz: p.SampleRateHz,
		TicksPerSecond: ticksPerSecond, FramesPerSecond: p.FramesPerSecond,
		FrameShiftTicks: sampleToTick(p.FrameShiftSamples), FrameWindowTicks: sampleToTick(p.FrameWindowSamples),
		EnergyCapPer10k: p.EnergyCapPermille * 10, PCMBase64: encodePCM(p.PCM),
	}
	for _, s := range p.Segments {
		dtoSeg := dtoV2Segment{
			ID: s.ID, StartTick: sampleToTick(s.StartSample), DurationTicks: sampleToTick(s.EndSample - s.StartSample),
			GainPerTenThousand: s.GainPermille * 10, TargetAmplitudePer10k: s.TargetLevelPermille * 10,
		}
		syllableOrder := []string{}
		syllablePhonemes := map[string][]Phoneme{}
		syllableMinStart := map[string]int{}
		for _, ph := range p.Phonemes {
			if ph.SegmentID != s.ID {
				continue
			}
			if _, ok := syllablePhonemes[ph.SyllableID]; !ok {
				syllableOrder = append(syllableOrder, ph.SyllableID)
				syllableMinStart[ph.SyllableID] = ph.StartSample
			}
			syllablePhonemes[ph.SyllableID] = append(syllablePhonemes[ph.SyllableID], ph)
			if ph.StartSample < syllableMinStart[ph.SyllableID] {
				syllableMinStart[ph.SyllableID] = ph.StartSample
			}
		}
		for _, sylID := range syllableOrder {
			minStart := syllableMinStart[sylID]
			syl := dtoV2Syllable{ID: sylID, OffsetTicks: sampleToTick(minStart - s.StartSample)}
			for _, ph := range syllablePhonemes[sylID] {
				syl.Phonemes = append(syl.Phonemes, dtoV2Phoneme{
					ID: ph.ID, OffsetTicks: sampleToTick(ph.StartSample - minStart),
					DurationTicks: sampleToTick(ph.EndSample - ph.StartSample), Class: string(ph.Class),
					F0StartHzX100: int(int64(ph.F0StartHzQ8)/Q8Scale) * 100, F0EndHzX100: int(int64(ph.F0EndHzQ8)/Q8Scale) * 100,
					WeightPermille: ph.WeightPermille,
				})
			}
			dtoSeg.Syllables = append(dtoSeg.Syllables, syl)
		}
		doc.Segments = append(doc.Segments, dtoSeg)
	}
	return doc
}

func projectToV3(p *Project) dtoV3 {
	watermarks := map[string]*int{}
	for _, s := range p.Segments {
		w := s.WatermarkDBX100
		watermarks[s.ID] = &w
	}
	doc := dtoV3{
		Schema: SchemaV3, DialogueID: p.ProjectID, Seed: p.Seed, SampleRateHz: p.SampleRateHz,
		FramesPerSecond: p.FramesPerSecond, FrameShiftSamples: p.FrameShiftSamples, FrameWindowSamples: p.FrameWindowSamples,
		EnergyCapPermille: p.EnergyCapPermille, PCMBase64: encodePCM(p.PCM),
	}
	for _, s := range p.Segments {
		doc.Segments = append(doc.Segments, dtoV3Segment{
			ID: s.ID, StartSample: s.StartSample, EndSample: s.EndSample,
			GainDBTimes1e5: gainDBMicroFromPermille(s.GainPermille), TargetLoudnessDBX100: dbX100FromPermille(s.TargetLevelPermille),
			CalibrationWatermarkDBX100: watermarks[s.ID],
		})
	}
	for _, ph := range p.Phonemes {
		doc.Phonemes = append(doc.Phonemes, dtoV3Phoneme{
			ID: ph.ID, SyllableID: ph.SyllableID, StartSample: ph.StartSample, EndSample: ph.EndSample,
			Class: string(ph.Class), F0StartHzQ8: ph.F0StartHzQ8, F0EndHzQ8: ph.F0EndHzQ8, WeightPermille: ph.WeightPermille,
		})
	}
	return doc
}

package schema

import (
	"time"

	"lip-pipeline-prototype/internal/domain"
)

func fixedLoadTime() time.Time {
	return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
}

func pcmForShard(shard domain.Shard, sampleRate int, seed int64) domain.PCMDescription {
	return domain.PCMDescription{
		Encoding: "pcm_s16le_mono", Channels: 1, SampleRate: sampleRate, BitsPerSample: 16,
		SampleCount: shard.TotalSamples, ByteCount: shard.TotalSamples * 2,
		Deterministic: true, GeneratorSeed: seed,
	}
}

func pcmForDialogue(dlg domain.Dialogue, sampleRate int, seed, samples int64) domain.PCMDescription {
	return domain.PCMDescription{
		Encoding: "pcm_s16le_mono", Channels: 1, SampleRate: sampleRate, BitsPerSample: 16,
		SampleCount: samples, ByteCount: samples * 2, Deterministic: true, GeneratorSeed: seed,
	}
}

func categoryFromText(codeOrCategory string) (domain.Category, bool) {
	switch domain.Category(codeOrCategory) {
	case domain.CategoryVowel, domain.CategoryConsonant, domain.CategorySilence:
		return domain.Category(codeOrCategory), true
	default:
		return "", false
	}
}

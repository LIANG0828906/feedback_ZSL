package generate

import (
	"fmt"
	"time"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

const DefaultSeed int64 = 420420

type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed) + 0x9E3779B97F4A7C15}
}

func (r *rng) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func fixedTime() time.Time {
	return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
}

func Project() *domain.Project {
	r := newRNG(DefaultSeed)
	params := domain.DefaultParams()
	p := &domain.Project{
		ID:            "lip-deterministic-project",
		Seed:          DefaultSeed,
		Params:        params,
		SourceVersion: domain.SchemaV3,
		GeneratedAt:   fixedTime(),
		Channels: []domain.ChannelSpec{
			{ID: "ch-a", Speaker: "SPK-ALPHA", Slots: 2},
			{ID: "ch-b", Speaker: "SPK-BETA", Slots: 1},
		},
	}

	specs := []struct {
		id, title, speaker, channel string
		shards                      int
	}{
		{"dlg-101", "早安管线", "SPK-ALPHA", "ch-a", 2},
		{"dlg-102", "逐帧一致性", "SPK-ALPHA", "ch-a", 2},
		{"dlg-201", "断点续演", "SPK-BETA", "ch-b", 2},
	}

	for di, spec := range specs {
		dlg := domain.Dialogue{
			ID: spec.id, Title: spec.title, Speaker: spec.speaker, ChannelID: spec.channel,
			SourceVersion: domain.SchemaV3,
		}
		var pcmSamples int64
		for si := 0; si < spec.shards; si++ {
			shard := makeShard(r, di, si, spec.id, spec.channel, params)
			pcmSamples += shard.TotalSamples
			dlg.Shards = append(dlg.Shards, shard)
		}
		dlg.PCM = domain.PCMDescription{
			Encoding: "pcm_s16le_mono", Channels: 1, SampleRate: params.SampleRate,
			BitsPerSample: 16, SampleCount: pcmSamples, ByteCount: pcmSamples * 2,
			Deterministic: true, GeneratorSeed: DefaultSeed,
		}
		for si := range dlg.Shards {
			dlg.Shards[si].PCM = dlg.PCM
			dlg.Shards[si].PCM.SampleCount = dlg.Shards[si].TotalSamples
			dlg.Shards[si].PCM.ByteCount = dlg.Shards[si].TotalSamples * 2
		}
		dlg.ContentHash = hashx.JSON(struct {
			ID     string
			Shards []domain.Shard
		}{dlg.ID, dlg.Shards})
		p.Dialogues = append(p.Dialogues, dlg)
	}
	return p
}

func makeShard(r *rng, dialogueOrdinal, shardOrdinal int, dialogueID, channelID string, params domain.Params) domain.Shard {
	sr := params.SampleRate
	msToSamples := func(ms int64) int64 { return ms * int64(sr) / 1000 }
	shardID := fmt.Sprintf("%s-s%d", dialogueID, shardOrdinal+1)
	shard := domain.Shard{
		ID: shardID, DialogueID: dialogueID, Index: shardOrdinal,
		ChannelID: channelID,
	}
	targetCount := 8 + r.intn(3)
	var phonemes []domain.Phoneme
	var cursor int64
	index := 0

	add := func(category domain.Category, code string, group string, durationMS int64) {
		samples := msToSamples(durationMS)
		phoneme := domain.Phoneme{
			ID:              fmt.Sprintf("%s-p%02d", shardID, index+1),
			DialogueID:      dialogueID,
			ShardID:         shardID,
			Index:           index,
			StartSample:     cursor,
			EndSample:       cursor + samples,
			Category:        category,
			CategoryCode:    code,
			EncodingGroup:   group,
			WeightPermyriad: 600 + r.intn(160)*10,
		}
		phonemes = append(phonemes, phoneme)
		cursor += samples
		index++
	}

	vowelCodes := []string{"A", "E", "O"}
	consonantCodes := []string{"K", "M", "T", "S"}
	for len(phonemes) < targetCount {
		wordSize := 2 + r.intn(2)
		for j := 0; j < wordSize && len(phonemes) < targetCount; j++ {
			if j%2 == 0 {
				code := consonantCodes[r.intn(len(consonantCodes))]
				add(domain.CategoryConsonant, code, "g-consonant", int64(200+r.intn(3)*100))
			} else {
				code := vowelCodes[r.intn(len(vowelCodes))]
				add(domain.CategoryVowel, code, "g-vowel", int64(200+r.intn(3)*100))
			}
		}
		if len(phonemes) < targetCount {
			add(domain.CategorySilence, "SIL", "g-silence", int64(200+r.intn(2)*100))
		}
	}

	affinities := []int{2400, 4100, 6900, 8700}
	for i := range phonemes {
		if i == len(phonemes)-1 {
			phonemes[i].AffinityToNext = 0
			phonemes[i].AffinityExplicit = false
			break
		}
		if phonemes[i].Category == domain.CategorySilence || phonemes[i+1].Category == domain.CategorySilence {
			phonemes[i].AffinityToNext = 0
		} else {
			phonemes[i].AffinityToNext = affinities[r.intn(len(affinities))]
		}
		phonemes[i].AffinityExplicit = true
	}

	shard.Phonemes = phonemes
	shard.TotalSamples = cursor
	return shard
}

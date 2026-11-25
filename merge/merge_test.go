package merge_test

import (
	"math/rand"
	"reflect"
	"testing"

	"lipsync/domain"
	"lipsync/merge"
)

func config() domain.RenderConfig {
	return domain.RenderConfig{
		TargetFPS: 50, Rounding: domain.RoundFloor,
		FrameBudget: 4, ChannelSlots: 2, MinVisibleFrames: 2,
		MaxPhonemesPerPackage: 3, AffinityThreshold: 0.5, MaxDelayFrames: 3,
	}
}

func dialogue() domain.Dialogue {
	return domain.Dialogue{
		ID: "d-test", ShardIndex: 7, Speaker: "A", Channel: 0,
		PCM: domain.PCMInfo{SampleRate: 48000, Channels: 1, Samples: 48000},
		Phonemes: []domain.Phoneme{
			{ID: "p1", StartFrame: 0, EndFrame: 5, Category: domain.CategoryVowel, CategoryCode: "V", WeightPermyriad: 1000, Affinity: 0.9},
			{ID: "p2", StartFrame: 5, EndFrame: 10, Category: domain.CategoryVowel, CategoryCode: "V", WeightPermyriad: 1000, Affinity: 0.8},
			{ID: "p3", StartFrame: 10, EndFrame: 16, Category: domain.CategoryConsonant, CategoryCode: "C", WeightPermyriad: 1200, Affinity: 0.9},
			{ID: "p4", StartFrame: 16, EndFrame: 20, Category: domain.CategoryVowel, CategoryCode: "V", WeightPermyriad: 800, Affinity: 0.8},
			{ID: "p5", StartFrame: 20, EndFrame: 24, Category: domain.CategorySilence, CategoryCode: "S", WeightPermyriad: 800, Affinity: 0.1},
			{ID: "p6", StartFrame: 24, EndFrame: 30, Category: domain.CategoryConsonant, CategoryCode: "C", WeightPermyriad: 1200, Affinity: 0.8},
			{ID: "p7", StartFrame: 30, EndFrame: 35, Category: domain.CategoryVowel, CategoryCode: "V", WeightPermyriad: 1000, Affinity: 0.0},
		},
	}
}

func TestPartitionIndependentOfEnumerationAndChunkOrder(t *testing.T) {
	d := dialogue()
	want, err := merge.Partition(d, config())
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 4 {
		t.Fatalf("package count = %d, want 4: %+v", len(want), want)
	}
	for _, pkg := range want {
		for _, id := range pkg.PhonemeIDs {
			if id == "p5" && len(pkg.PhonemeIDs) != 1 {
				t.Fatalf("silence crossed a package boundary: %+v", pkg)
			}
		}
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 50; i++ {
		shuffled := d
		shuffled.Phonemes = append([]domain.Phoneme(nil), d.Phonemes...)
		rng.Shuffle(len(shuffled.Phonemes), func(a, b int) {
			shuffled.Phonemes[a], shuffled.Phonemes[b] = shuffled.Phonemes[b], shuffled.Phonemes[a]
		})
		got, err := merge.Partition(shuffled, config())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d changed partition\ngot=%+v\nwant=%+v", i, got, want)
		}
	}
	chunksA := [][]domain.Phoneme{
		d.Phonemes[:2],
		d.Phonemes[2:5],
		d.Phonemes[5:],
	}
	chunksB := [][]domain.Phoneme{
		d.Phonemes[5:],
		d.Phonemes[2:5],
		d.Phonemes[:2],
	}
	gotA, err := merge.PartitionBatched(d, config(), chunksA)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := merge.PartitionBatched(d, config(), chunksB)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotA, want) || !reflect.DeepEqual(gotA, gotB) {
		t.Fatalf("chunk completion order changed partition")
	}
}

func TestOrderClassAndAffinityErrors(t *testing.T) {
	d := dialogue()
	badOrder := d
	badOrder.Phonemes = append([]domain.Phoneme(nil), d.Phonemes...)
	badOrder.Phonemes[2].EndFrame = badOrder.Phonemes[2].StartFrame - 1
	_, err := merge.Partition(badOrder, config())
	expectCode(t, err, domain.CodeOrder, d.ShardIndex, "p3")

	badClass := d
	badClass.Phonemes = append([]domain.Phoneme(nil), d.Phonemes...)
	badClass.Phonemes[1].Category = "nasal"
	_, err = merge.Partition(badClass, config())
	expectCode(t, err, domain.CodeClass, d.ShardIndex, "p2")

	badAffinity := d
	badAffinity.Phonemes = append([]domain.Phoneme(nil), d.Phonemes...)
	badAffinity.Phonemes[0].Affinity = 1.2
	_, err = merge.Partition(badAffinity, config())
	expectCode(t, err, domain.CodeParam, d.ShardIndex, "p1")
}

func expectCode(t *testing.T, err error, code domain.Code, shard int, phone string) {
	t.Helper()
	if domain.ErrorCode(err) != code {
		t.Fatalf("code = %s, want %s: %v", domain.ErrorCode(err), code, err)
	}
	de, _ := domain.AsError(err)
	if de.ShardIndex != shard || de.PhonemeID != phone {
		t.Fatalf("error location = %+v", de)
	}
}

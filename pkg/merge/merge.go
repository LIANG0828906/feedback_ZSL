package merge

import (
	"fmt"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

type Config struct {
	AffinityThreshold     int
	MaxPhonemesPerPackage int
}

// Merge partitions adjacent phonemes into deterministic lip packages. It
// normalizes input order before making decisions, so caller enumeration order
// cannot change package boundaries.
func Merge(dialogueID, channel string, input []domain.QuantizedPhoneme, cfg Config) ([]domain.LipPackage, error) {
	if cfg.MaxPhonemesPerPackage < 1 || cfg.MaxPhonemesPerPackage > 32 {
		return nil, &domain.Error{
			Code: domain.ErrPara, DialogueID: dialogueID,
			Field: "max_phonemes_per_package", Reason: "must be in [1,32]",
		}
	}
	if cfg.AffinityThreshold < 0 || cfg.AffinityThreshold > 1000 {
		return nil, &domain.Error{
			Code: domain.ErrPara, DialogueID: dialogueID,
			Field: "affinity_threshold", Reason: "must be in [0,1000]",
		}
	}
	if len(input) == 0 {
		return nil, &domain.Error{
			Code: domain.ErrSchema, DialogueID: dialogueID,
			Field: "phonemes", Reason: "at least one phoneme required",
		}
	}
	phonemes := append([]domain.QuantizedPhoneme(nil), input...)
	sort.Slice(phonemes, func(i, j int) bool {
		if phonemes[i].StartFrame != phonemes[j].StartFrame {
			return phonemes[i].StartFrame < phonemes[j].StartFrame
		}
		if phonemes[i].StartSample != phonemes[j].StartSample {
			return phonemes[i].StartSample < phonemes[j].StartSample
		}
		return phonemes[i].PhonemeID < phonemes[j].PhonemeID
	})
	seen := make(map[string]struct{}, len(phonemes))
	for i := range phonemes {
		if phonemes[i].PhonemeID == "" {
			return nil, &domain.Error{Code: domain.ErrSchema, DialogueID: dialogueID, Field: "phoneme_id", Reason: "missing phoneme id"}
		}
		if _, ok := seen[phonemes[i].PhonemeID]; ok {
			return nil, &domain.Error{Code: domain.ErrSchema, DialogueID: dialogueID, PhonemeID: phonemes[i].PhonemeID, Reason: "duplicate phoneme id"}
		}
		seen[phonemes[i].PhonemeID] = struct{}{}
		if phonemes[i].EndFrame <= phonemes[i].StartFrame {
			return nil, &domain.Error{Code: domain.ErrOrder, DialogueID: dialogueID, PhonemeID: phonemes[i].PhonemeID, Field: "frame_interval", Reason: "invalid quantized interval"}
		}
		if i > 0 && phonemes[i].StartFrame < phonemes[i-1].EndFrame {
			return nil, &domain.Error{
				Code: domain.ErrOrder, DialogueID: dialogueID,
				PhonemeA: phonemes[i-1].PhonemeID, PhonemeB: phonemes[i].PhonemeID,
				Field: "frame_interval", Reason: "quantized phonemes overlap",
			}
		}
		if phonemes[i].AffinityToNext < 0 || phonemes[i].AffinityToNext > 1000 {
			next := ""
			if i+1 < len(phonemes) {
				next = phonemes[i+1].PhonemeID
			}
			return nil, &domain.Error{
				Code: domain.ErrPara, DialogueID: dialogueID,
				PhonemeA: phonemes[i].PhonemeID, PhonemeB: next,
				Field: "affinity_to_next", Reason: "must be in [0,1000]",
			}
		}
	}

	type group struct {
		start int
		end   int
	}
	groups := make([]group, 0, len(phonemes))
	start := 0
	for i := 1; i < len(phonemes); i++ {
		prev, cur := phonemes[i-1], phonemes[i]
		canMerge := len(phonemes[start:i]) < cfg.MaxPhonemesPerPackage
		canMerge = canMerge &&
			prev.Category != domain.CategorySilence &&
			cur.Category != domain.CategorySilence &&
			prev.AffinityToNext >= cfg.AffinityThreshold
		if !canMerge {
			groups = append(groups, group{start: start, end: i})
			start = i
		}
	}
	groups = append(groups, group{start: start, end: len(phonemes)})

	packages := make([]domain.LipPackage, 0, len(groups))
	for i, g := range groups {
		pkg := domain.LipPackage{
			DialogueID: dialogueID,
			ID:         fmt.Sprintf("%s/pkg/%03d", dialogueID, i),
			Channel:    channel,
			StartFrame: phonemes[g.start].StartFrame,
			EndFrame:   phonemes[g.end-1].EndFrame,
		}
		for j := g.start; j < g.end; j++ {
			pkg.PhonemeIDs = append(pkg.PhonemeIDs, phonemes[j].PhonemeID)
			pkg.PhonemeIndex = append(pkg.PhonemeIndex, j)
			pkg.Categories = append(pkg.Categories, phonemes[j].Category)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

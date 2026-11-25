package merge

import (
	"fmt"
	"sort"
	"strconv"

	"lipsync/domain"
)

// Partition converts a dialogue's phoneme annotations into deterministic
// mouth packages. Input enumeration order is irrelevant: validation and
// partition selection always operate on a stable (start, id) ordering.
func Partition(dialogue domain.Dialogue, cfg domain.RenderConfig) ([]domain.Package, error) {
	if cfg.MaxPhonemesPerPackage <= 0 {
		return nil, &domain.Error{
			Code: domain.CodeParam, Message: "max phonemes per package must be positive",
			Field:      "render.max_phonemes_per_package",
			ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
		}
	}
	if cfg.AffinityThreshold < 0 || cfg.AffinityThreshold > 1 {
		return nil, &domain.Error{
			Code: domain.CodeParam, Message: "affinity threshold outside [0,1]",
			Field:      "render.affinity_threshold",
			Value:      strconv.FormatFloat(cfg.AffinityThreshold, 'g', -1, 64),
			ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
		}
	}

	phones := append([]domain.Phoneme(nil), dialogue.Phonemes...)
	sort.Slice(phones, func(i, j int) bool {
		if phones[i].StartFrame != phones[j].StartFrame {
			return phones[i].StartFrame < phones[j].StartFrame
		}
		if phones[i].EndFrame != phones[j].EndFrame {
			return phones[i].EndFrame < phones[j].EndFrame
		}
		return phones[i].ID < phones[j].ID
	})

	seen := make(map[string]struct{}, len(phones))
	var previousEnd int64 = -1
	for _, ph := range phones {
		if _, ok := seen[ph.ID]; ok || ph.ID == "" {
			return nil, &domain.Error{
				Code: domain.CodeOrder, Message: "empty or duplicate phoneme id",
				Field: "phoneme.id", PhonemeID: ph.ID,
				ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
			}
		}
		seen[ph.ID] = struct{}{}
		if ph.StartFrame < 0 || ph.EndFrame <= ph.StartFrame {
			return nil, &domain.Error{
				Code: domain.CodeOrder, Message: "phoneme interval is inverted or empty",
				Field: "phoneme.interval", PhonemeID: ph.ID,
				ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
			}
		}
		if ph.StartFrame < previousEnd {
			return nil, &domain.Error{
				Code: domain.CodeOrder, Message: "phoneme intervals overlap or are out of order",
				Field: "phoneme.start_frame", PhonemeID: ph.ID,
				ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
			}
		}
		if !ph.Category.Valid() {
			return nil, &domain.Error{
				Code: domain.CodeClass, Message: "unknown phoneme category",
				Field: "phoneme.category", PhonemeID: ph.ID,
				Value:      string(ph.Category),
				ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
			}
		}
		if ph.Affinity < 0 || ph.Affinity > 1 {
			return nil, &domain.Error{
				Code: domain.CodeParam, Message: "transition affinity outside [0,1]",
				Field: "phoneme.affinity", PhonemeID: ph.ID,
				Value:      strconv.FormatFloat(ph.Affinity, 'g', -1, 64),
				ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
			}
		}
		if ph.WeightPermyriad < 0 {
			return nil, &domain.Error{
				Code: domain.CodeParam, Message: "negative duration weight",
				Field: "phoneme.duration_weight", PhonemeID: ph.ID,
				ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
			}
		}
		previousEnd = ph.EndFrame
	}

	if len(phones) == 0 {
		return nil, &domain.Error{
			Code: domain.CodeOrder, Message: "dialogue contains no phonemes",
			Field:      "dialogues.phonemes",
			ShardIndex: dialogue.ShardIndex, DialogueID: dialogue.ID,
		}
	}

	var out []domain.Package
	start := 0
	for start < len(phones) {
		end := start + 1
		if phones[start].Category != domain.CategorySilence {
			for end < len(phones) {
				last := phones[end-1]
				next := phones[end]
				if next.Category == domain.CategorySilence ||
					last.Category == domain.CategorySilence {
					break
				}
				if last.Affinity < cfg.AffinityThreshold {
					break
				}
				if end-start >= cfg.MaxPhonemesPerPackage {
					break
				}
				// A package cannot silently absorb a timeline gap.
				if next.StartFrame != last.EndFrame {
					break
				}
				end++
			}
		}
		pkg := makePackage(dialogue, phones[start:end])
		out = append(out, pkg)
		start = end
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].StartFrame != out[j].StartFrame {
			return out[i].StartFrame < out[j].StartFrame
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func makePackage(dialogue domain.Dialogue, phones []domain.Phoneme) domain.Package {
	ids := make([]string, len(phones))
	for i := range phones {
		ids[i] = phones[i].ID
	}
	id := fmt.Sprintf("pkg:%s:%d:%s:%s", dialogue.ID, dialogue.ShardIndex, ids[0], ids[len(ids)-1])
	pkg := domain.Package{
		ID:         id,
		DialogueID: dialogue.ID,
		ShardIndex: dialogue.ShardIndex,
		Channel:    dialogue.Channel,
		StartFrame: phones[0].StartFrame,
		EndFrame:   phones[len(phones)-1].EndFrame,
		PhonemeIDs: ids,
	}
	sum, _ := domain.Hash(struct {
		ID           string   `json:"id"`
		StartFrame   int64    `json:"start_frame"`
		EndFrame     int64    `json:"end_frame"`
		PhonemeIDs   []string `json:"phoneme_ids"`
		CategoryHash string   `json:"category_hash"`
	}{
		ID:           pkg.ID,
		StartFrame:   pkg.StartFrame,
		EndFrame:     pkg.EndFrame,
		PhonemeIDs:   pkg.PhonemeIDs,
		CategoryHash: categories(phones),
	})
	pkg.Summary = sum
	return pkg
}

func categories(phones []domain.Phoneme) string {
	out := ""
	for _, p := range phones {
		out += string(p.Category) + "|"
	}
	return out
}

// PartitionBatched is a collection boundary for callers that parse or compute
// separate chunks. Chunk completion order cannot affect the result because all
// phonemes are recollected and globally ordered before partitioning.
func PartitionBatched(dialogue domain.Dialogue, cfg domain.RenderConfig, chunks [][]domain.Phoneme) ([]domain.Package, error) {
	dialogue.Phonemes = nil
	for _, chunk := range chunks {
		dialogue.Phonemes = append(dialogue.Phonemes, chunk...)
	}
	return Partition(dialogue, cfg)
}

func PackageSummary(packages []domain.ScheduledPackage) string {
	cp := append([]domain.ScheduledPackage(nil), packages...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	sum, _ := domain.Hash(cp)
	return sum
}

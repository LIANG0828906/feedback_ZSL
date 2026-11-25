package sample

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

const (
	DefaultSeed = int64(20261126)
	AlgorithmID = "lipsync-prototype-v1"
	RecipeID    = "recipe-v1"
)

type Truth struct {
	Seed      int64           `json:"seed"`
	Recipe    domain.Recipe   `json:"recipe"`
	Dialogues []TruthDialogue `json:"dialogues"`
}

type TruthDialogue struct {
	DialogueID  string                       `json:"dialogue_id"`
	Packages    []TruthPackage               `json:"packages"`
	Frames      []domain.FrameLip            `json:"frames"`
	Transitions []domain.TransitionInsertion `json:"transitions"`
}

type TruthPackage struct {
	ID           string   `json:"id"`
	StartFrame   int      `json:"start_frame"`
	EndFrame     int      `json:"end_frame"`
	PhonemeIDs   []string `json:"phoneme_ids"`
	PhonemeIndex []int    `json:"phoneme_index"`
}

// Generate builds the deterministic dialogue project and its analytic truth.
// The generator uses its own reference construction for package and frame truth
// rather than invoking the production merge/lip engines.
func Generate(seed int64) (*domain.Project, *Truth, error) {
	rng := rand.New(rand.NewSource(seed))
	recipe := domain.Recipe{
		Version:                    RecipeID,
		FrameRate:                  50,
		Quantize:                   domain.QuantizeNearest,
		AffinityThreshold:          600,
		TransitionTolerance:        1,
		MaxPhonemesPerPackage:      3,
		MaxPackagesPerFrame:        2,
		MaxPackagesPerChannelFrame: 1,
		MaxDeferFrames:             3,
		AlgorithmVersion:           AlgorithmID,
		Channels: []domain.Channel{
			{ID: "speaker-a", Slots: 1},
			{ID: "speaker-b", Slots: 2},
			{ID: "speaker-c", Slots: 1},
		},
	}
	patterns := [][]string{
		{domain.CategorySilence, domain.CategoryConsonant, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence},
		{domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence},
		{domain.CategorySilence, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence},
		{domain.CategoryConsonant, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence},
		{domain.CategorySilence, domain.CategoryVowel, domain.CategoryConsonant, domain.CategorySilence, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence},
		{domain.CategoryConsonant, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence, domain.CategoryVowel, domain.CategoryConsonant, domain.CategoryVowel, domain.CategorySilence},
	}
	channels := []string{"speaker-a", "speaker-b", "speaker-c", "speaker-a", "speaker-b", "speaker-c"}
	project := &domain.Project{Version: 1, Seed: seed, Recipe: recipe}
	truth := &Truth{Seed: seed, Recipe: recipe}
	for di, pattern := range patterns {
		dialogueID := fmt.Sprintf("dlg-%02d", di+1)
		phonemes := make([]domain.Phoneme, 0, len(pattern))
		cursor := int64(0)
		for pi, category := range pattern {
			durationFrames := 5 + rng.Intn(7)
			jitter := int64(rng.Intn(601) - 300)
			duration := int64(durationFrames)*960 + jitter
			if duration < 480 {
				duration = 480
			}
			affinity := 250 + rng.Intn(701)
			if category == domain.CategorySilence || (pi+1 < len(pattern) && pattern[pi+1] == domain.CategorySilence) {
				affinity = 0
			}
			phonemes = append(phonemes, domain.Phoneme{
				ID:             fmt.Sprintf("%s/ph-%02d", dialogueID, pi),
				StartSample:    cursor,
				EndSample:      cursor + duration,
				Category:       category,
				WeightPermille: int64(80 + rng.Intn(161)),
				AffinityToNext: affinity,
			})
			cursor += duration
		}
		dialogue := domain.Dialogue{
			ID:      dialogueID,
			Channel: channels[di],
			PCM: domain.PCMDescriptor{
				Format: "s16le", SampleRate: 48000, BitsPerSample: 16,
				Channels: 1, SampleCount: cursor,
			},
			Phonemes: phonemes,
		}
		project.Dialogues = append(project.Dialogues, dialogue)
		truth.Dialogues = append(truth.Dialogues, buildTruthDialogue(dialogue, recipe))
	}
	if err := domain.ValidateProject(project); err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

func WriteFiles(dir string, seed int64) (*domain.Project, *Truth, error) {
	project, truth, err := Generate(seed)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, &domain.Error{Code: domain.ErrStage, Path: dir, Reason: "create artifact directory", Cause: err}
	}
	if err := writeJSON(filepath.Join(dir, "sample_project.json"), project); err != nil {
		return nil, nil, err
	}
	if err := writeJSON(filepath.Join(dir, "sample_truth.json"), truth); err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

func LoadTruth(path string) (*Truth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &domain.Error{Code: domain.ErrDecode, Path: path, Reason: "read truth", Cause: err}
	}
	var truth Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, &domain.Error{Code: domain.ErrDecode, Path: path, Reason: "decode truth", Cause: err}
	}
	return &truth, nil
}

func buildTruthDialogue(d domain.Dialogue, recipe domain.Recipe) TruthDialogue {
	type qph struct {
		id       string
		start    int
		end      int
		category string
		aff      int
		index    int
	}
	q := make([]qph, 0, len(d.Phonemes))
	for i, ph := range d.Phonemes {
		q = append(q, qph{
			id: ph.ID, start: int((ph.StartSample*int64(recipe.FrameRate) + 24000) / 48000),
			end:      int((ph.EndSample*int64(recipe.FrameRate) + 24000) / 48000),
			category: ph.Category, aff: ph.AffinityToNext, index: i,
		})
	}
	groups := make([][2]int, 0)
	start := 0
	for i := 1; i < len(q); i++ {
		canMerge := i-start < recipe.MaxPhonemesPerPackage &&
			q[i-1].category != domain.CategorySilence &&
			q[i].category != domain.CategorySilence &&
			q[i-1].aff >= recipe.AffinityThreshold
		if !canMerge {
			groups = append(groups, [2]int{start, i})
			start = i
		}
	}
	groups = append(groups, [2]int{start, len(q)})

	td := TruthDialogue{DialogueID: d.ID}
	owner := make(map[string]string, len(q))
	for i, g := range groups {
		pkg := TruthPackage{
			ID:         fmt.Sprintf("%s/pkg/%03d", d.ID, i),
			StartFrame: q[g[0]].start, EndFrame: q[g[1]-1].end,
		}
		for j := g[0]; j < g[1]; j++ {
			pkg.PhonemeIDs = append(pkg.PhonemeIDs, q[j].id)
			pkg.PhonemeIndex = append(pkg.PhonemeIndex, q[j].index)
			owner[q[j].id] = pkg.ID
		}
		td.Packages = append(td.Packages, pkg)
	}
	minFrame := q[0].start
	maxFrame := q[len(q)-1].end
	td.Frames = make([]domain.FrameLip, maxFrame-minFrame)
	for i := range td.Frames {
		td.Frames[i] = domain.FrameLip{Frame: minFrame + i, Lip: domain.LipRest}
	}
	for _, ph := range q {
		for f := ph.start; f < ph.end; f++ {
			td.Frames[f-minFrame] = domain.FrameLip{
				Frame: f, Lip: domain.LipForCategory(ph.category),
				PhonemeID: ph.id, PackageID: owner[ph.id],
			}
		}
	}
	for i := 1; i < len(q); i++ {
		from := domain.LipForCategory(q[i-1].category)
		to := domain.LipForCategory(q[i].category)
		if domain.FamilyDistance(from, to) <= 1 {
			continue
		}
		bridge := domain.BridgeLip(from, to)
		frame := q[i].start
		idx := frame - minFrame
		td.Frames[idx].Lip = bridge
		td.Frames[idx].Transition = true
		td.Frames[idx].TransitionFrom = from
		td.Frames[idx].TransitionTo = to
		td.Transitions = append(td.Transitions, domain.TransitionInsertion{
			DialogueID: d.ID, PackageID: owner[q[i].id], Frame: frame,
			From: from, To: to, Bridge: bridge,
			Basis: "family_distance=2 tolerance=1 bridge=1/1",
		})
	}
	sort.Slice(td.Transitions, func(i, j int) bool { return td.Transitions[i].Frame < td.Transitions[j].Frame })
	return td
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return &domain.Error{Code: domain.ErrStage, Path: path, Reason: "marshal artifact", Cause: err}
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return &domain.Error{Code: domain.ErrStage, Path: path, Reason: "write artifact", Cause: err}
	}
	return nil
}

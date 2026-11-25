package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	CategoryVowel     = "vowel"
	CategoryConsonant = "consonant"
	CategorySilence   = "silence"

	QuantizeFloor   = "floor"
	QuantizeCeil    = "ceil"
	QuantizeNearest = "nearest"

	LipRest   = "rest"
	LipClosed = "closed"
	LipOpen   = "open"
)

var SupportedFrameRates = map[int]struct{}{
	24: {}, 25: {}, 30: {}, 48: {}, 50: {}, 60: {}, 120: {},
}

type Project struct {
	Version   int        `json:"version"`
	Seed      int64      `json:"seed"`
	Recipe    Recipe     `json:"recipe"`
	Dialogues []Dialogue `json:"dialogues"`
}

type Recipe struct {
	Version                    string    `json:"version"`
	FrameRate                  int       `json:"frame_rate"`
	Quantize                   string    `json:"quantize"`
	AffinityThreshold          int       `json:"affinity_threshold"`
	TransitionTolerance        int       `json:"transition_tolerance"`
	MaxPhonemesPerPackage      int       `json:"max_phonemes_per_package"`
	MaxPackagesPerFrame        int       `json:"max_packages_per_frame"`
	MaxPackagesPerChannelFrame int       `json:"max_packages_per_channel_frame"`
	MaxDeferFrames             int       `json:"max_defer_frames"`
	AlgorithmVersion           string    `json:"algorithm_version"`
	Channels                   []Channel `json:"channels"`
}

type Channel struct {
	ID    string `json:"id"`
	Slots int    `json:"slots"`
}

type PCMDescriptor struct {
	Format        string `json:"format"`
	SampleRate    int    `json:"sample_rate"`
	BitsPerSample int    `json:"bits_per_sample"`
	Channels      int    `json:"channels"`
	SampleCount   int64  `json:"sample_count"`
}

type Dialogue struct {
	ID       string        `json:"id"`
	Channel  string        `json:"channel"`
	PCM      PCMDescriptor `json:"pcm"`
	Phonemes []Phoneme     `json:"phonemes"`
}

type Phoneme struct {
	ID             string `json:"id"`
	StartSample    int64  `json:"start_sample"`
	EndSample      int64  `json:"end_sample"`
	Category       string `json:"category"`
	WeightPermille int64  `json:"weight_permille"`
	AffinityToNext int    `json:"affinity_to_next"`
}

type QuantizedPhoneme struct {
	DialogueID     string `json:"dialogue_id"`
	PhonemeID      string `json:"phoneme_id"`
	Index          int    `json:"index"`
	StartSample    int64  `json:"start_sample"`
	EndSample      int64  `json:"end_sample"`
	StartFrame     int    `json:"start_frame"`
	EndFrame       int    `json:"end_frame"`
	Category       string `json:"category"`
	WeightPermille int64  `json:"weight_permille"`
	WeightBasis    int64  `json:"weight_basis_points"`
	AffinityToNext int    `json:"affinity_to_next"`
}

type Truncation struct {
	DialogueID  string `json:"dialogue_id"`
	PhonemeID   string `json:"phoneme_id"`
	Boundary    string `json:"boundary"`
	Sample      int64  `json:"sample"`
	Numerator   int64  `json:"numerator"`
	Denominator int64  `json:"denominator"`
	FloorFrame  int    `json:"floor_frame"`
	Frame       int    `json:"frame"`
	Remainder   int64  `json:"remainder"`
	Direction   string `json:"direction"`
	Adjusted    bool   `json:"adjusted"`
}

type QuantizeResult struct {
	FrameRate   int                `json:"frame_rate"`
	Direction   string             `json:"direction"`
	Phonemes    []QuantizedPhoneme `json:"phonemes"`
	Truncations []Truncation       `json:"truncations"`
}

type LipPackage struct {
	DialogueID   string   `json:"dialogue_id"`
	ID           string   `json:"id"`
	Channel      string   `json:"channel"`
	StartFrame   int      `json:"start_frame"`
	EndFrame     int      `json:"end_frame"`
	PhonemeIDs   []string `json:"phoneme_ids"`
	PhonemeIndex []int    `json:"phoneme_index"`
	Categories   []string `json:"categories"`
}

type TransitionInsertion struct {
	DialogueID string `json:"dialogue_id"`
	PackageID  string `json:"package_id"`
	Frame      int    `json:"frame"`
	From       string `json:"from"`
	To         string `json:"to"`
	Bridge     string `json:"bridge"`
	Basis      string `json:"basis"`
}

type FrameLip struct {
	Frame          int    `json:"frame"`
	Lip            string `json:"lip"`
	PhonemeID      string `json:"phoneme_id"`
	PackageID      string `json:"package_id"`
	Transition     bool   `json:"transition"`
	TransitionFrom string `json:"transition_from,omitempty"`
	TransitionTo   string `json:"transition_to,omitempty"`
}

type LipResult struct {
	DialogueID  string                `json:"dialogue_id"`
	Packages    []LipPackage          `json:"packages"`
	Frames      []FrameLip            `json:"frames"`
	Transitions []TransitionInsertion `json:"transitions"`
}

type DispatchEvent struct {
	DialogueID string `json:"dialogue_id"`
	PackageID  string `json:"package_id"`
	Frame      int    `json:"frame"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
}

type FrameChannelState struct {
	Frame      int                 `json:"frame"`
	Channels   map[string][]string `json:"channels"`
	Used       int                 `json:"used"`
	Dispatched []string            `json:"dispatched"`
}

type DispatchResult struct {
	Accepted      []string            `json:"accepted"`
	Deferred      []string            `json:"deferred"`
	Rejected      []string            `json:"rejected"`
	DispatchFrame map[string]int      `json:"dispatch_frame"`
	Frames        []FrameChannelState `json:"frames"`
	Events        []DispatchEvent     `json:"events"`
}

func (d Dialogue) StaticContentHash() string {
	view := struct {
		ID      string        `json:"id"`
		Channel string        `json:"channel"`
		PCM     PCMDescriptor `json:"pcm"`
	}{d.ID, d.Channel, d.PCM}
	b, _ := json.Marshal(view)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (d Dialogue) FullContentHash(recipe Recipe) string {
	view := struct {
		Dialogue Dialogue `json:"dialogue"`
		Recipe   Recipe   `json:"recipe"`
	}{d, recipe}
	b, _ := json.Marshal(view)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p LipPackage) Summary() string {
	return fmt.Sprintf("%s[%d:%d]{%s}", p.ID, p.StartFrame, p.EndFrame, strings.Join(p.PhonemeIDs, ","))
}

func SortedDialogues(in []Dialogue) []Dialogue {
	out := append([]Dialogue(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func CategoryAllowed(category string) bool {
	switch category {
	case CategoryVowel, CategoryConsonant, CategorySilence:
		return true
	default:
		return false
	}
}

func QuantizeAllowed(direction string) bool {
	switch direction {
	case QuantizeFloor, QuantizeCeil, QuantizeNearest:
		return true
	default:
		return false
	}
}

func LipForCategory(category string) string {
	switch category {
	case CategoryVowel:
		return LipOpen
	case CategoryConsonant:
		return LipClosed
	default:
		return LipRest
	}
}

// LipFamilyRank defines an ordered family lattice. A gap greater than one is a
// non-contiguous family transition and requires bridge frames.
func LipFamilyRank(lip string) int {
	switch lip {
	case LipRest:
		return 0
	case LipClosed:
		return 1
	case LipOpen:
		return 2
	default:
		return -1
	}
}

func FamilyDistance(from, to string) int {
	a, b := LipFamilyRank(from), LipFamilyRank(to)
	if a < 0 || b < 0 {
		return 1 << 30
	}
	if a > b {
		a, b = b, a
	}
	return b - a
}

func BridgeLip(from, to string) string {
	a, b := LipFamilyRank(from), LipFamilyRank(to)
	if a < 0 || b < 0 || a == b {
		return ""
	}
	step := 1
	if a > b {
		step = -1
	}
	rank := a + step
	switch rank {
	case 0:
		return LipRest
	case 1:
		return LipClosed
	case 2:
		return LipOpen
	default:
		return ""
	}
}

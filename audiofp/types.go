package audiofp

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
)

// Recipe fully defines frame analysis and retrieval behavior.
type Recipe struct {
	FrameLen            int     `json:"frame_len"`
	FrameShift          int     `json:"frame_shift"`
	Bands               int     `json:"bands"`
	QuantBits           int     `json:"quant_bits"`
	SimilarityThreshold float64 `json:"similarity_threshold"`
	CandidateLimit      int     `json:"candidate_limit"`
}

// AssetSpec describes one little-endian signed 16-bit mono PCM file.
type AssetSpec struct {
	ID           string `json:"asset_id"`
	File         string `json:"file"`
	SampleRate   int    `json:"sample_rate"`
	TotalSamples int    `json:"total_samples"`
	Channels     int    `json:"channels"`
	SampleFormat string `json:"sample_format"`
	SHA256       string `json:"sha256"`
}

// PCMAsset is the in-memory mono sample stream.
type PCMAsset struct {
	ID         string  `json:"asset_id"`
	SampleRate int     `json:"sample_rate"`
	Samples    []int16 `json:"-"`
}

// ContentHash hashes the exact little-endian PCM sample bytes.
func (a PCMAsset) ContentHash() string {
	buf := make([]byte, 2*len(a.Samples))
	for i, s := range a.Samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return digest(buf)
}

func (a PCMAsset) Clone() PCMAsset {
	out := a
	out.Samples = append([]int16(nil), a.Samples...)
	return out
}

// EditOp is one deterministic logical edit.
type EditOp struct {
	Seq                     int      `json:"seq"`
	LogicalTime             int64    `json:"logical_time"`
	Kind                    string   `json:"kind"`
	AssetID                 string   `json:"asset_id,omitempty"`
	StartSample             int      `json:"start_sample,omitempty"`
	EndSample               int      `json:"end_sample,omitempty"`
	ReplacementFile         string   `json:"replacement_file,omitempty"`
	ReplacementSHA256       string   `json:"replacement_sha256,omitempty"`
	ReplacementTotalSamples int      `json:"replacement_total_samples,omitempty"`
	Param                   string   `json:"param,omitempty"`
	Value                   *float64 `json:"value,omitempty"`
}

// FrameTruth is the generator's analytically-derived expected frame.
type FrameTruth struct {
	FrameNo     int     `json:"frame_no"`
	StartSample int     `json:"start_sample"`
	Bands       int     `json:"bands"`
	BitWidth    int     `json:"bit_width"`
	Levels      []uint8 `json:"levels"`
	PackedHex   string  `json:"packed_hex"`
	Zero        bool    `json:"zero"`
	Summary     string  `json:"summary"`
}

type AssetTruth struct {
	AssetID      string       `json:"asset_id"`
	ContentHash  string       `json:"content_hash"`
	TotalSamples int          `json:"total_samples"`
	Frames       []FrameTruth `json:"frames"`
}

type MarkerTruth struct {
	MarkerID    string `json:"marker_id"`
	AssetID     string `json:"asset_id"`
	Sample      int    `json:"sample"`
	FrameNo     int    `json:"frame_no"`
	Description string `json:"description"`
}

type RankedTruth struct {
	Rank        int     `json:"rank"`
	AssetID     string  `json:"asset_id"`
	StartSample int     `json:"start_sample"`
	EndSample   int     `json:"end_sample"`
	Similarity  float64 `json:"similarity"`
}

type QueryTruth struct {
	QueryID        string        `json:"query_id"`
	AssetID        string        `json:"asset_id"`
	File           string        `json:"file"`
	SampleRate     int           `json:"sample_rate"`
	TotalSamples   int           `json:"total_samples"`
	SHA256         string        `json:"sha256"`
	StartSample    int           `json:"start_sample"`
	FrameCount     int           `json:"frame_count"`
	Threshold      float64       `json:"similarity_threshold"`
	CandidateLimit int           `json:"candidate_limit"`
	Expected       []RankedTruth `json:"expected"`
}

type Truth struct {
	FinalRecipe Recipe        `json:"final_recipe"`
	Assets      []AssetTruth  `json:"assets"`
	Markers     []MarkerTruth `json:"markers"`
	Queries     []QueryTruth  `json:"queries"`
}

type Manifest struct {
	SchemaVersion int         `json:"schema_version"`
	Seed          int64       `json:"seed"`
	BatchID       string      `json:"batch_id"`
	Assets        []AssetSpec `json:"assets"`
	Recipe        Recipe      `json:"recipe"`
	Edits         []EditOp    `json:"edits"`
	Truth         Truth       `json:"truth"`
}

type EditEvent struct {
	Seq               int    `json:"seq"`
	LogicalTime       int64  `json:"logical_time"`
	Kind              string `json:"kind"`
	AssetID           string `json:"asset_id,omitempty"`
	Reason            string `json:"reason"`
	FramesInvalidated bool   `json:"frames_invalidated"`
}

// FrameFingerprint is one quantized analysis frame.
type FrameFingerprint struct {
	AssetID     string  `json:"asset_id"`
	FrameNo     int     `json:"frame_no"`
	StartSample int     `json:"start_sample"`
	Bands       int     `json:"bands"`
	BitWidth    int     `json:"bit_width"`
	Levels      []uint8 `json:"levels"`
	PackedHex   string  `json:"packed_hex"`
	Zero        bool    `json:"zero"`
	Summary     string  `json:"summary"`
}

func (f FrameFingerprint) ToTruth() FrameTruth {
	return FrameTruth{
		FrameNo:     f.FrameNo,
		StartSample: f.StartSample,
		Bands:       f.Bands,
		BitWidth:    f.BitWidth,
		Levels:      append([]uint8(nil), f.Levels...),
		PackedHex:   f.PackedHex,
		Zero:        f.Zero,
		Summary:     f.Summary,
	}
}

func TruthToFingerprint(assetID string, t FrameTruth) FrameFingerprint {
	return FrameFingerprint{
		AssetID:     assetID,
		FrameNo:     t.FrameNo,
		StartSample: t.StartSample,
		Bands:       t.Bands,
		BitWidth:    t.BitWidth,
		Levels:      append([]uint8(nil), t.Levels...),
		PackedHex:   t.PackedHex,
		Zero:        t.Zero,
		Summary:     t.Summary,
	}
}

type IndexEntry struct {
	AssetID     string `json:"asset_id"`
	FrameNo     int    `json:"frame_no"`
	StartSample int    `json:"start_sample"`
	CacheKey    string `json:"cache_key"`
	EntryDigest string `json:"entry_digest"`
}

type AssetAnalysis struct {
	AssetID      string             `json:"asset_id"`
	ContentHash  string             `json:"content_hash"`
	RecipeHash   string             `json:"recipe_hash"`
	TotalSamples int                `json:"total_samples"`
	Frames       []FrameFingerprint `json:"frames"`
	Index        []IndexEntry       `json:"index"`
}

func (a AssetAnalysis) Clone() AssetAnalysis {
	out := a
	out.Frames = make([]FrameFingerprint, len(a.Frames))
	for i := range a.Frames {
		out.Frames[i] = a.Frames[i]
		out.Frames[i].Levels = append([]uint8(nil), a.Frames[i].Levels...)
	}
	out.Index = append([]IndexEntry(nil), a.Index...)
	return out
}

func SortAnalyses(items []AssetAnalysis) {
	sort.Slice(items, func(i, j int) bool { return items[i].AssetID < items[j].AssetID })
	for i := range items {
		sort.Slice(items[i].Frames, func(a, b int) bool {
			return items[i].Frames[a].FrameNo < items[i].Frames[b].FrameNo
		})
		sort.Slice(items[i].Index, func(a, b int) bool {
			return items[i].Index[a].FrameNo < items[i].Index[b].FrameNo
		})
	}
}

type Match struct {
	Rank        int     `json:"rank"`
	AssetID     string  `json:"asset_id"`
	StartSample int     `json:"start_sample"`
	EndSample   int     `json:"end_sample"`
	Similarity  float64 `json:"similarity"`
	FrameStart  int     `json:"frame_start"`
	FrameCount  int     `json:"frame_count"`
}

type SearchResult struct {
	QueryID        string  `json:"query_id"`
	Threshold      float64 `json:"similarity_threshold"`
	CandidateLimit int     `json:"candidate_limit"`
	Matches        []Match `json:"matches"`
}

func maxLevel(bits int) int {
	if bits <= 0 {
		return 0
	}
	if bits >= 16 {
		return 65535
	}
	return (1 << bits) - 1
}

// PackLevels packs quantized band levels MSB-first into a hex string.
func PackLevels(levels []uint8, bits int) string {
	if bits <= 0 || len(levels) == 0 {
		return ""
	}
	totalBits := len(levels) * bits
	out := make([]byte, (totalBits+7)/8)
	bitPos := 0
	for _, level := range levels {
		v := int(level)
		for b := bits - 1; b >= 0; b-- {
			if (v>>b)&1 == 1 {
				out[bitPos/8] |= 1 << (7 - uint(bitPos%8))
			}
			bitPos++
		}
	}
	return hex.EncodeToString(out)
}

// UnpackLevels reverses PackLevels and is also used by the independent verifier.
func UnpackLevels(packed string, bands, bits int) ([]uint8, error) {
	if bands <= 0 || bits <= 0 {
		return nil, fmt.Errorf("invalid packed fingerprint dimensions")
	}
	data, err := hex.DecodeString(packed)
	if err != nil {
		return nil, err
	}
	need := (bands*bits + 7) / 8
	if len(data) != need {
		return nil, fmt.Errorf("packed fingerprint has %d bytes, want %d", len(data), need)
	}
	levels := make([]uint8, bands)
	bitPos := 0
	for i := 0; i < bands; i++ {
		v := 0
		for b := 0; b < bits; b++ {
			v <<= 1
			if data[bitPos/8]&(1<<(7-uint(bitPos%8))) != 0 {
				v |= 1
			}
			bitPos++
		}
		levels[i] = uint8(v)
	}
	return levels, nil
}

// FrameSummary is deterministic and independent of extraction implementation.
func FrameSummary(startSample int, levels []uint8, bits int, zero bool) string {
	packed := PackLevels(levels, bits)
	var start [4]byte
	binary.LittleEndian.PutUint32(start[:], uint32(startSample))
	zeroByte := []byte{0}
	if zero {
		zeroByte[0] = 1
	}
	return digest(start[:], []byte(packed), []byte{byte(bits), byte(len(levels))}, zeroByte)
}

func roundTiny(v float64) float64 {
	if v < 1e-12 && v > -1e-12 {
		return 0
	}
	if v > 1-1e-12 {
		return 1
	}
	return math.Round(v*1e12) / 1e12
}

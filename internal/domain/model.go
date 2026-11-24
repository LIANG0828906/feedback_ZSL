package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaVersion    = 1
	AlgorithmVersion = "mfx-transcode/1.0.0"
	PackageVersion   = "MFXPKG01"
)

type Recipe struct {
	TargetWidth     int      `json:"targetWidth"`
	TargetHeight    int      `json:"targetHeight"`
	ImageChannels   int      `json:"imageChannels"`
	ImageBitDepth   int      `json:"imageBitDepth"`
	ChromaSampling  string   `json:"chromaSampling"`
	AudioSampleRate int      `json:"audioSampleRate"`
	AudioBitDepth   int      `json:"audioBitDepth"`
	MetadataPolicy  string   `json:"metadataPolicy"`
	MetadataFields  []string `json:"metadataFields,omitempty"`
}

func (r Recipe) Clone() Recipe {
	out := r
	out.MetadataFields = append([]string(nil), r.MetadataFields...)
	sort.Strings(out.MetadataFields)
	return out
}

type Budget struct {
	MaxWorkers          int   `json:"maxWorkers"`
	MemoryBytes         int64 `json:"memoryBytes"`
	PerVariantMillis    int   `json:"perVariantMillis"`
	MaxPixelsPerWorker  int64 `json:"maxPixelsPerWorker"`
	MaxSamplesPerWorker int64 `json:"maxSamplesPerWorker"`
	FineMaxPixels       int64 `json:"fineMaxPixels"`
	FineMaxSamples      int64 `json:"fineMaxSamples"`
	FineMaxVariants     int   `json:"fineMaxVariants"`
	AdaptiveBlockPixels int64 `json:"adaptiveBlockPixels"`
	MaxDownsample       int   `json:"maxDownsample"`
}

type Consistency struct {
	MaxPixelValueDelta   int  `json:"maxPixelValueDelta"`
	MaxSampleValueDelta  int  `json:"maxSampleValueDelta"`
	RequireByteIdentical bool `json:"requireByteIdentical"`
}

type Source struct {
	ID                string            `json:"id"`
	Kind              string            `json:"kind"`
	Path              string            `json:"path"`
	SHA256            string            `json:"sha256"`
	Width             int               `json:"width,omitempty"`
	Height            int               `json:"height,omitempty"`
	Channels          int               `json:"channels,omitempty"`
	SampleRate        int               `json:"sampleRate,omitempty"`
	AudioChannels     int               `json:"audioChannels,omitempty"`
	AudioBitDepth     int               `json:"audioBitDepth,omitempty"`
	SamplesPerChannel int64             `json:"samplesPerChannel,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type VariantSeed struct {
	ID          string `json:"id"`
	ImageSource string `json:"imageSource"`
	AudioSource string `json:"audioSource"`
}

type Operation struct {
	Seq        int64           `json:"seq"`
	Time       int64           `json:"time"`
	Type       string          `json:"type"`
	Variant    string          `json:"variant,omitempty"`
	Media      string          `json:"media,omitempty"`
	Source     string          `json:"source,omitempty"`
	Field      string          `json:"field,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	NewVariant *VariantSeed    `json:"newVariant,omitempty"`
}

type TruthVariant struct {
	ID             string   `json:"id"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	Channels       int      `json:"channels"`
	PixelCount     int64    `json:"pixelCount"`
	AudioSamples   int64    `json:"audioSamples"`
	MetadataFields []string `json:"metadataFields"`
	Fingerprint    string   `json:"fingerprint"`
	SizeBytes      int64    `json:"sizeBytes"`
}

type TruthManifest struct {
	Variants []TruthVariant `json:"variants"`
}

func (t TruthManifest) ByID() map[string]TruthVariant {
	out := make(map[string]TruthVariant, len(t.Variants))
	for _, v := range t.Variants {
		out[v.ID] = v
	}
	return out
}

type Project struct {
	SchemaVersion int           `json:"schemaVersion"`
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Sources       []Source      `json:"sources"`
	BaseRecipe    Recipe        `json:"baseRecipe"`
	Variants      []VariantSeed `json:"variants"`
	Operations    []Operation   `json:"operations"`
	Truth         TruthManifest `json:"truth"`
	Budget        Budget        `json:"budget"`
	Consistency   Consistency   `json:"consistency"`
}

type ResolvedVariant struct {
	ID            string `json:"id"`
	ImageSourceID string `json:"imageSource"`
	AudioSourceID string `json:"audioSource"`
	Recipe        Recipe `json:"recipe"`
	RecipeVersion string `json:"recipeVersion"`
	InputHash     string `json:"inputHash"`
}

type ResolvedProject struct {
	Project  *Project          `json:"-"`
	Variants []ResolvedVariant `json:"variants"`
	Sources  map[string]Source `json:"sources"`
}

type PreviewInfo struct {
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Downsample    int    `json:"downsample"`
	SampledPixels int64  `json:"sampledPixels"`
	SHA256        string `json:"sha256"`
}

func (r *ResolvedProject) Variant(id string) (ResolvedVariant, bool) {
	for _, v := range r.Variants {
		if v.ID == id {
			return v, true
		}
	}
	return ResolvedVariant{}, false
}

func MetadataFields(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func NormalizeRecipeField(field string) string {
	switch field {
	case "targetWidth", "targetHeight", "imageChannels", "imageBitDepth", "chromaSampling",
		"audioSampleRate", "audioBitDepth", "metadataPolicy", "metadataFields":
		return field
	default:
		return ""
	}
}

func ApplyRecipeField(r *Recipe, field string, raw json.RawMessage) error {
	switch field {
	case "targetWidth":
		return json.Unmarshal(raw, &r.TargetWidth)
	case "targetHeight":
		return json.Unmarshal(raw, &r.TargetHeight)
	case "imageChannels":
		return json.Unmarshal(raw, &r.ImageChannels)
	case "imageBitDepth":
		return json.Unmarshal(raw, &r.ImageBitDepth)
	case "chromaSampling":
		return json.Unmarshal(raw, &r.ChromaSampling)
	case "audioSampleRate":
		return json.Unmarshal(raw, &r.AudioSampleRate)
	case "audioBitDepth":
		return json.Unmarshal(raw, &r.AudioBitDepth)
	case "metadataPolicy":
		return json.Unmarshal(raw, &r.MetadataPolicy)
	case "metadataFields":
		return json.Unmarshal(raw, &r.MetadataFields)
	default:
		return fmt.Errorf("unknown recipe field %q", field)
	}
}

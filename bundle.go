package textureconv

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SourceRecord describes one deterministic source image in a sample bundle.
type SourceRecord struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	BitDepth BitDepth `json:"bitDepth"`
	Layout   Layout   `json:"layout"`
	SHA256   string   `json:"sha256"`
}

type SampleManifest struct {
	SchemaVersion    int            `json:"schemaVersion"`
	Seed             int64          `json:"seed"`
	PlanPath         string         `json:"planPath"`
	TruthPath        string         `json:"truthPath"`
	TruthFingerprint string         `json:"truthFingerprint"`
	Images           []SourceRecord `json:"images"`
}

type Truth struct {
	SchemaVersion   int          `json:"schemaVersion"`
	PlanFingerprint string       `json:"planFingerprint"`
	Images          []ImageTruth `json:"images"`
	Fingerprint     string       `json:"fingerprint"`
}

type ImageTruth struct {
	ImageID      string        `json:"imageId"`
	SourceSHA256 string        `json:"sourceSha256"`
	Width        int           `json:"width"`
	Height       int           `json:"height"`
	Samples      []TruthSample `json:"samples"`
}

type TruthSample struct {
	Entry     int      `json:"entry"`
	X         int      `json:"x"`
	Y         int      `json:"y"`
	BitDepth  BitDepth `json:"bitDepth"`
	Channels  []uint16 `json:"channels"`
	Tolerance uint16   `json:"tolerance"`
}

// SampleBundle is a fully parsed and cross-checked local sample.
type SampleBundle struct {
	Dir      string
	Manifest SampleManifest
	Plan     *Plan
	Truth    *Truth
	Images   map[string]*Image
	Hashes   map[string]string
}

func LoadSample(dir string) (*SampleBundle, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, ioErr("readManifest", err)
	}
	var m SampleManifest
	if err := decodeStrict(data, &m); err != nil {
		return nil, schemaErr(0, "manifest", "invalid JSON: %v", err)
	}
	if m.SchemaVersion != SchemaVersion {
		return nil, schemaErr(0, "schemaVersion", "got %d, want %d", m.SchemaVersion, SchemaVersion)
	}
	if len(m.Images) == 0 {
		return nil, schemaErr(0, "images", "manifest contains no images")
	}
	if m.PlanPath == "" {
		return nil, schemaErr(0, "planPath", "required field is missing")
	}
	if m.TruthPath == "" {
		return nil, schemaErr(0, "truthPath", "required field is missing")
	}
	if m.TruthFingerprint == "" {
		return nil, schemaErr(0, "truthFingerprint", "required field is missing")
	}
	planPath := m.PlanPath
	if !filepath.IsAbs(planPath) {
		planPath = filepath.Join(dir, planPath)
	}
	plan, err := LoadPlan(planPath)
	if err != nil {
		return nil, err
	}
	truthPath := m.TruthPath
	if !filepath.IsAbs(truthPath) {
		truthPath = filepath.Join(dir, truthPath)
	}
	truthData, err := os.ReadFile(truthPath)
	if err != nil {
		return nil, ioErr("readTruth", err)
	}
	var truth Truth
	if err := decodeStrict(truthData, &truth); err != nil {
		return nil, schemaErr(0, "truth", "invalid JSON: %v", err)
	}
	if truth.Fingerprint == "" {
		return nil, schemaErr(0, "fingerprint", "truth fingerprint is missing")
	}
	if truth.SchemaVersion != SchemaVersion {
		return nil, schemaErr(0, "truth.schemaVersion", "got %d, want %d", truth.SchemaVersion, SchemaVersion)
	}
	if truth.PlanFingerprint == "" {
		return nil, schemaErr(0, "truth.planFingerprint", "required field is missing")
	}
	if got := truthFingerprint(&truth); got != truth.Fingerprint {
		return nil, paramErr(0, "fingerprint", "truth fingerprint mismatch: file=%s calculated=%s", truth.Fingerprint, got)
	}
	if m.TruthFingerprint != truth.Fingerprint {
		return nil, paramErr(0, "truthFingerprint", "manifest and truth fingerprints differ")
	}
	if truth.PlanFingerprint != plan.Fingerprint() {
		return nil, paramErr(0, "planFingerprint", "truth was generated for a different plan")
	}
	truthIDs := map[string]bool{}
	for _, it := range truth.Images {
		if truthIDs[it.ImageID] {
			return nil, schemaErr(0, "truth.imageId", "duplicate image truth %q", it.ImageID)
		}
		truthIDs[it.ImageID] = true
		if it.ImageID == "" {
			return nil, schemaErr(0, "truth.imageId", "required field is missing")
		}
		if it.SourceSHA256 == "" {
			return nil, schemaErr(0, "truth.sourceSha256", "required field is missing")
		}
		if it.Width <= 0 {
			return nil, schemaErr(0, "truth.width", "required field is missing or invalid")
		}
		if it.Height <= 0 {
			return nil, schemaErr(0, "truth.height", "required field is missing or invalid")
		}
	}
	b := &SampleBundle{
		Dir: dir, Manifest: m, Plan: plan, Truth: &truth,
		Images: map[string]*Image{}, Hashes: map[string]string{},
	}
	for _, rec := range m.Images {
		if _, exists := b.Images[rec.ID]; exists {
			return nil, schemaErr(0, "images.id", "duplicate image id %q", rec.ID)
		}
		p := rec.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		im, hash, err := LoadImageFile(p)
		if err != nil {
			return nil, err
		}
		if err := validateSourceRecord(rec, im, hash); err != nil {
			return nil, err
		}
		if err := plan.ValidateForImage(im); err != nil {
			return nil, err
		}
		if _, ok := truthImage(&truth, rec.ID); !ok {
			return nil, schemaErr(0, "truth.images", "missing truth for image %q", rec.ID)
		}
		b.Images[rec.ID] = im
		b.Hashes[rec.ID] = hash
	}
	for _, it := range truth.Images {
		if _, ok := b.Images[it.ImageID]; !ok {
			return nil, schemaErr(0, "truth.imageId", "unknown image %q", it.ImageID)
		}
		rec, _ := b.Source(it.ImageID)
		if it.SourceSHA256 != b.Hashes[it.ImageID] {
			return nil, paramErr(0, "truth.sourceSha256", "image %q source hash mismatch", it.ImageID)
		}
		if it.Width != rec.Width || it.Height != rec.Height {
			return nil, paramErr(0, "truth.geometry", "image %q geometry mismatch", it.ImageID)
		}
		for _, s := range it.Samples {
			if s.Entry <= 0 {
				return nil, schemaErr(0, "truth.sample.entry", "required field is missing")
			}
			if len(s.Channels) == 0 {
				return nil, schemaErr(s.Entry, "truth.sample.channels", "required field is missing")
			}
			if _, ok := plan.Entry(s.Entry); !ok {
				return nil, schemaErr(s.Entry, "entry", "truth sample references an unknown entry")
			}
			if s.X < 0 || s.Y < 0 || s.X >= rec.Width || s.Y >= rec.Height {
				return nil, paramErr(s.Entry, "sample", "sample (%d,%d) is outside image", s.X, s.Y)
			}
			if s.BitDepth == 0 {
				return nil, schemaErr(s.Entry, "truth.sample.bitDepth", "required field is missing")
			}
			if !s.BitDepth.Valid() {
				return nil, paramErr(s.Entry, "bitDepth", "truth sample has invalid depth %d", s.BitDepth)
			}
		}
	}
	return b, nil
}

func truthImage(t *Truth, id string) (*ImageTruth, bool) {
	for i := range t.Images {
		if t.Images[i].ImageID == id {
			return &t.Images[i], true
		}
	}
	return nil, false
}

func (b *SampleBundle) Source(id string) (SourceRecord, bool) {
	for _, r := range b.Manifest.Images {
		if r.ID == id {
			return r, true
		}
	}
	return SourceRecord{}, false
}

func (b *SampleBundle) ImageTruth(id string) (*ImageTruth, bool) {
	for i := range b.Truth.Images {
		if b.Truth.Images[i].ImageID == id {
			return &b.Truth.Images[i], true
		}
	}
	return nil, false
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

// truthFingerprint calculates the canonical truth hash with Fingerprint
// replaced by the empty string.
func truthFingerprint(t *Truth) string {
	cp := *t
	cp.Fingerprint = ""
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

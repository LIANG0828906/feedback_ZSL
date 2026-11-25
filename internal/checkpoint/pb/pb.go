// Package pb holds the on-disk checkpoint wire types so that the engine
// and the checkpoint store do not form an import cycle.
package pb

import "strokeforge/internal/model"

// Record is the on-disk serialization of one committed segment.
type Record struct {
	Algorithm    string         `json:"algorithm"`
	ContentHash  string         `json:"content_hash"`
	ContourHash  string         `json:"contour_hash"`
	ParamVersion string         `json:"param_version"`
	GlyphID      string         `json:"glyph_id"`
	SegmentID    int            `json:"segment_id"`
	ContourID    string         `json:"contour_id"`
	CacheKey     string         `json:"cache_key"`
	MaskWidth    int            `json:"mask_width"`
	MaskHeight   int            `json:"mask_height"`
	MaskPix      []byte         `json:"mask_pix"`
	Densities    []int          `json:"densities"`
	Boundary     []model.FPoint `json:"boundary"`
	RecordHash   string         `json:"record_hash"`
}

// Manifest records document-level identity used to validate the whole store.
type Manifest struct {
	Algorithm    string            `json:"algorithm"`
	GlyphHashes  map[string]string `json:"glyph_hashes"`
	ParamVersion string            `json:"param_version"`
	Seed         int64             `json:"seed"`
}

// SegmentSnapshot is the engine-independent payload persisted per segment.
type SegmentSnapshot struct {
	Algorithm    string
	ContentHash  string
	ContourHash  string
	ParamVersion string
	GlyphID      string
	SegmentID    int
	ContourID    string
	CacheKey     string
	MaskWidth    int
	MaskHeight   int
	MaskPix      []byte
	Densities    []int
	Boundary     []model.FPoint
}

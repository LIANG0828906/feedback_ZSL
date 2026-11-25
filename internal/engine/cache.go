package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"strokeforge/internal/checkpoint/pb"
	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// SegmentRecord is the cached render product of one stroke segment.
type SegmentRecord struct {
	GlyphID       string
	SegmentID     int
	ContourID     string
	State         string
	CacheKey      string
	Band          StrokeBand
	Mask          *raster.Mask
	Densities     []int
	Boundary      []model.FPoint
	ParamVersion  string
	ContentHash   string
	ContourHash   string
	Algorithm     string
	Attempts      int
	FailureReason string
}

// ContentHash returns a stable SHA-256 over the glyph contours (control-point
// geometry only), so control-point edits change the hash deterministically.
func ContentHash(g *model.Glyph) string {
	type slim struct {
		ID       string          `json:"id"`
		Contours []model.Contour `json:"contours"`
	}
	raw, _ := json.Marshal(slim{ID: g.ID, Contours: g.Contours})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ContourHash fingerprints a single contour's control-point geometry.
func ContourHash(c *model.Contour) string {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ParamsHash hashes the stroke parameter snapshot.
func ParamsHash(p model.StrokeParams) string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SegmentCacheKey composes the four required inputs: contour content hash,
// segment number, parameter values and upstream cache keys.
func SegmentCacheKey(contourHash, paramVersion string, seg model.StrokeSegment, upstreamKeys []string) string {
	if upstreamKeys == nil {
		upstreamKeys = []string{}
	}
	if seg.Upstream == nil {
		seg.Upstream = []int{}
	}
	type wire struct {
		ContourHash  string              `json:"contour_hash"`
		ParamVersion string              `json:"param_version"`
		Segment      model.StrokeSegment `json:"segment"`
		Upstream     []string            `json:"upstream"`
	}
	raw, _ := json.Marshal(wire{
		ContourHash:  contourHash,
		ParamVersion: paramVersion,
		Segment:      seg,
		Upstream:     upstreamKeys,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MaskHash fingerprints a segment mask component for checkpoint validation.
func MaskHash(m *raster.Mask) string {
	if m == nil {
		return ""
	}
	sum := sha256.Sum256(m.Pix)
	return hex.EncodeToString(sum[:])
}

// Snapshot converts a committed record into the checkpoint wire payload.
func (r *SegmentRecord) Snapshot() pb.SegmentSnapshot {
	return pb.SegmentSnapshot{
		Algorithm:    r.Algorithm,
		ContentHash:  r.ContentHash,
		ContourHash:  r.ContourHash,
		ParamVersion: r.ParamVersion,
		GlyphID:      r.GlyphID,
		SegmentID:    r.SegmentID,
		ContourID:    r.ContourID,
		CacheKey:     r.CacheKey,
		MaskWidth:    r.Mask.Width,
		MaskHeight:   r.Mask.Height,
		MaskPix:      r.Mask.Pix,
		Densities:    r.Densities,
		Boundary:     r.Boundary,
	}
}

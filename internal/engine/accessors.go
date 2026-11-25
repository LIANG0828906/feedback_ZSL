package engine

import (
	"bytes"
	"image"
	"image/png"

	"strokeforge/internal/checkpoint/pb"
	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

func modelCodedError(code, msg string) error {
	return model.NewError(code, msg)
}

// GlyphContentHash exposes the cached content hash of one glyph runtime.
func (wb *Workbench) GlyphContentHash(glyphID string) string {
	if rt, ok := wb.runtimes[glyphID]; ok {
		return rt.ContentHash
	}
	return ""
}

// Frames returns a copy of every glyph pixel frame.
func (wb *Workbench) Frames() map[string]raster.Frame {
	out := map[string]raster.Frame{}
	for id, rt := range wb.runtimes {
		out[id] = rt.Frames
	}
	return out
}

// Record returns one committed segment record.
func (wb *Workbench) Record(glyphID string, segID int) *SegmentRecord {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	if rt, ok := wb.runtimes[glyphID]; ok {
		return rt.Records[segID]
	}
	return nil
}

// Runtime exposes one glyph runtime (read-only uses).
func (wb *Workbench) Runtime(glyphID string) *GlyphRuntime {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	return wb.runtimes[glyphID]
}

// Runtimes returns the full runtime map.
func (wb *Workbench) Runtimes() map[string]*GlyphRuntime { return wb.runtimes }

// CanonicalSequenceExport exposes the deterministic committed sequence.
func (wb *Workbench) CanonicalSequenceExport() []CommitEntry { return wb.canonicalSequence() }

// RestoreCheckpoint rebuilds in-memory records from persisted ones, verifying
// that the recomputed cache key matches the stored key.
func (wb *Workbench) RestoreCheckpoint(records []pb.Record) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	for _, r := range records {
		rt, ok := wb.runtimes[r.GlyphID]
		if !ok {
			return modelCodedError("E_SCHEMA", "checkpoint references unknown glyph: "+r.GlyphID)
		}
		seg := findSeg(rt.Glyph, r.SegmentID)
		var upstreamKeys []string
		for _, up := range seg.Upstream {
			upRec := rt.Records[up]
			if upRec == nil {
				return modelCodedError("E_STAGE", "upstream checkpoint missing")
			}
			upstreamKeys = append(upstreamKeys, upRec.CacheKey)
		}
		expectedKey := SegmentCacheKey(rt.ContourHash[seg.ContourID], wb.paramVersion, seg, upstreamKeys)
		if expectedKey != r.CacheKey {
			return modelCodedError("E_STAGE", "restored cache key mismatch")
		}
		mask := &raster.Mask{Width: r.MaskWidth, Height: r.MaskHeight, Pix: r.MaskPix}
		rec := &SegmentRecord{
			GlyphID:      r.GlyphID,
			SegmentID:    r.SegmentID,
			ContourID:    r.ContourID,
			State:        StateCommitted,
			CacheKey:     r.CacheKey,
			Mask:         mask,
			Densities:    r.Densities,
			Boundary:     r.Boundary,
			ParamVersion: r.ParamVersion,
			ContentHash:  r.ContentHash,
			ContourHash:  r.ContourHash,
			Algorithm:    r.Algorithm,
		}
		rt.Records[r.SegmentID] = rec
		if err := rt.Mask.MergeMax(mask); err != nil {
			return err
		}
	}
	return nil
}

// MaskPNG encodes one glyph mask as an 8-bit grayscale PNG.
func (wb *Workbench) MaskPNG(glyphID string) ([]byte, error) {
	wb.mu.Lock()
	rt, ok := wb.runtimes[glyphID]
	wb.mu.Unlock()
	if !ok {
		return nil, modelCodedError("E_SCHEMA", "unknown glyph: "+glyphID)
	}
	img := image.NewGray(image.Rect(0, 0, rt.Mask.Width, rt.Mask.Height))
	for i, v := range rt.Mask.Pix {
		img.Pix[i] = v
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

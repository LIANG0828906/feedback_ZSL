package vpf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		// All domain value types passed here are JSON-safe.
		panic(fmt.Sprintf("vpf: canonical hash encoding failed: %v", err))
	}
	return SHA256Hex(data)
}

func HashStrings(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ShortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// LayerContentHash hashes only the immutable procedural content descriptor.
// Opacity/version are intentionally separate inputs to the cache key.
func LayerContentHash(layer Layer) string {
	rippleIDs := make([]string, 0, len(layer.Ripples))
	for _, source := range layer.Ripples {
		rippleIDs = append(rippleIDs, source.ID)
	}
	return HashJSON(struct {
		ID             string    `json:"id"`
		Kind           LayerKind `json:"kind"`
		Rect           Rect      `json:"rect"`
		Color          RGBA      `json:"color"`
		ContentSummary string    `json:"content_summary"`
		RippleIDs      []string  `json:"ripple_ids"`
	}{
		ID:             layer.ID,
		Kind:           layer.Kind,
		Rect:           layer.Rect,
		Color:          layer.Color,
		ContentSummary: layer.ContentSummary,
		RippleIDs:      rippleIDs,
	})
}

func RippleSourceHash(source RippleSource) string {
	return HashJSON(struct {
		ID          string `json:"id"`
		Center      Point  `json:"center"`
		Radius      int    `json:"radius"`
		DecayBP     int    `json:"decay_bp"`
		AmplitudeBP int    `json:"amplitude_bp"`
	}{source.ID, source.Center, source.Radius, source.DecayBP, source.AmplitudeBP})
}

// LayerCacheKey chains an upstream key with the exact required inputs.
func LayerCacheKey(upstream string, layer Layer) string {
	return HashStrings(
		"layer-v1",
		upstream,
		layer.ID,
		layer.ContentHash,
		fmt.Sprintf("param=%d", layer.ParamVersion),
		fmt.Sprintf("opacity=%d", layer.OpacityBP),
		fmt.Sprintf("blend=%s", layer.Blend),
	)
}

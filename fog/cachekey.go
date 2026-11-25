package fog

// ComputeCacheKey combines the content hash, ray identity, parameter version
// and ordered upstream keys. Keeping this explicit prevents a tile from
// accidentally reusing a result across a camera, recipe or media edit.
func ComputeCacheKey(contentHash, rayID, paramVersion string, upstream ...string) string {
	parts := make([]string, 0, 3+len(upstream))
	parts = append(parts, contentHash, rayID, paramVersion)
	parts = append(parts, upstream...)
	return hashStrings(parts...)
}

package fog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

type RenderPixel struct {
	X        int        `json:"x"`
	Y        int        `json:"y"`
	Radiance [3]float64 `json:"radiance"`
	Color    [3]uint8   `json:"color"`
}

type SegmentDigest struct {
	RayID       string   `json:"ray_id"`
	RayCacheKey string   `json:"ray_cache_key"`
	Segments    []string `json:"segments"`
}

type TileResult struct {
	Tile              TileCoord          `json:"tile"`
	Pixels            []RenderPixel      `json:"pixels"`
	RayResults        []RayResult        `json:"ray_results,omitempty"`
	RaySegmentDigest  string             `json:"ray_segment_digest"`
	SegmentDigests    []SegmentDigest    `json:"segment_digests,omitempty"`
	Transmittance     float64            `json:"transmittance"`
	Scattering        float64            `json:"scattering"`
	AverageScattering float64            `json:"average_scattering"`
	CacheKey          string             `json:"cache_key"`
	ContentHash       string             `json:"content_hash"`
	RecipeVersion     int64              `json:"recipe_version"`
	CameraVersion     int64              `json:"camera_version"`
	AlgoVersion       string             `json:"algo_version"`
	Options           IntegrationOptions `json:"options"`
	ShadowSamples     int                `json:"shadow_samples"`
	MaxRayBufferUsed  int                `json:"max_ray_buffer_used"`
	CommitOrder       int                `json:"commit_order"`
	Status            string             `json:"status"`
}

type TileRenderConfig struct {
	Options        IntegrationOptions
	RayBufferLimit int
	FailInjection  func(tile TileCoord) error
}

func RenderTile(scene *Scene, tile TileCoord, config TileRenderConfig) (*TileResult, error) {
	if config.FailInjection != nil {
		if err := config.FailInjection(tile); err != nil {
			return nil, err
		}
	}
	rays := RaysForTile(*scene, tile)
	limit := config.RayBufferLimit
	if limit <= 0 {
		limit = len(rays)
	}
	options := config.Options.withRecipe(scene.Recipe)
	results := make([]RayResult, 0, len(rays))
	pixels := make([]RenderPixel, 0, len(rays))
	digests := make([]SegmentDigest, 0, len(rays))
	bufferUsed := 0
	maxBuffer := 0

	for start := 0; start < len(rays); start += limit {
		end := start + limit
		if end > len(rays) {
			end = len(rays)
		}
		batch := rays[start:end]
		bufferUsed = len(batch)
		if bufferUsed > maxBuffer {
			maxBuffer = bufferUsed
		}
		for _, ray := range batch {
			result, err := IntegrateRay(scene, ray, options)
			if err != nil {
				if fe := AsError(err); fe != nil {
					fe.Tile = &tile
				}
				return nil, err
			}
			results = append(results, *result)
			pixels = append(pixels, RenderPixel{X: ray.Pixel.X, Y: ray.Pixel.Y, Radiance: result.Radiance, Color: result.Color})
			digests = append(digests, segmentDigest(*result))
		}
		bufferUsed = 0
	}

	sort.Slice(pixels, func(i, j int) bool {
		return pixels[i].Y < pixels[j].Y || (pixels[i].Y == pixels[j].Y && pixels[i].X < pixels[j].X)
	})
	transmittance := 0.0
	scattering := 0.0
	shadowSamples := 0
	for _, result := range results {
		transmittance += result.FinalTransmittance
		scattering += result.Scattering
		shadowSamples += result.ShadowSamples
	}
	averageT := transmittance / float64(len(results))
	averageS := scattering / float64(len(results))
	digestPayload, _ := json.Marshal(digests)
	digestSum := sha256.Sum256(digestPayload)
	contentHash := TileContentHashForCoord(scene, tile)
	cacheKey := TileCacheKey(scene, tile, results, options)
	return &TileResult{
		Tile: tile, Pixels: pixels, RayResults: results,
		RaySegmentDigest: hex.EncodeToString(digestSum[:]), SegmentDigests: digests,
		Transmittance: averageT, Scattering: scattering, AverageScattering: averageS,
		CacheKey: cacheKey, ContentHash: contentHash,
		RecipeVersion: scene.Recipe.Version, CameraVersion: scene.CameraTrack.Version,
		AlgoVersion: AlgoVersion, Options: options, ShadowSamples: shadowSamples,
		MaxRayBufferUsed: maxBuffer, Status: "rendered",
	}, nil
}

func segmentDigest(result RayResult) SegmentDigest {
	parts := make([]string, 0, len(result.Segments))
	for _, segment := range result.Segments {
		payload, _ := json.Marshal(struct {
			MediumIDs []string `json:"m"`
			Near      Fixed    `json:"n"`
			Far       Fixed    `json:"f"`
			T         float64  `json:"t"`
			S         float64  `json:"s"`
		}{segment.MediumIDs, segment.Near, segment.Far, segment.Transmittance, segment.Scattering})
		sum := sha256.Sum256(payload)
		parts = append(parts, hex.EncodeToString(sum[:8]))
	}
	return SegmentDigest{RayID: result.RayID, RayCacheKey: result.CacheKey, Segments: parts}
}

func TileContentHashForCoord(scene *Scene, tile TileCoord) string {
	ids := map[string]bool{}
	for _, ray := range RaysForTile(*scene, tile) {
		hits, err := IntersectRayMedia(ray, scene.Media)
		if err != nil {
			continue
		}
		segments, err := BuildRaySegments(ray, hits)
		if err != nil {
			continue
		}
		for _, segment := range segments {
			for _, id := range segment.MediumIDs {
				ids[id] = true
			}
		}
	}
	return tileContentHash(scene, ids)
}

func tileContentHash(scene *Scene, ids map[string]bool) string {
	hashes := make([]string, 0, len(ids))
	for id := range ids {
		medium := scene.Medium(id)
		if medium != nil {
			hashes = append(hashes, medium.ID+":"+medium.ContentHash())
		}
	}
	sort.Strings(hashes)
	payload, _ := json.Marshal(struct {
		Media  []string `json:"media"`
		Camera string   `json:"camera"`
		Light  Light    `json:"light"`
	}{hashes, scene.CameraTrack.ContentHash(), scene.Light})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func FindPixel(frame *Frame, x, y int) *RenderPixel {
	if frame == nil {
		return nil
	}
	for i := range frame.Pixels {
		if frame.Pixels[i].X == x && frame.Pixels[i].Y == y {
			return &frame.Pixels[i]
		}
	}
	return nil
}

type Frame struct {
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	Pixels        []RenderPixel `json:"pixels"`
	Tiles         []TileResult  `json:"tiles,omitempty"`
	Steps         int           `json:"steps"`
	ShadowSamples int           `json:"shadow_samples"`
	Mode          string        `json:"mode"`
}

func FrameFromTiles(scene *Scene, tiles []TileResult, mode string) *Frame {
	frame := &Frame{Width: scene.Viewport.Width, Height: scene.Viewport.Height, Mode: mode}
	for _, tile := range tiles {
		frame.Pixels = append(frame.Pixels, tile.Pixels...)
		frame.ShadowSamples += tile.ShadowSamples
		for _, ray := range tile.RayResults {
			frame.Steps += ray.Steps
		}
	}
	sort.Slice(frame.Pixels, func(i, j int) bool {
		return frame.Pixels[i].Y < frame.Pixels[j].Y || (frame.Pixels[i].Y == frame.Pixels[j].Y && frame.Pixels[i].X < frame.Pixels[j].X)
	})
	return frame
}

package engine

import (
	"sort"

	"fixedsampler/internal/hashutil"
	"fixedsampler/internal/model"
)

type NodeRecord struct {
	Seq          int                  `json:"seq"`
	ChunkIndex   int                  `json:"chunk_index"`
	ChunkX       int                  `json:"chunk_x"`
	ChunkY       int                  `json:"chunk_y"`
	CacheKey     string               `json:"cache_key"`
	ContentHash  string               `json:"content_hash"`
	InputHash    string               `json:"input_hash"`
	UpstreamKeys []string             `json:"upstream_keys"`
	Artifact     *model.LevelArtifact `json:"-"`
}

type RegionHash struct {
	Plane string     `json:"plane"`
	Rect  model.Rect `json:"rect"`
	Hash  string     `json:"hash"`
}

func HashRegion(plane string, rect model.Rect, values []int64) string {
	return hashutil.CanonicalHash(struct {
		Plane  string     `json:"plane"`
		Rect   model.Rect `json:"rect"`
		Values []int64    `json:"values"`
	}{plane, rect, values})
}

func HashTile(tile model.Tile) string {
	return HashRegion(tile.Plane, tile.Rect, tile.Values)
}

func HashTiles(tiles []model.Tile) string {
	hashes := make([]string, 0, len(tiles))
	for _, tile := range tiles {
		hashes = append(hashes, HashTile(tile))
	}
	return hashutil.CanonicalHash(hashes)
}

type cacheKeyPayload struct {
	AlgoVersion  string            `json:"algo_version"`
	ParamVersion string            `json:"param_version"`
	Seq          int               `json:"seq"`
	ChunkX       int               `json:"chunk_x"`
	ChunkY       int               `json:"chunk_y"`
	ChunkSize    int               `json:"chunk_size"`
	Stage        model.StageConfig `json:"stage"`
	InputRegions []RegionHash      `json:"input_regions"`
	UpstreamKeys []string          `json:"upstream_keys"`
}

func ComputeCacheKey(p *model.Project, stage model.StageConfig, chunkX, chunkY, chunkSize int, inputs []RegionHash, upstreamKeys []string) string {
	sort.Strings(upstreamKeys)
	return hashutil.CanonicalHash(cacheKeyPayload{
		AlgoVersion: model.AlgoVersion, ParamVersion: model.ParamVersion,
		Seq: stage.Seq, ChunkX: chunkX, ChunkY: chunkY, ChunkSize: chunkSize,
		Stage: stage, InputRegions: inputs, UpstreamKeys: upstreamKeys,
	})
}

func sourceCacheKey(p *model.Project, chunkX, chunkY, chunkSize int, regions []RegionHash) string {
	return hashutil.CanonicalHash(struct {
		AlgoVersion  string       `json:"algo_version"`
		ParamVersion string       `json:"param_version"`
		Seq          int          `json:"seq"`
		ProjectID    string       `json:"project_id"`
		ChunkX       int          `json:"chunk_x"`
		ChunkY       int          `json:"chunk_y"`
		ChunkSize    int          `json:"chunk_size"`
		Regions      []RegionHash `json:"regions"`
	}{model.AlgoVersion, model.ParamVersion, 0, p.ProjectID, chunkX, chunkY, chunkSize, regions})
}

func sortedRecords(records map[int]*NodeRecord) []*NodeRecord {
	out := make([]*NodeRecord, 0, len(records))
	for _, r := range records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkIndex < out[j].ChunkIndex })
	return out
}

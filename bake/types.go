// Package bake implements the deterministic terrain lighting bake kernel,
// chunk cache, dependency-aware invalidation, and consistency comparison.
package bake

import (
	"context"
	"fmt"
	"sort"

	"terrainbake/model"
)

// UnitRef identifies one chunk/time bake unit.
type UnitRef struct {
	ChunkID string `json:"chunk_id"`
	Time    int    `json:"time"`
}

// UnitID returns the stable cache identity for a chunk/time unit.
func UnitID(chunkID string, time int) string {
	return fmt.Sprintf("%s@%d", chunkID, time)
}

// NamedHash associates a source identity with its content hash.
type NamedHash struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// Dependencies is the explicit input and upstream cache contract of a unit.
type Dependencies struct {
	HeightHashes  []NamedHash `json:"height_hashes"`
	AlbedoHash    string      `json:"albedo_hash"`
	OccluderIDs   []string    `json:"occluder_ids"`
	OccluderHash  string      `json:"occluder_hash"`
	SunTime       int         `json:"sun_time"`
	SunHash       string      `json:"sun_hash"`
	SkyHash       string      `json:"sky_hash"`
	SourceChunks  []string    `json:"source_chunks"`
	EdgeIDs       []string    `json:"edge_ids"`
	UpstreamKeys  []string    `json:"upstream_keys"`
	TemporalKey   string      `json:"temporal_key,omitempty"`
	ParameterVer  int         `json:"parameter_version"`
	AlgorithmVer  string      `json:"algorithm_version"`
	DependencyIDs []string    `json:"dependency_ids"`
	OutputNodes   []string    `json:"output_nodes"`
}

// BakedUnit is the atomic output committed for one chunk at one solar time.
type BakedUnit struct {
	ChunkID          string              `json:"chunk_id"`
	Time             int                 `json:"time"`
	CacheKey         string              `json:"cache_key"`
	HeightHash       string              `json:"height_hash"`
	ContentHash      string              `json:"content_hash"`
	SunHash          string              `json:"sun_hash"`
	SkyHash          string              `json:"sky_hash"`
	ParameterVersion int                 `json:"parameter_version"`
	AlgorithmVersion string              `json:"algorithm_version"`
	Points           []model.PointResult `json:"points"`
	ShadowDigest     string              `json:"shadow_digest"`
	SkyDigest        string              `json:"sky_digest"`
	IrradianceDigest string              `json:"irradiance_digest"`
	Dependencies     Dependencies        `json:"dependencies"`
	BufferBatches    int                 `json:"buffer_batches"`
}

// Vertex returns one point by local coordinate.
func (u *BakedUnit) Vertex(x, z int) (*model.PointResult, bool) {
	for i := range u.Points {
		if u.Points[i].Vertex.X == x && u.Points[i].Vertex.Z == z {
			return &u.Points[i], true
		}
	}
	return nil, false
}

// BakeResult is a complete, stable-ordered baked series view.
type BakeResult struct {
	Units          map[string]*BakedUnit `json:"units"`
	CommitSequence []UnitRef             `json:"commit_sequence"`
	Rebaked        []UnitRef             `json:"rebaked"`
	Reused         []UnitRef             `json:"reused"`
	Stats          ScheduleStats         `json:"stats"`
}

// Unit looks up a chunk/time result.
func (r *BakeResult) Unit(chunkID string, time int) (*BakedUnit, bool) {
	u, ok := r.Units[UnitID(chunkID, time)]
	return u, ok
}

// BuildCommitSequence returns the canonical time/chunk submission order.
func BuildCommitSequence(p *model.Project) []UnitRef {
	times := make([]int, 0, len(p.Sun))
	for _, s := range p.Sun {
		times = append(times, s.Time)
	}
	sort.Ints(times)
	chunks := make([]string, 0, len(p.Chunks))
	for _, c := range p.Chunks {
		chunks = append(chunks, c.ID)
	}
	sort.Strings(chunks)
	out := make([]UnitRef, 0, len(times)*len(chunks))
	for _, t := range times {
		for _, id := range chunks {
			out = append(out, UnitRef{ChunkID: id, Time: t})
		}
	}
	return out
}

// Invalidation describes one cache entry invalidated by a source edit.
type Invalidation struct {
	ChunkID    string `json:"chunk_id"`
	Time       int    `json:"time"`
	Reason     string `json:"reason"`
	Dependency string `json:"dependency"`
}

// QueueEvent records a deliberate block caused by a scheduler budget.
type QueueEvent struct {
	UnitID   string `json:"unit_id"`
	Reason   string `json:"reason"`
	WaitNano int64  `json:"wait_nano"`
}

// ScheduleStats exposes actual budget behavior without affecting bake output.
type ScheduleStats struct {
	MaxParallelism  int          `json:"max_parallelism"`
	VertexBufferCap int          `json:"vertex_buffer_cap"`
	QueueBudget     int          `json:"queue_budget"`
	Workers         int          `json:"workers"`
	Enqueued        int          `json:"enqueued"`
	QueueEvents     []QueueEvent `json:"queue_events,omitempty"`
	BufferBatches   int          `json:"buffer_batches"`
}

// ScheduleOptions fixes runtime resource budgets.
type ScheduleOptions struct {
	MaxParallelism  int
	VertexBufferCap int
	QueueBudget     int
}

// DefaultScheduleOptions reads the project declaration.
func DefaultScheduleOptions(p model.BakeParams) ScheduleOptions {
	return ScheduleOptions{
		MaxParallelism:  p.MaxParallelism,
		VertexBufferCap: p.VertexBufferCap,
		QueueBudget:     p.QueueBudget,
	}
}

// Validate checks scheduler budgets.
func (o ScheduleOptions) Validate() error {
	if o.MaxParallelism <= 0 || o.VertexBufferCap <= 0 || o.QueueBudget <= 0 {
		return model.NewError(model.CodeValue, "scheduler budgets must be positive").WithField("schedule")
	}
	return nil
}

// UnitStore is the persistence boundary used by the engine. Implementations
// must save each unit atomically and delete batches atomically.
type UnitStore interface {
	LoadUnits(ctx context.Context) ([]*BakedUnit, error)
	SaveUnit(ctx context.Context, unit *BakedUnit) error
	DeleteUnits(ctx context.Context, refs []UnitRef) error
}

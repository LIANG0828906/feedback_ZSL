// Package store contains the durable chunk checkpoint boundary used by the
// volumetric renderer. The cgo build uses the system SQLite ABI directly;
// non-cgo builds use an atomic, standard-library file backend.
package store

import (
	"errors"
	"time"
)

var ErrNotFound = errors.New("chunk checkpoint not found")

// ChunkRecord is one atomically committed screen-chunk checkpoint.
type ChunkRecord struct {
	ChunkX           int     `json:"chunk_x"`
	ChunkY           int     `json:"chunk_y"`
	CommitSeq        int64   `json:"commit_seq"`
	RayDigest        string  `json:"ray_digest"`
	CacheKey         string  `json:"cache_key"`
	ContentHash      string  `json:"content_hash"`
	RecipeVersion    string  `json:"recipe_version"`
	AlgorithmVersion string  `json:"algorithm_version"`
	CameraVersion    string  `json:"camera_version"`
	SceneVersion     string  `json:"scene_version"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	Transmittance    float64 `json:"transmittance"`
	Scattering       float64 `json:"scattering"`
	Dependencies     []byte  `json:"dependencies,omitempty"`
	Payload          []byte  `json:"payload"`
	CreatedUnixUS    int64   `json:"created_unix_us"`
}

// CommitRecord is the stable, row-major commit journal for one chunk.
type CommitRecord struct {
	Seq             int64  `json:"seq"`
	ChunkX          int    `json:"chunk_x"`
	ChunkY          int    `json:"chunk_y"`
	CacheKey        string `json:"cache_key"`
	CommittedUnixUS int64  `json:"committed_unix_us"`
}

// Store serializes chunk transactions. Implementations must be safe for
// concurrent callers and must not interleave writes from two chunks.
type Store interface {
	PutChunk(rec ChunkRecord) error
	GetChunk(x, y int) (ChunkRecord, error)
	ListChunks() ([]ChunkRecord, error)
	DeleteChunk(x, y int) error
	TruncateFrom(seq int64) error
	Clear() error
	Commits() ([]CommitRecord, error)
	Close() error
	Path() string
	Backend() string
}

// Open opens the checkpoint store. With cgo this is a real SQLite database;
// otherwise the path is used as the base name of an atomic JSON snapshot.
func Open(path string) (Store, error) {
	if path == "" {
		return nil, errors.New("checkpoint path is empty")
	}
	return openStore(path)
}

func nowUS() int64 { return time.Now().UnixMicro() }

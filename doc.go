// Package thermalcache implements a deterministic hot/cold cache domain
// kernel. It loads three equivalent schema encodings into one normalized
// project model, advances fixed-point heat scores over a logical timeline,
// plans capacity-constrained migration batches, gates reads and writes while a
// migration is in flight, persists SQLite checkpoints, and supports incremental
// recomputation from a cached prefix.
//
// All business failures are returned as *BizError with a stable E_xxx code and
// locatable context. The package has no external Go module dependency; on Linux
// it binds to the system libsqlite3.so.0 through cgo.
package thermalcache

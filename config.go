package acousticfield

import (
	"fmt"
	"runtime"
)

// Options affect the integer reconstruction and scheduling. PropagationRounds
// is the number of chunk visits made by a source packet; truncation after that
// finite budget is reported as escape energy.
type Options struct {
	MaxReflections      int   `json:"max_reflections"`
	PropagationRounds   int   `json:"propagation_rounds"`
	PathToleranceMicros int64 `json:"path_tolerance_micros"`
	ConservationResidue int64 `json:"conservation_residue"`
	MaxPathsPerChunk    int   `json:"max_paths_per_chunk"`
	MaxParallel         int   `json:"max_parallel"`
	QueueBudget         int   `json:"queue_budget"`
	PathBufferPerWorker int   `json:"path_buffer_per_worker"`
}

func DefaultOptions() Options {
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 4 {
		workers = 4
	}
	return Options{
		MaxReflections:      2,
		PropagationRounds:   3,
		PathToleranceMicros: 5_000,
		ConservationResidue: 16,
		MaxPathsPerChunk:    64,
		MaxParallel:         workers,
		QueueBudget:         8,
		PathBufferPerWorker: 64,
	}
}

func (o Options) Normalized() Options {
	d := DefaultOptions()
	if o.MaxReflections <= 0 {
		o.MaxReflections = d.MaxReflections
	}
	if o.PropagationRounds <= 0 {
		o.PropagationRounds = o.MaxReflections + 1
	}
	if o.PathToleranceMicros <= 0 {
		o.PathToleranceMicros = d.PathToleranceMicros
	}
	if o.ConservationResidue <= 0 {
		o.ConservationResidue = d.ConservationResidue
	}
	if o.MaxPathsPerChunk <= 0 {
		o.MaxPathsPerChunk = d.MaxPathsPerChunk
	}
	if o.MaxParallel <= 0 {
		o.MaxParallel = d.MaxParallel
	}
	if o.QueueBudget <= 0 {
		o.QueueBudget = d.QueueBudget
	}
	if o.PathBufferPerWorker <= 0 {
		o.PathBufferPerWorker = d.PathBufferPerWorker
	}
	return o
}

func (o Options) Validate() error {
	o = o.Normalized()
	if o.MaxReflections > 8 {
		return valueError("options", "max_reflections", o.MaxReflections, 1, 8)
	}
	if o.PropagationRounds > 16 {
		return valueError("options", "propagation_rounds", o.PropagationRounds, 1, 16)
	}
	if o.MaxParallel > 256 {
		return valueError("options", "max_parallel", o.MaxParallel, 1, 256)
	}
	if o.QueueBudget > 1<<20 {
		return valueError("options", "queue_budget", o.QueueBudget, 1, 1<<20)
	}
	return nil
}

func (o Options) Version() string {
	o = o.Normalized()
	// Scheduling knobs are deliberately excluded: changing worker count or
	// queue capacity must not invalidate acoustic results.
	return canonicalHash(struct {
		MaxReflections      int   `json:"max_reflections"`
		PropagationRounds   int   `json:"propagation_rounds"`
		PathToleranceMicros int64 `json:"path_tolerance_micros"`
		ConservationResidue int64 `json:"conservation_residue"`
		MaxPathsPerChunk    int   `json:"max_paths_per_chunk"`
		PathBufferPerWorker int   `json:"path_buffer_per_worker"`
	}{
		MaxReflections: o.MaxReflections, PropagationRounds: o.PropagationRounds,
		PathToleranceMicros: o.PathToleranceMicros, ConservationResidue: o.ConservationResidue,
		MaxPathsPerChunk: o.MaxPathsPerChunk, PathBufferPerWorker: o.PathBufferPerWorker,
	})
}

func (o Options) String() string {
	return fmt.Sprintf("refl=%d rounds=%d workers=%d queue=%d", o.MaxReflections, o.PropagationRounds, o.MaxParallel, o.QueueBudget)
}

// Package stage implements a deterministic stage-lighting show preview kernel.
//
// Projects can be loaded from three distinct JSON schemas, converted to an
// integer internal model, evaluated at logical sample times, sampled into 3D
// light-cone cross-sections, accumulated into stable spatial cells, and
// committed to atomic file checkpoints. Editing support is provided by a
// dependency graph and a window-level invalidation report.
//
// The package uses only the Go standard library. Unit conversions are integer
// only; floating point is confined to deterministic geometric construction from
// already-resolved integer poses.
package stage

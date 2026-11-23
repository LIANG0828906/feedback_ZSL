// Package terrainbake implements a deterministic terrain elevation to
// tangent-space normal-map baking pipeline.
//
// The package provides a self-contained sample generator, schema validation,
// per-pixel normal and slope baking, deterministic bilateral seam
// constraints, dependency-aware incremental invalidation, resource-bounded
// parallel scheduling, atomic checkpoint/resume, three-path pixel equality
// verification, and offline HTML diagnostics.
//
// All terrain data, material weights, seam constraints and truth samples are
// local and generated from a fixed seed. The only external representation is
// the project JSON; all images are standard PNG files.
package terrainbake

// Package spatialpick provides deterministic, fixed-point spatial editing
// primitives for game editors and interactive media.
//
// The package is organized around immutable query indexes, versioned derived
// caches, explicit dependency invalidation, pivot-space transactions, and
// atomically published snapshots. All world transforms are stored as int64
// fixed-point affine matrices so repeated runs and thread schedules produce
// bit-identical results.
package spatialpick

// Package stage provides deterministic stage-lighting project validation,
// frame evaluation, grouped cue arbitration, resource-bounded window
// scheduling, SQLite checkpoints, and cross-path consistency verification.
//
// Scheduling output is a pure function of project content, scheduling
// parameters, and logical time. Worker concurrency and completion order do not
// affect frames, cue consumption, or checkpoint commit order.
package stage

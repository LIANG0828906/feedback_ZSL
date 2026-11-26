// Package dialogue implements a deterministic, integer-only dialogue loudness
// kernel: schema loading and conversion, frame envelope measurement, gain
// calibration, lifecycle enforcement, dependency invalidation, atomic
// checkpoints, and three-path reconstruction verification.
//
// The public model is always expressed in sample points and per-myriad fixed
// point. No measurement or rounding path accumulates floating-point values.
package dialogue

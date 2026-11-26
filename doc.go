// Package loudness implements a deterministic, integer-only reconstruction
// kernel for game-audio loudness envelopes.
//
// The package normalizes three game-audio schema versions into one internal
// model, measures deterministic mono PCM frame-by-frame, calibrates per-clip
// gain against a target loudness, tracks exact dependency invalidation, and
// persists per-clip checkpoints for full, incremental, and resumed builds.
//
// All public time values use samples. Gain is linear Q12 (4096 == 1.0),
// loudness is Q8 dB (256 == 1 dB), weight is Q8 (256 == 1.0), and amplitude is
// Q16 (65536 == 1.0 full scale). No floating-point accumulation is used by the
// conversion or measurement pipeline.
package loudness

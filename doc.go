// Package dialoguecal implements a deterministic dialogue loudness and
// per-frame acoustic-feature kernel.
//
// It generates fixed-seed mono PCM fixtures, loads three dialogue schema
// revisions into one integer-only model, extracts frame features under bounded
// parallelism, calibrates segment loudness, propagates precise invalidation,
// and checkpoints committed frame intervals through database/sql.
package dialoguecal

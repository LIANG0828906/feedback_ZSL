"""Broadcast-center multichannel loudness metering pipeline.

Modules:
- config:    JSON-backed configuration (parallelism / memory budget)
- errors:    stable error codes (E_WAV_INVALID, E_BUSY, E_VERSION_STALE,
             E_STALE_HANDLE, E_REPORT_MISMATCH, E_CONFIG)
- wavfile:   5.1 WAV reading and weighted downmix
- blocks:    observation-block slicing, K-weighting, true peak
- gating:    two-stage gated integrated loudness
- scheduler: deterministic parallel block scheduler with trace
- versions:  immutable published versions and isolated queries
- snapshot:  atomic snapshots and crash recovery
- recompute: incremental recompute and gating invalidation
- samplegen: deterministic local sample generator
- pipeline:  end-to-end metering helpers
"""

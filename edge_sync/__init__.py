"""Local multi-egress validation service for edge telemetry archive sync.

The package is intentionally dependency free (Python standard library only)
so it runs inside the 2 vCPU / 1.25 GiB resource envelope on Windows and on
the local CI/WSL verification host.  Every random decision is derived from a
fixed seed through :mod:`edge_sync.rng`, which makes a full run repeatable.
"""

__version__ = "1.0.0"

# Stable business error codes (part of the report / status contract).
ERR_SIGNAL_UNSUPPORTED = "SIG_PROBE_UNSUPPORTED"
ERR_SIGNAL_COLLECTION = "SIG_PROBE_ERROR"
ERR_SHARD_CHECKSUM = "SHARD_CHECKSUM_MISMATCH"
ERR_SHARD_TIMEOUT = "SHARD_UPLOAD_TIMEOUT"
ERR_SHARD_ATTEMPTS = "SHARD_MAX_ATTEMPTS"
ERR_EGRESS_CAP = "EGRESS_CONCURRENCY_CAP"
ERR_HTTP = "HTTP_UPLOAD_FAILED"

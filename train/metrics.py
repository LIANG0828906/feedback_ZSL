from __future__ import annotations

from prometheus_client import Counter, Gauge, Histogram


STEP_DURATION = Histogram(
    "training_step_duration_seconds",
    "Wall-clock duration from first queued submission to atomic step commit.",
    buckets=(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.4, 0.8, 2.0, 5.0),
)
GRADIENT_SUBMIT_DURATION = Histogram(
    "training_gradient_submit_duration_seconds",
    "Coordinator service time for a gradient submission RPC.",
    buckets=(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.4, 0.8, 2.0, 5.0),
)
WORKERS_ALIVE = Gauge(
    "training_workers_alive",
    "Registered workers whose last heartbeat is within the configured timeout.",
)
SHARDS_UP = Gauge(
    "training_shards_up",
    "Registered parameter shards whose last heartbeat is within the configured timeout.",
)
SHARD_UP = Gauge(
    "training_shard_up",
    "Per-shard liveness indicator.",
    ("shard_id",),
)
SHARD_STATUS = Gauge(
    "training_shard_status",
    "Per-shard status indicator; exactly one status label is 1 for a known shard.",
    ("shard_id", "status"),
)
GRADIENT_SUBMISSIONS = Counter(
    "training_gradient_submissions_total",
    "Gradient submissions grouped by final result.",
    ("result",),
)
GRADIENT_DISCARDED = Counter(
    "training_gradient_discarded_total",
    "Discarded gradient submissions grouped by reason.",
    ("reason",),
)
SHARD_GRADIENT_SUBMISSIONS = Counter(
    "training_shard_gradient_submissions_total",
    "Gradient applications routed to each parameter shard.",
    ("shard_id",),
)
SHARD_GRADIENT_DISCARDED = Counter(
    "training_shard_gradient_discarded_total",
    "Discarded gradient attempts associated with each parameter shard.",
    ("shard_id", "reason"),
)
STALE_REJECTIONS = Counter(
    "training_stale_rejections_total",
    "Stale gradient rejections associated with each parameter shard.",
    ("shard_id",),
)
TAKEOVERS = Counter(
    "training_shard_takeovers_total",
    "Completed parameter-shard takeovers.",
    ("from_shard", "to_shard"),
)
PARAM_VERSION = Gauge(
    "training_parameter_version",
    "Latest atomically applied global parameter version.",
)
PENDING_SUBMISSIONS = Gauge(
    "training_pending_submissions",
    "Gradient submissions buffered for not-yet-complete steps.",
)
TAKEOVER_IN_PROGRESS = Gauge(
    "training_takeover_in_progress",
    "Whether a parameter-shard takeover is currently being installed.",
)

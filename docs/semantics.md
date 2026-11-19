# Semantics and invariants

This note records the precise rules the implementation enforces. The tests and
acceptance runner treat these as invariants.

1. Input order is manifest batch order, not event-time order. The watermark is
   monotone: `max(declared_event_time of consumed batches) - lateness`.
2. A tumbling window is `[floor(event_time/width)*width, +width)`.
3. On consuming a batch, an event whose window end is `<=` the new watermark
   is late: it increments `late_windows` and is excluded from the aggregate.
   A window whose end is `<=` the watermark is closed exactly once.
4. Deduplication uses a stable natural key (default vehicle, trip, event time,
   event type). The first occurrence in manifest order wins.
5. A completed window result is immutable. Late events never update
   `completed_windows` or the final result row.
6. Stream provisional versions are marked `is_final = 0` and may be updated by
   a later batch. Final versions are marked `is_final = 1` and may not be
   overwritten by a provisional version.
7. Final flush uses an infinite watermark, so batch and stream output the same
   final set of windows.
8. Routing is a pure function of deduplicated input counts. A vehicle is
   normally one shard; a hot vehicle is split by trip (or by event fingerprint
   if one trip is still too large) and merged back to one result row.
9. `filter` and `group_by_vehicle` commit atomically. Each window shard
   commits its accumulators, watermark, consumed batch, and closed windows
   atomically. A stage batch becomes visible only after every shard has
   committed.
10. Merge reads accumulators only after every window shard committed. A
    killed/restarted shard is replayed from its last committed state; no
    downstream stage reads a missing or zeroed accumulator.
11. Output publication is idempotent: `window_results` is keyed by
    `(job, vehicle, window_start)`, `result_versions` by
    `(job, vehicle, window_start, publish_seq)`, and the outbox by
    `(job, batch, vehicle, window_start)`.
12. Re-running a completed job does not re-emit rows. Re-entering the same
    commit point repeats only idempotent statements.


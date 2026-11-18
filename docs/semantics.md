# Window semantics

All timestamps are UTC and are represented internally as integer microseconds.
A minute window is the half-open interval `[window_start, window_start+60s)`.

## Canonical event and deduplication

The canonical key is:

```
(side, payment_id, event_minute)
```

`side` is `PAYMENT` or `REFUND`.  It is required: a payment and its refund
share a payment id and minute but are different business events.  For one
canonical key only one event is retained:

1. smallest `event_time` wins;
2. if `event_time` is equal, the first arrival wins.

Arrival order is deterministic: history shards in manifest order, then rows in
Parquet order, then refund batches in `batch_index` order and JSONL lines in
file order.  The same rule is used for both sides and after recovery.

If an earlier event arrives at an already closed window, the committed result
is not changed.  It is counted in the window's late statistics (and, when it is
a duplicate, also in the dedup-dropped statistic).  Exact late duplicates are
therefore visible in both counters; late statistics never modify output.

## Join and output

For each canonical `(payment_id, window_start)`:

* `pay_count = 1` if a canonical payment exists;
* `refund_count = 1` if a canonical refund exists and
  `refund.event_time >= payment.event_time`;
* amounts are the canonical event amounts;
* `net = payment_amount - refund_amount`.

Minute-level output sums joined key-window rows.  A refund without a payment,
or a refund earlier in the same minute than its payment, is reported through
`join_diagnostics.unmatched_refunds`; it does not create a minute result.
Amounts are exact integer cents internally and formatted as decimal(18,2)
strings in the report.

`key_windows.csv` exposes the per-payment/per-minute join detail; `windows.csv`
is the minute aggregate consumed by strategy review.

## Watermark

Only refund batches carry `declared_event_time`.  Historical payment shards are
seed state and do not advance the watermark.  This lets payment windows remain
open while same-minute refunds arrive.

For every consumed refund batch:

```
new_watermark = max(old_watermark, declared_event_time - allowed_lateness)
```

The watermark never regresses, including when batches are missing, appended,
or replayed after recovery.  A minute closes when:

```
watermark >= window_start + 60s
```

Therefore `allowed_lateness=0` closes a minute as soon as its declared batch
time reaches the minute end; a larger tolerance closes it later and admits more
out-of-order events.  All events in a batch are processed before the batch's
watermark is applied, so intra-batch disorder is not misclassified as late.

At end of input every still-open key-window is committed with
`close_reason=end_of_input`.  Report window rows include `close_reason`,
`commit_seq`, and `closed_watermark` for audit.

## Window commit

One consumed shard or batch is one SQLite transaction.  The transaction covers:

* canonical event updates for affected key-windows;
* window aggregates;
* minute result insert;
* `key_windows.closed` commit flags and `closed_minutes` rows;
* watermark, dedup/late counters;
* the consumed-input marker and commit log entry.

The transaction is atomic.  A committed minute is never updated again.

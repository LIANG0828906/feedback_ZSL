# Architecture

## Topology

`SnapshotGateway` runs as one process and simulates four independent SQLite
nodes on the local machine:

- `data/primary/orders-primary.db`: writable primary.
- `data/replicas/replica-{1..3}/orders-replica-{n}.db`: read-only replica
  snapshots.

Every replica owns a real SQLite file and an independent
`data_snapshot_version`. Replica catch-up uses the SQLite online backup API.
The gateway serializes writes to the primary with a process-local write gate,
uses WAL mode, and gives every replica its own read/sync lock. A page query is
executed inside one deferred read transaction, so count and rows always come
from one SQLite snapshot.

## Snapshot protocol

1. Read the primary `data_snapshot_version`.
2. Read the version of all three replicas.
3. If all four versions match, select one replica and execute the page inside
   a read transaction.
4. If any version differs, synchronously trigger all-replica catch-up to the
   current primary version, then return `SNAPSHOT_LAG` for the triggering
   request with `versionsBefore`, `versionsAfter`, and catch-up timing. No
   order rows are returned.
5. The next request succeeds after the replicas are aligned.

The implementation deliberately requires all three replicas, which is
stricter than the minimum "at least two" requirement and prevents a stale
third replica from serving a later request.

## Key orders and plans

The old key order is:

```sql
ORDER BY Status, OrderId
```

and is pinned with `INDEXED BY idx_old_status_orderid`.

The new key order is:

```sql
ORDER BY TenantId, Status, UpdatedAt, OrderId
```

and is pinned with `INDEXED BY idx_new_tenant_status_updated_orderid`.

Both statements use the same tenant/status predicate. Only the row sequence can
differ. Before data is returned, the gateway reads SQLite
`EXPLAIN QUERY PLAN` for both the count and page statements and requires:

- The expected index appears in both plans.
- The page statement uses `SEARCH`, not `SCAN`.
- No plan line performs a full scan of `orders`.
- The page plan does not need a temporary B-tree sort.

Any mismatch returns `E_PLAN_MISMATCH` with all plan lines and the query filter.

## Index switch and rollback

Both indexes always coexist physically. A switch changes only control metadata:

| Stage | old index | new index | active generation |
| --- | --- | --- | --- |
| Initial | `ACTIVE` | `READY` | `OLD` |
| Switch | `ACTIVE_LEGACY` | `ACTIVE` | `NEW` |
| Rollback | `ACTIVE` | `READY` | `OLD` |

`index_catalog`, `app_metadata`, and `switch_audit` are updated in one
`BEGIN IMMEDIATE` transaction. The data snapshot version is intentionally not
changed by metadata transitions. Therefore a rollback keeps the exact same
data snapshot and cannot discard new-key-order writes.

## Concurrency and resource limits

- Global SQLite connection gate: default 8 connections. Acquisition is
  non-blocking; over-cap requests return `E_CONN_LIMIT` and HTTP 429.
- Primary data writes and index transitions share one write gate.
- Replica queries and online backups are separated by per-replica locks.
- WAL plus deferred read transactions keep count and page rows on one
  snapshot.
- Default workload uses two CPU cores comfortably: 20,000 rows, one gateway
  process, four database files, and no external service.

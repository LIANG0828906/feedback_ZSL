# Shared WAL Archive Service

A dependency-free Python 3.11+ service that turns a shared, NFS-style
PostgreSQL WAL archive into an auditable catalog. It indexes every segment by
instance, timeline and segment number, validates content, resolves conflicting
versions, selects reachable recovery points, and persists the result in SQLite.

The implementation uses only the Python standard library. It does not require
a PostgreSQL server, network access, or third-party packages.

## Quick Start

```bash
# 1. Generate the deterministic full archive (~250 files, ~130 MiB).
python scripts/generate_archive.py --root ./archive --profile full

# 2. Scan it, select/verify a recovery point, and print a report.
python app.py --root ./archive --db ./wal_archive.sqlite3 scan
python app.py --root ./archive --db ./wal_archive.sqlite3 \
  recover --instance pg-55432 --timeline 1 \
  --target-time 2026-01-01T00:01:30Z
python app.py --root ./archive --db ./wal_archive.sqlite3 report

# 3. Run the HTTP service (scans once at startup).
python app.py --root ./archive --db ./wal_archive.sqlite3 \
  serve --host 127.0.0.1 --port 8080
```

Global options (`--root`, `--db`, `--segment-size`, ...) go before the
subcommand. Environment alternatives are `WAL_ARCHIVE_ROOT`,
`WAL_ARCHIVE_DB`, `WAL_SEGMENT_SIZE`, `WAL_ARCHIVE_HOST` and
`WAL_ARCHIVE_PORT`.

Keep the SQLite file outside the archive root. The service never writes to the
archive tree; placing the catalog there would also be reported as a layout
error on the next scan.

## Archive Layout

```text
ARCHIVE_ROOT/
  instances/
    pg-55432/
      segments/
        000000010000000000000001
        00000001000000000000007E.partial       # staging / writing
        00000001000000000000007F
        00000001000000000000007F.writer.json   # active-writer marker
      timeline-markers/                         # optional, per instance
    pg-55433/
      segments/
  timeline-markers/                             # shared authoritative markers
    00000001.json
    00000002.json
```

Instance directory names must match `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`.
Recognised entries below an instance are only `segments` and optional
`timeline-markers`. Anything else is reported with a stable error code and
path, and is never indexed as a usable segment.

Segment names use PostgreSQL's 24 uppercase hex digits:

```text
TTTTTTTT LLLLLLLL SSSSSSSS
timeline   log      segment
```

For a 512 KiB segment size, one log contains 8192 segments, so
`segno = log * 8192 + segment`. The mapping is derived from the configured
`--segment-size`. Lowercase names are normalised; malformed names are rejected
with `WAL003`.

### Timeline Markers

Each shared marker is JSON:

```json
{
  "timeline": 2,
  "parent_timeline": 1,
  "switch_segment": 1,
  "switch_lsn": "0/01000000",
  "switched_at": "2026-01-01T00:03:00Z",
  "authoritative_instance": "pg-55433",
  "reason": "promotion after local recovery"
}
```

The filename stem must be the 8-digit timeline. PostgreSQL `.history` files
are also accepted as parent-timeline markers. When several markers exist for a
timeline, the latest `switched_at` (then digest) is effective. A malformed
marker produces `WAL006` and is excluded.

## WAL Sample Semantics

The generated sample keeps PostgreSQL's relevant framing:

- 8 KiB pages, WAL magic `0xD118`, 20-byte page header and 36-byte long
  header on page 0; record data begins at offset 40 on page 0 and 24 on later
  pages.
- Segment size 512 KiB by default (`pages_total = 64`).
- A private `xlp_info` bit (`0x4000`) marks a CRC32 page trailer in the last
  four bytes of each page. The CRC covers the preceding 8188 bytes.
- A PostgreSQL-style WAL record header with CRC32C is written as a fallback.
  The validator also understands absolute or segment-relative page addresses,
  so standard PostgreSQL record CRCs can be checked when no sample trailer
  flag is present.

Validation reads files with `rb` only. A size, mtime or inode change during the
read is treated as an in-progress write, never as a complete segment.

Classification rules:

| Classification | Meaning |
| --- | --- |
| `complete` | Expected length, valid magic/header, and every parsed checksum passes. |
| `truncated` | File is shorter than the configured segment size but has a usable prefix. |
| `checksum_failed` | Bad magic/length, an unparseable non-empty page, or a failed page/record CRC. |

A truncated version is sticky. Once a path has been observed as truncated, the
same path cannot later become complete, even if its content is replaced. This
is deliberate audit behaviour. Historical content versions remain queryable.
Use `clear-quarantine --instance ID --filename NAME` only as an explicit
operator override.

## Concurrent Writes and Versions

The scanner excludes a segment from the complete view when any of these holds:

- the file uses a staging suffix: `.partial`, `.tmp`, `.inprogress`, `.writing`;
- a sibling `<segment>.writer.json` exists;
- a recently modified short file changes size/mtime during a stability probe;
- the file changes while it is being read/hashed.

Writer markers are treated conservatively: their presence always excludes the
segment. Remove a stale `<segment>.writer.json` after a crashed writer before
rescanning.

Content identity is the SHA-256 digest of the entire file. Versions are unique
by `(instance, timeline, segno, digest)`. Identical files written by two
instances are not conflicts. Differing digests at the same logical segment are
all retained:

- the first completed, checksum-valid version is marked as canonical
  (`conflict = false`); later differing versions are marked `conflict = true`;
- every candidate, current or historical, can be listed through
  `GET /v1/conflicts`;
- recovery selection is deterministic: the timeline marker's
  `authoritative_instance` wins when it has a complete candidate, then the
  requested source instance, then the first completed valid version.

The chosen version and reason (plus marker digest and all candidates) are
stored in the recovery point's selection trace.

## Recovery Point Selection

Input is an instance, timeline, ISO-8601 target time, and optional source
instance. A segment is eligible when its archive completion time (file mtime)
is at or before the target. Selection then:

1. Takes the highest eligible segment number on that timeline.
2. Starts the predecessor chain at the timeline marker's `switch_segment`
   (or the first indexed segment when no marker exists).
3. Chooses exactly one complete version per segment using the rules above.
4. Reports every missing segment number in the chain.
5. Live-validates every selected file, comparing the digest to the catalog and
   rechecking header, length and page checksums.

If the chain has a gap, the request returns HTTP 409 / `WAL011` with the
explicit `gaps` list. No recovery point is committed in that case. The chain is
returned oldest-to-newest; the last element is the selected head.

### Idempotency and SQLite Transactions

The catalog is `wal_archive.sqlite3` by default (configurable). Relevant
tables:

- `wal_versions` -- every content version, validation counters and conflict
  flag;
- `in_progress` -- writing segments from the latest scan;
- `quarantine` -- sticky truncated paths;
- `timeline_markers`;
- `recovery_points` -- only fully committed points;
- `validations` -- scan and recovery verification rows;
- `scans`, `scan_errors`.

A recovery point's chain, selected versions, gaps and per-segment validation
conclusions are inserted in one `BEGIN IMMEDIATE` transaction. There are no
draft recovery-point rows. The request key is derived from
`instance|timeline|normalised_target_time`; an existing committed key is
returned unchanged forever. Restarting the service therefore resumes from
committed state rather than partial in-memory selections.

## HTTP API

All JSON responses use sorted keys. Errors have this shape:

```json
{
  "error": {
    "code": "WAL011",
    "name": "E_RECOVERY_GAP",
    "message": "predecessor chain has gaps; no reachable recovery point was committed",
    "details": {"instance": "pg-55432", "timeline": 1, "gaps": [47, 48]}
  }
}
```

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness and version. |
| `POST` | `/v1/scan` | Rebuild and commit the archive view. |
| `GET` | `/v1/archive?instance=&timeline=` | Current versions, writing segments, scan errors. |
| `GET` | `/v1/conflicts?timeline=&segno=` | Current and historical conflicting versions. |
| `POST` | `/v1/recovery-points` | Select, verify and commit a recovery point. |
| `GET` | `/v1/recovery-points` | List committed points. |
| `GET` | `/v1/recovery-points/{id}` | Read one committed point. |
| `POST` | `/v1/validate` | Live-validate one segment version. |
| `GET` | `/v1/report` | Deterministic archive report. |
| `GET` | `/v1/status?instance=&timeline=` | Compact JSON status, no host paths. |
| `GET` | `/metrics` | Prometheus text exposition. |

Recovery request:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/recovery-points \
  -H 'Content-Type: application/json' \
  -d '{
    "instance": "pg-55432",
    "timeline": 1,
    "target_time": "2026-01-01T00:01:30Z",
    "source_instance": "pg-55432",
    "refresh": true
  }'
```

`refresh` defaults to `true` for HTTP recovery requests so an active writer is
detected at request time.

Status summary:

```json
{
  "chain_length": 45,
  "counts": {
    "checksum_failed_segments": 1,
    "conflict_versions": 2,
    "gap_segment_numbers": [47, 48],
    "segment_count": 123,
    "truncated_segments": 1,
    "usable_bytes": 62390272,
    "writing_segments": 2
  },
  "current_recovery_point": {
    "chain_length": 45,
    "gaps": [],
    "head_segno": 45,
    "id": "rp-...",
    "target_time": "2026-01-01T00:01:30.000000Z"
  },
  "instance": "pg-55432",
  "latest_validation": {"classification": "complete", "checksum_pass_rate": 1.0},
  "timeline": 1
}
```

The status response contains only relative/catalog identifiers, never the
absolute archive root.

## Prometheus Metrics

Every instance/timeline pair exposes:

```text
wal_archive_segments_total
wal_archive_usable_bytes
wal_archive_writing_segments
wal_archive_truncated_segments_total
wal_archive_checksum_failures_total
wal_archive_conflict_versions_total
wal_archive_committed_recovery_points_total
```

Labels are stable: `{instance="...",timeline="<decimal>"}`. There is also a
global `wal_archive_scan_errors_total`.

## Archive Report

`GET /v1/report` (or `report --output FILE`) contains:

- per-instance/per-timeline segment counts, usable bytes, writing/truncated/
  checksum-failed counts, conflict versions, gap segment numbers, committed
  recovery-point counts, and checksum pass rate;
- every committed recovery point's chain and the selected instance/digest for
  each segment;
- markers, writing segments, and scan errors.

Lists and object keys are sorted. Repeating a report over the same directory
produces identical values except `generated_at`, `duration_ms`, and explicit
timestamps such as `created_at` and file mtimes.

## Stable Error Codes

| Code | Name | Meaning |
| --- | --- | --- |
| `WAL001` | `E_ARCHIVE_ROOT_MISSING` | Root is absent or not a directory. |
| `WAL002` | `E_LAYOUT_UNEXPECTED` | Unexpected directory/file in the layout. |
| `WAL003` | `E_WAL_NAME_INVALID` | Segment filename/number cannot be parsed. |
| `WAL004` | `E_SEGMENT_READ_ERROR` | Segment could not be read. |
| `WAL005` | `E_SEGMENT_CHANGED_DURING_SCAN` | Content changed while reading. |
| `WAL006` | `E_TIMELINE_MARKER_INVALID` | Marker is malformed. |
| `WAL007` | `E_INVALID_REQUEST` | Invalid API/CLI argument. |
| `WAL008` | `E_SOURCE_INSTANCE_UNKNOWN` | Instance is not in the current view. |
| `WAL009` | `E_TIMELINE_UNKNOWN` | Timeline is not present for the instance. |
| `WAL010` | `E_NO_RECOVERY_POINT` | No eligible complete segment. |
| `WAL011` | `E_RECOVERY_GAP` | Predecessor chain has gaps; nothing committed. |
| `WAL012` | `E_SEGMENT_TRUNCATED` | Reserved for explicit truncated responses. |
| `WAL013` | `E_SEGMENT_CHECKSUM_FAILED` | Reserved for explicit checksum failures. |
| `WAL014` | `E_RECOVERY_VERIFY_FAILED` | Live chain verification failed. |
| `WAL015` | `E_DATABASE_ERROR` | SQLite open/initialisation failure. |
| `WAL016` | `E_DATABASE_BUSY` | SQLite write lock timeout. |
| `WAL017` | `E_METHOD_NOT_ALLOWED` | Unsupported method on a known route. |
| `WAL018` | `E_NOT_FOUND` | Unknown recovery point or endpoint. |
| `WAL019` | `E_INTERNAL` | Unexpected internal failure. |
| `WAL020` | `E_UNSAFE_PATH` | Catalog path escapes the archive root. |

Errors caused by archive entries include a relative `path`; absolute host
paths are not exposed.

## Generator and Determinism

`scripts/generate_archive.py` (or `app.py ... generate`) uses a fixed seed and
covers two instances, two timelines, about 120 jointly referenced segments,
four differing-version conflicts, two truncated files, two checksum failures,
an internal gap, timeline markers, partial files, an active writer, malformed
names and an unexpected instance entry.

The full profile produces 250 segment versions (~130 MiB), with 120 logical
segments written by both instances. Generated segment bytes, mtimes, markers
and writing sidecars are deterministic for a given seed. The small profile
keeps the same semantics for tests.

Useful full-profile targets:

```text
2026-01-01T00:01:30Z  timeline 1, reachable point before gap 47/48
2026-01-01T00:02:00Z  timeline 1, unreachable point with gaps [47, 48]
2026-01-01T00:05:30Z  timeline 2, authoritative instance pg-55433
```

## Verification

```bash
python -m unittest discover -s tests -v
```

The suite covers complete/truncated/checksum-failed validation, record-CRC
fallback, unified scans, malformed paths, marker-based selection, gap refusal,
restart/idempotency, sticky truncation, live writer exclusion, read-only
verification, report repeatability, historical conflicts, metrics, status
redaction, and HTTP routes.

Measured on the full 250-version sample in this workspace: generation about
0.7 s; scan about 0.2 s / 28 MiB RSS; 45-segment recovery verification about
0.08 s. These are far below the 2-core / 1.25 GiB / 2 GB temporary-file
constraints.

## Windows Notes

Run from `cmd.exe` or PowerShell with Python 3.11+:

```bat
python scripts\generate_archive.py --root .\archive --profile full
python app.py --root .\archive --db .\wal_archive.sqlite3 serve --port 8080
```

Paths are handled with `pathlib`; no POSIX locks or shell utilities are used.
The SQLite catalog uses WAL mode and a 30-second busy timeout.

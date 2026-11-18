"""Unified dedup, event-time windows, skew splitting and task planning.

Records are plain tuples: (vehicle_id, event_time_ms, speed_x10, fuel_ml,
distance_m). All measures are integers so aggregation sums are exact and
batch/stream comparisons are bit-stable regardless of processing order.
"""

import hashlib
import math

MS_PER_HOUR = 3600_000


def dedup_key(rec):
    """Unified dedup key for both sides: same vehicle + same event time."""
    return (rec[0], rec[1])


def content_hash(rec):
    """Deterministic tie-break so both sides keep the same twin record."""
    raw = f"{rec[0]}|{rec[1]}|{rec[2]}|{rec[3]}|{rec[4]}".encode("utf-8")
    return hashlib.sha1(raw).hexdigest()


def dedup(records):
    """Order-independent dedup. Returns (kept_records, dropped_count)."""
    best = {}
    for rec in records:
        key = dedup_key(rec)
        h = content_hash(rec)
        cur = best.get(key)
        if cur is None or h < cur[0]:
            best[key] = (h, rec)
    kept = [entry[1] for entry in best.values()]
    return kept, len(records) - len(kept)


def window_start(event_time_ms, window_ms):
    return (event_time_ms // window_ms) * window_ms


def window_closed(event_time_ms, watermark_ms, window_ms):
    """A window is closed once the watermark reaches its end."""
    return window_start(event_time_ms, window_ms) + window_ms <= watermark_ms


def aggregate(records, window_ms):
    """Partial aggregate: {window_start: [count, sum_speed, sum_fuel, sum_dist]}."""
    partials = {}
    for rec in records:
        ws = window_start(rec[1], window_ms)
        slot = partials.get(ws)
        if slot is None:
            partials[ws] = [1, rec[2], rec[3], rec[4]]
        else:
            slot[0] += 1
            slot[1] += rec[2]
            slot[2] += rec[3]
            slot[3] += rec[4]
    return partials


def split_index(rec, n_chunks):
    """Deterministic salted sub-key used to split a skewed vehicle."""
    raw = f"{rec[0]}|{rec[1]}".encode("utf-8")
    return int.from_bytes(hashlib.sha1(raw).digest()[:8], "big") % n_chunks


def build_pieces(records, skew_threshold):
    """Group records per vehicle; split skewed keys into salted chunks.

    Returns (pieces, skew_stats) where a piece is
    {"vehicle", "chunk", "records"}.
    """
    by_vehicle = {}
    for rec in records:
        by_vehicle.setdefault(rec[0], []).append(rec)
    pieces = []
    split_keys = 0
    split_chunks = 0
    for vid in sorted(by_vehicle):
        recs = by_vehicle[vid]
        if len(recs) > skew_threshold:
            n_chunks = math.ceil(len(recs) / skew_threshold)
            chunks = [[] for _ in range(n_chunks)]
            for rec in recs:
                chunks[split_index(rec, n_chunks)].append(rec)
            split_keys += 1
            split_chunks += sum(1 for c in chunks if c)
            for idx, chunk in enumerate(chunks):
                if chunk:
                    pieces.append({"vehicle": vid, "chunk": idx, "records": chunk})
        else:
            pieces.append({"vehicle": vid, "chunk": 0, "records": recs})
    return pieces, {"split_keys": split_keys, "split_chunks": split_chunks}


def assign_units(pieces, n_units):
    """LPT greedy assignment of pieces to logical processing units."""
    loads = [0] * n_units
    assignment = [[] for _ in range(n_units)]
    for piece in sorted(pieces, key=lambda p: -len(p["records"])):
        unit = min(range(n_units), key=lambda u: loads[u])
        assignment[unit].append(piece)
        loads[unit] += len(piece["records"])
    return assignment, loads


def build_tasks(tenant_id, side_records, n_units, skew_threshold):
    """Plan deterministic tasks for one tenant over both sides.

    side_records: {"batch": [...], "stream": [...]} of deduped records.
    """
    pieces = []
    skew = {"split_keys": 0, "split_chunks": 0}
    for side in ("batch", "stream"):
        side_pieces, side_skew = build_pieces(side_records[side], skew_threshold)
        skew["split_keys"] += side_skew["split_keys"]
        skew["split_chunks"] += side_skew["split_chunks"]
        for piece in side_pieces:
            piece["side"] = side
            pieces.append(piece)
    assignment, loads = assign_units(pieces, n_units)
    tasks = []
    for unit_idx, unit_pieces in enumerate(assignment):
        for piece in unit_pieces:
            task_id = (f"{tenant_id}|{piece['side']}|u{unit_idx:02d}|"
                       f"{piece['vehicle']}|c{piece['chunk']:02d}")
            tasks.append({
                "task_id": task_id,
                "tenant_id": tenant_id,
                "side": piece["side"],
                "vehicle": piece["vehicle"],
                "chunk": piece["chunk"],
                "unit": unit_idx,
                "rows": len(piece["records"]),
                "records": piece["records"],
                "attempts": 0,
            })
    tasks.sort(key=lambda t: t["task_id"])
    for idx, task in enumerate(tasks):
        task["index"] = idx
    return tasks, skew, loads

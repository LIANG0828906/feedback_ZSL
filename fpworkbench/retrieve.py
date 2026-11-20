"""Version-compatible retrieval: hash buckets + time-offset voting.

Cross-version comparison aligns both sides onto a common time quantization
baseline. If the baselines are incompatible (non-integer ratio), retrieval
aborts with E_BASELINE_MISMATCH.
"""

from __future__ import annotations

from collections import defaultdict

from .errors import E_BASELINE_MISMATCH, FpError


def common_baseline(query_quant: float, lib_quant: float) -> float:
    """Unified quantization baseline for a query/library pair."""
    if abs(query_quant - lib_quant) < 1e-9:
        return query_quant
    hi, lo = max(query_quant, lib_quant), min(query_quant, lib_quant)
    ratio = hi / lo
    if abs(ratio - round(ratio)) < 1e-9:
        return hi
    raise FpError(
        E_BASELINE_MISMATCH,
        f"incompatible time quantization baselines: query={query_quant}ms "
        f"vs library={lo}ms (non-integer ratio {ratio:.3f})",
        detail={"query_quant_ms": query_quant, "library_quant_ms": lib_quant},
    )


def build_index(entries):
    """entries: list of Fingerprint. Returns hash -> [(entry_idx, t_ms)]."""
    buckets = defaultdict(list)
    for idx, fp in enumerate(entries):
        for h, t in fp.hashes:
            buckets[h].append((idx, t))
    return buckets


def search(query, entries, index=None, min_votes=3, top_k=5):
    """Search the library with a query fingerprint.

    Returns a list of candidates:
      {sample_id, offset_ms, start_ms, end_ms, votes, confidence,
       vote_histogram, lib_version}
    """
    if index is None:
        index = build_index(entries)
    if not entries:
        return []

    quant = query.time_quant_ms
    for fp in entries:
        quant = common_baseline(quant, fp.time_quant_ms)

    votes = defaultdict(lambda: defaultdict(int))  # entry_idx -> offset -> n
    for h, tq in query.hashes:
        for idx, tl in index.get(h, ()):
            off = round(round((tl - tq) / quant) * quant, 6)
            votes[idx][off] += 1

    candidates = []
    for idx, offsets in votes.items():
        best_off, best_votes = max(offsets.items(), key=lambda kv: kv[1])
        if best_votes < min_votes:
            continue
        fp = entries[idx]
        candidates.append({
            "sample_id": fp.sample_id,
            "offset_ms": best_off,
            "start_ms": max(0.0, best_off),
            "end_ms": best_off + query.duration_ms,
            "votes": best_votes,
            "confidence": round(min(1.0, best_votes / max(1, len(query.hashes))), 6),
            "vote_histogram": {str(k): v for k, v in sorted(offsets.items())},
            "lib_version": fp.source_version,
            "quant_ms": quant,
            "total_votes": sum(offsets.values()),
        })
    candidates.sort(key=lambda c: (-c["votes"], c["sample_id"]))
    return candidates[:top_k]

"""Observable retrieval-consistency verification across migration.

Runs the same query sequence before and after migration and compares each
matched segment and offset against a declared tolerance. Every discrepancy
is located to sample, offset and version -- never just a total count.
"""

from __future__ import annotations

from .retrieve import search


def run_queries(queries, entries):
    """Return {query_id: [candidates]} for a query sequence."""
    results = {}
    for q in queries:
        results[q.sample_id] = search(q, entries)
    return results


def compare_results(before, after, tolerance_ms, before_version, after_version):
    """Per-query, per-sample comparison. Returns a report dict."""
    details = []
    n_ok = 0
    for qid in before:
        b_map = {c["sample_id"]: c for c in before[qid]}
        a_map = {c["sample_id"]: c for c in after.get(qid, [])}
        for sid in sorted(set(b_map) | set(a_map)):
            b, a = b_map.get(sid), a_map.get(sid)
            if b is None:
                details.append({
                    "query": qid, "sample": sid, "status": "MISSING_BEFORE",
                    "after_version": after_version,
                    "before_offset_ms": None,
                    "after_offset_ms": a["offset_ms"],
                })
                continue
            if a is None:
                details.append({
                    "query": qid, "sample": sid,
                    "status": "LOST_AFTER_MIGRATION",
                    "before_version": before_version,
                    "before_offset_ms": b["offset_ms"],
                    "after_offset_ms": None,
                })
                continue
            delta = abs(a["offset_ms"] - b["offset_ms"])
            status = "OK" if delta <= tolerance_ms else "OFFSET_EXCEEDS_TOLERANCE"
            if status == "OK":
                n_ok += 1
            details.append({
                "query": qid, "sample": sid, "status": status,
                "before_version": before_version,
                "after_version": after_version,
                "before_offset_ms": b["offset_ms"],
                "after_offset_ms": a["offset_ms"],
                "delta_ms": round(delta, 6),
                "tolerance_ms": tolerance_ms,
                "before_votes": b["votes"], "after_votes": a["votes"],
            })
    return {
        "tolerance_ms": tolerance_ms,
        "comparisons": len(details),
        "ok": n_ok,
        "passed": all(d["status"] == "OK" for d in details),
        "details": details,
    }

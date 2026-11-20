"""Hash-bucket retrieval, time-basis alignment and migration consistency reports."""

from __future__ import annotations

from collections import Counter, defaultdict
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from .audio import AudioData, read_wav
from .config import FingerprintConfig
from .errors import BizError, E_ALIGN_MISMATCH, E_LIB_MISMATCH
from .fingerprint import ExtractedFingerprint, HashHit, extract_fingerprint
from .library import Library, LibrarySnapshot


@dataclass(frozen=True)
class AlignedOffset:
    ok: bool
    offset: Fraction | int | None
    offset_ms: float | None
    mode: str
    detail: dict[str, Any]


def _basis_label(basis: dict[str, Any]) -> str:
    kind = basis.get("kind")
    if kind == "frame":
        return f"frame:{basis.get('sample_rate')}/{basis.get('hop_length')}"
    if kind == "ms":
        return f"ms:{basis.get('bits')}"
    return f"unknown:{kind}"


def align_offsets(
    query_basis: dict[str, Any],
    query_ticks: int,
    db_basis: dict[str, Any],
    db_ticks: int,
) -> AlignedOffset:
    query_kind = query_basis.get("kind")
    db_kind = db_basis.get("kind")
    if query_kind == db_kind == "frame":
        query_sr = query_basis.get("sample_rate")
        query_hop = query_basis.get("hop_length")
        db_sr = db_basis.get("sample_rate")
        db_hop = db_basis.get("hop_length")
        if not all(isinstance(value, int) and value > 0 for value in (query_sr, query_hop, db_sr, db_hop)):
            return AlignedOffset(
                False,
                None,
                None,
                "incompatible",
                {
                    "code": E_ALIGN_MISMATCH,
                    "reason": "frame basis lacks sample_rate or hop_length",
                    "query_basis": _basis_label(query_basis),
                    "db_basis": _basis_label(db_basis),
                },
            )
        if query_hop * db_sr != db_hop * query_sr:
            return AlignedOffset(
                False,
                None,
                None,
                "incompatible",
                {
                    "code": E_ALIGN_MISMATCH,
                    "reason": "frame durations differ",
                    "query_basis": _basis_label(query_basis),
                    "db_basis": _basis_label(db_basis),
                },
            )
        query_offset = Fraction(query_ticks * query_hop * 1000, query_sr)
        db_offset = Fraction(db_ticks * db_hop * 1000, db_sr)
        return AlignedOffset(
            True,
            db_offset - query_offset,
            float(db_offset - query_offset),
            "frame_exact",
            {
                "query_basis": _basis_label(query_basis),
                "db_basis": _basis_label(db_basis),
                "common_basis": _basis_label(query_basis),
            },
        )
    if query_kind == db_kind == "ms":
        query_bits = query_basis.get("bits")
        db_bits = db_basis.get("bits")
        if not isinstance(query_bits, int) or not isinstance(db_bits, int):
            return AlignedOffset(
                False,
                None,
                None,
                "incompatible",
                {
                    "code": E_ALIGN_MISMATCH,
                    "reason": "millisecond basis lacks quantization bits",
                    "query_basis": _basis_label(query_basis),
                    "db_basis": _basis_label(db_basis),
                },
            )
        common_bits = max(query_bits, db_bits)
        quantum = 1 << common_bits
        query_ms = query_ticks * (1 << query_bits)
        db_ms = db_ticks * (1 << db_bits)
        query_aligned = (query_ms // quantum) * quantum
        db_aligned = (db_ms // quantum) * quantum
        mode = "ms_exact" if query_bits == db_bits else f"ms_coarsened:{common_bits}"
        return AlignedOffset(
            True,
            db_aligned - query_aligned,
            float(db_aligned - query_aligned),
            mode,
            {
                "query_basis": _basis_label(query_basis),
                "db_basis": _basis_label(db_basis),
                "common_basis": f"ms:{common_bits}",
            },
        )
    return AlignedOffset(
        False,
        None,
        None,
        "incompatible",
        {
            "code": E_ALIGN_MISMATCH,
            "reason": "frame and millisecond bases cannot be compared without migration",
            "query_basis": _basis_label(query_basis),
            "db_basis": _basis_label(db_basis),
        },
    )


def _coerce_query(
    query: ExtractedFingerprint | AudioData | str | Path,
    config: FingerprintConfig | None,
    track_id: str | None,
) -> ExtractedFingerprint:
    if isinstance(query, ExtractedFingerprint):
        return query
    if config is None:
        raise BizError(E_ALIGN_MISMATCH, "query WAV requires an explicit fingerprint config", {"field": "config"})
    if isinstance(query, (str, Path)):
        audio: AudioData | str = str(query)
    else:
        audio = query
    return extract_fingerprint(audio, config, track_id=track_id)


def _fraction_key(value: Fraction | int) -> tuple[int, int]:
    fraction = value if isinstance(value, Fraction) else Fraction(value)
    return fraction.numerator, fraction.denominator


def search_library(
    library: Library,
    query: ExtractedFingerprint | AudioData | str | Path,
    *,
    config: FingerprintConfig | None = None,
    track_id: str | None = None,
    top_k: int = 5,
    expected_version_hash: str | None = None,
) -> dict[str, Any]:
    query_fp = _coerce_query(query, config, track_id)
    snapshot = library.open_snapshot()
    result: dict[str, Any] | None = None
    try:
        if expected_version_hash is not None and expected_version_hash != snapshot.library_version_hash:
            raise BizError(
                E_LIB_MISMATCH,
                "caller expected a different published library version hash",
                {
                    "expected": expected_version_hash,
                    "actual": snapshot.library_version_hash,
                    "field": "library_version_hash",
                },
            )
        bucket: dict[str, list[tuple[dict[str, Any], HashHit]]] = defaultdict(list)
        track_hash_counts: Counter[tuple[str, int, str]] = Counter()
        for entry, record in zip(snapshot.entries, snapshot.records):
            track_key = (record.track_id, record.version, entry["config_hash"])
            track_hash_counts[track_key] += len(record.hashes)
            for hit in record.hashes:
                bucket[hit.hash].append((entry, hit))

        groups: dict[tuple[str, int, str], dict[str, Any]] = {}
        alignment_errors: dict[tuple[str, int, str, str], dict[str, Any]] = {}
        for query_hit in query_fp.all_hashes:
            for entry, db_hit in bucket.get(query_hit.hash, []):
                record_version = int(entry["fingerprint_version"])
                track_key = (entry["track_id"], record_version, entry["config_hash"])
                aligned = align_offsets(
                    query_fp.time_basis,
                    query_hit.offset_ticks,
                    entry["time_basis"],
                    db_hit.offset_ticks,
                )
                if not aligned.ok:
                    detail = aligned.detail
                    error_key = (
                        entry["track_id"],
                        int(entry["segment_index"]),
                        str(detail.get("query_basis")),
                        str(detail.get("db_basis")),
                    )
                    alignment_errors.setdefault(
                        error_key,
                        {
                            "code": E_ALIGN_MISMATCH,
                            "sample": entry["track_id"],
                            "segment_index": int(entry["segment_index"]),
                            "fingerprint_version": record_version,
                            "query_basis": detail.get("query_basis"),
                            "db_basis": detail.get("db_basis"),
                            "reason": detail.get("reason"),
                            "excluded": True,
                        },
                    )
                    continue
                group = groups.setdefault(
                    track_key,
                    {
                        "votes": Counter(),
                        "hashes": set(),
                        "segments": set(),
                        "entries": {},
                        "alignment_modes": set(),
                    },
                )
                delta_key = _fraction_key(aligned.offset)  # type: ignore[arg-type]
                group["votes"][delta_key] += 1
                group["hashes"].add(query_hit.hash)
                group["segments"].add(int(entry["segment_index"]))
                group["entries"][entry["key"]] = entry
                group["alignment_modes"].add(aligned.mode)

        candidates: list[dict[str, Any]] = []
        for track_key, group in groups.items():
            if not group["votes"]:
                continue
            best_delta, votes = sorted(
                group["votes"].items(),
                key=lambda item: (-item[1], abs(Fraction(*item[0])), Fraction(*item[0])),
            )[0]
            delta = Fraction(*best_delta)
            delta_ms = float(delta)
            best_entry = sorted(group["entries"].values(), key=lambda entry: entry["key"])[0]
            track_hash_count = track_hash_counts[track_key]
            confidence = votes / max(1, min(len(query_fp.all_hashes), track_hash_count))
            start_ms = max(0, int(round(delta_ms)))
            end_ms = start_ms + query_fp.duration_ms
            candidates.append(
                {
                    "sample": track_key[0],
                    "fingerprint_version": track_key[1],
                    "config_hash": track_key[2],
                    "match_start_ms": start_ms,
                    "match_end_ms": end_ms,
                    "offset_ms": round(delta_ms, 6),
                    "votes": votes,
                    "confidence": round(min(1.0, confidence), 6),
                    "segments": sorted(group["segments"]),
                    "alignment": sorted(group["alignment_modes"]),
                    "time_basis": best_entry["time_basis"],
                    "match_hashes": sorted(group["hashes"]),
                    "source_segment": best_entry["key"],
                }
            )
        candidates.sort(key=lambda item: (-item["confidence"], -item["votes"], item["sample"]))
        candidates = candidates[:top_k]
        result = {
            "ok": True,
            "query_duration_ms": query_fp.duration_ms,
            "query_hash_count": len(query_fp.all_hashes),
            "query_time_basis": query_fp.time_basis,
            "library_version_hash": snapshot.library_version_hash,
            "candidates": candidates,
            "alignment_errors": sorted(
                alignment_errors.values(),
                key=lambda item: (item["sample"], item["segment_index"], str(item["query_basis"]), str(item["db_basis"])),
            ),
            "snapshot_released": False,
        }
        return result
    finally:
        snapshot.close()
        if result is not None:
            result["snapshot_released"] = True


def _candidate_map(result: dict[str, Any]) -> dict[str, dict[str, Any]]:
    mapping: dict[str, dict[str, Any]] = {}
    for candidate in result.get("candidates", []):
        mapping.setdefault(candidate["sample"], candidate)
    return mapping


def _candidate_summary(candidate: dict[str, Any] | None) -> dict[str, Any] | None:
    if candidate is None:
        return None
    return {
        "library_version_hash": None,
        "sample": candidate["sample"],
        "fingerprint_version": candidate["fingerprint_version"],
        "match_start_ms": candidate["match_start_ms"],
        "match_end_ms": candidate["match_end_ms"],
        "offset_ms": candidate["offset_ms"],
        "confidence": candidate["confidence"],
        "match_hashes": candidate["match_hashes"][:8],
        "hash_count": len(candidate["match_hashes"]),
    }


def compare_query_sequences(
    before_results: list[dict[str, Any]],
    after_results: list[dict[str, Any]],
    *,
    tolerance_ms: int,
    query_names: Iterable[str] | None = None,
) -> dict[str, Any]:
    names = list(query_names or [f"query-{index + 1}" for index in range(max(len(before_results), len(after_results)))])
    rows: list[dict[str, Any]] = []
    differences: list[dict[str, Any]] = []
    for index in range(max(len(before_results), len(after_results))):
        query_name = names[index] if index < len(names) else f"query-{index + 1}"
        before = before_results[index] if index < len(before_results) else {"candidates": [], "library_version_hash": None}
        after = after_results[index] if index < len(after_results) else {"candidates": [], "library_version_hash": None}
        before_map = _candidate_map(before)
        after_map = _candidate_map(after)
        for sample in sorted(set(before_map).union(after_map)):
            pre_candidate = before_map.get(sample)
            post_candidate = after_map.get(sample)
            pre = _candidate_summary(pre_candidate)
            post = _candidate_summary(post_candidate)
            if pre is not None:
                pre["library_version_hash"] = before.get("library_version_hash")
            if post is not None:
                post["library_version_hash"] = after.get("library_version_hash")
            status = "ok"
            reason = None
            offset_delta = None
            hash_intersection: list[str] = []
            if pre is None or post is None:
                status = "missing_match"
                reason = "missing_before" if pre is None else "missing_after"
            else:
                offset_delta = post["offset_ms"] - pre["offset_ms"]
                hash_intersection = sorted(set(pre["match_hashes"]).intersection(post["match_hashes"]))
                if not hash_intersection:
                    status = "hash_mismatch"
                    reason = "no_shared_matched_hash"
                elif abs(offset_delta) > tolerance_ms:
                    status = "time_drift"
                    reason = "offset_exceeds_tolerance"
            row = {
                "query": query_name,
                "sample": sample,
                "status": status,
                "reason": reason,
                "tolerance_ms": tolerance_ms,
                "offset_delta_ms": offset_delta,
                "hash_intersection": hash_intersection[:8],
                "before": pre,
                "after": post,
            }
            rows.append(row)
            if status != "ok":
                differences.append(row)
    return {
        "ok": not differences,
        "tolerance_ms": tolerance_ms,
        "queries_compared": len(names),
        "row_count": len(rows),
        "difference_count": len(differences),
        "differences": differences,
        "rows": rows,
    }

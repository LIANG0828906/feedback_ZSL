"""Hash-bucket + time-offset voting retrieval against a published library."""

from collections import defaultdict

from .errors import AlignMismatch


def build_index(library):
    """hash -> list of (sample_id, t_offset); deterministic build order."""
    index = defaultdict(list)
    for entry, rec in library.iter_records():
        for h, t, band in rec["records"]:
            index[h].append((entry["sample_id"], t))
    return index


def search(library, query_records, query_id, query_rate, query_n_frames,
           fp_cfg, search_cfg):
    """Vote on (sample_id, quantized offset); return candidates + version.

    The query is bound to one fully published library version; the version
    hash is echoed back in the result. A quantization-base mismatch between
    query and library rejects the alignment with E_ALIGN_MISMATCH. Results
    are pure functions of library state, so any parallelism yields the same
    answer.
    """
    manifest = library.verify()  # E_LIB_MISMATCH on inconsistency
    quant = fp_cfg["time_quant"]
    if manifest["time_quant"] is not None and quant != manifest["time_quant"]:
        raise AlignMismatch(
            "query time-quant base does not match library",
            query_quant=quant, lib_quant=manifest["time_quant"],
        )
    index = build_index(library)
    votes = defaultdict(int)
    for h, tq, _band in query_records:
        for sid, tl in index.get(h, ()):
            delta = tl - tq
            qdelta = int(round(delta / quant))
            votes[(sid, qdelta)] += 1
    hop = fp_cfg["hop_size"]
    min_votes = search_cfg["min_votes"]
    n_query = max(1, len(query_records))
    candidates = []
    for (sid, qd), v in votes.items():
        if v < min_votes:
            continue
        start_frame = qd * quant
        start_sec = round(start_frame * hop / query_rate, 6)
        end_sec = round((start_frame + query_n_frames) * hop / query_rate, 6)
        candidates.append({
            "sample_id": sid,
            "start_frame": start_frame,
            "start_sec": start_sec,
            "end_sec": end_sec,
            "votes": v,
            "confidence": round(v / n_query, 6),
        })
    candidates.sort(key=lambda c: (-c["votes"], c["sample_id"], c["start_frame"]))
    return {
        "query_id": query_id,
        "lib_version": manifest["version_hash"],
        "config_version": manifest["config_version"],
        "time_quant": quant,
        "candidates": candidates,
    }

"""Cross-run consistency verification: per-query, per-candidate diffs."""

import json


def compare_query_results(results_a, results_b, tolerance_frames, label_a, label_b):
    """Compare two query-result sequences candidate by candidate.

    Returns (ok, diff_lines). Every mismatch is located by query id, sample
    id, offset and config version -- never just an aggregate statistic.
    """
    lines = []
    ok = True
    for ra, rb in zip(results_a, results_b):
        qid = ra["query_id"]
        ca = {c["sample_id"]: c for c in ra["candidates"]}
        cb = {c["sample_id"]: c for c in rb["candidates"]}
        for sid in sorted(set(ca) | set(cb)):
            if sid not in ca:
                ok = False
                lines.append(
                    f"DIFF query={qid} sample={sid}: missing in {label_a} "
                    f"(present in {label_b} start_frame={cb[sid]['start_frame']} "
                    f"config={rb['config_version']})")
            elif sid not in cb:
                ok = False
                lines.append(
                    f"DIFF query={qid} sample={sid}: missing in {label_b} "
                    f"(present in {label_a} start_frame={ca[sid]['start_frame']} "
                    f"config={ra['config_version']})")
            else:
                d = abs(ca[sid]["start_frame"] - cb[sid]["start_frame"])
                if d > tolerance_frames:
                    ok = False
                    lines.append(
                        f"DIFF query={qid} sample={sid}: offset drift {d} frames "
                        f"> tolerance {tolerance_frames} "
                        f"({label_a}={ca[sid]['start_frame']}@{ra['config_version']} "
                        f"{label_b}={cb[sid]['start_frame']}@{rb['config_version']})")
                else:
                    lines.append(
                        f"MATCH query={qid} sample={sid}: offset diff {d} frames "
                        f"within tolerance {tolerance_frames}")
        if ra["lib_version"] != rb["lib_version"]:
            lines.append(
                f"NOTE query={qid}: lib versions differ "
                f"({label_a}={ra['lib_version'][:12]} "
                f"{label_b}={rb['lib_version'][:12]})")
    return ok, lines


def compare_fp_hashes(hashes_a, hashes_b, label_a, label_b):
    """Per-sample fingerprint hash comparison. Returns (ok, diff_lines)."""
    lines = []
    ok = True
    for sid in sorted(set(hashes_a) | set(hashes_b)):
        ha, hb = hashes_a.get(sid), hashes_b.get(sid)
        if ha != hb:
            ok = False
            lines.append(f"DIFF sample={sid}: fp_hash {label_a}={ha} "
                         f"{label_b}={hb}")
    return ok, lines


def write_report(path, sections):
    import os
    os.makedirs(os.path.dirname(str(path)), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        for title, lines in sections:
            fh.write(f"== {title} ==\n")
            for line in lines:
                fh.write(line + "\n")
            fh.write("\n")


def load_json(path):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)

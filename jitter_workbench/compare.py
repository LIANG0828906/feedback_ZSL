"""Three-path consistency comparison.

Locates the first inconsistent sample / frame / decision and raises
E_DIFF with a surrounding context window when the paths diverge.
"""
from __future__ import annotations

import numpy as np

from .engine import RunResult
from .errors import DiffError

CONTEXT = 8


def _sample_context(arr, idx, width=CONTEXT):
    lo = max(0, idx - width)
    hi = min(arr.shape[0], idx + width + 1)
    return {"offset": lo, "values": [round(float(v), 6) for v in arr[lo:hi]]}


def compare_results(name_a: str, a: RunResult,
                    name_b: str, b: RunResult) -> dict:
    """Raise DiffError (E_DIFF) on the first divergence found."""
    # 1. dequeued sample stream
    if a.output.shape != b.output.shape:
        raise DiffError(
            "%s vs %s: output length %d != %d"
            % (name_a, name_b, a.output.shape[0], b.output.shape[0]),
            field="output",
            detail={"len_a": a.output.shape[0], "len_b": b.output.shape[0]})
    diff = np.nonzero(a.output != b.output)[0]
    if diff.size:
        idx = int(diff[0])
        raise DiffError(
            "%s vs %s: first sample mismatch at index %d"
            % (name_a, name_b, idx),
            field="output",
            detail={
                "sample_index": idx,
                "context_a": _sample_context(a.output, idx),
                "context_b": _sample_context(b.output, idx),
            })
    # 2. per-frame energy statistics
    ediff = np.nonzero(a.energy != b.energy)[0]
    if ediff.size:
        fidx = int(ediff[0])
        raise DiffError(
            "%s vs %s: first energy mismatch at frame %d"
            % (name_a, name_b, fidx),
            field="energy",
            detail={
                "frame_index": fidx,
                "context_a": _sample_context(a.energy, fidx),
                "context_b": _sample_context(b.energy, fidx),
            })
    # 3. target-delay decision sequence
    ka = [d.key() for d in a.decisions]
    kb = [d.key() for d in b.decisions]
    if ka != kb:
        didx = next((i for i, (x, y) in enumerate(zip(ka, kb)) if x != y),
                    min(len(ka), len(kb)))
        lo = max(0, didx - 2)
        raise DiffError(
            "%s vs %s: first decision mismatch at decision #%d"
            % (name_a, name_b, didx),
            field="decisions",
            detail={
                "decision_index": didx,
                "context_a": [repr(k) for k in ka[lo:didx + 3]],
                "context_b": [repr(k) for k in kb[lo:didx + 3]],
            })
    # 4. gap flags
    if a.gap_flags != b.gap_flags:
        gidx = next(i for i, (x, y) in enumerate(zip(a.gap_flags,
                                                     b.gap_flags)) if x != y)
        raise DiffError(
            "%s vs %s: first gap-flag mismatch at window %d"
            % (name_a, name_b, gidx),
            field="gap_flags",
            detail={"window": gidx})
    return {
        "match": True,
        "paths": [name_a, name_b],
        "samples": int(a.output.shape[0]),
        "frames": int(a.energy.shape[0]),
        "decisions": len(a.decisions),
    }


def compare_three(realtime: RunResult, resume: RunResult,
                  offline: RunResult) -> dict:
    r1 = compare_results("realtime", realtime, "offline", offline)
    r2 = compare_results("realtime", realtime, "resume", resume)
    return {"match": True, "pairs": [r1, r2]}

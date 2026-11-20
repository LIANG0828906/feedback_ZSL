"""Baseline loading and per-step divergence diagnosis."""

from __future__ import annotations

import json
import os

from .errors import BaselineCorruptError, BaselineDivergedError, \
    BaselineMissingError, BaselineVersionError
from .model import MODEL_VERSION

COMPARE_FIELDS = ("state_hash", "chain_hash")


def load_baseline(path: str):
    if not os.path.exists(path):
        raise BaselineMissingError("baseline file missing", path=path)
    with open(path, "r", encoding="utf-8") as fh:
        lines = [ln for ln in fh.read().splitlines() if ln.strip()]
    if not lines:
        raise BaselineMissingError("baseline file empty", path=path)
    try:
        header = json.loads(lines[0])
    except json.JSONDecodeError as exc:
        raise BaselineCorruptError("baseline header malformed",
                                   line=1, detail=str(exc)) from exc
    if header.get("version") != MODEL_VERSION:
        raise BaselineVersionError("baseline model version mismatch",
                                   found=header.get("version"),
                                   expected=MODEL_VERSION)
    entries = []
    for lineno, raw in enumerate(lines[1:], start=2):
        try:
            entries.append(json.loads(raw))
        except json.JSONDecodeError as exc:
            raise BaselineCorruptError("baseline line malformed",
                                       line=lineno, detail=str(exc)) from exc
    return header, entries


def compare(chain: list, baseline_path: str) -> dict:
    """Compare local chain entries against the baseline, step by step.

    Raises a BizError when the baseline is unusable (never partially
    compares) and E_BASELINE_DIVERGED with the first mismatch location.
    """
    header, entries = load_baseline(baseline_path)
    if len(entries) < len(chain):
        raise BaselineMissingError(
            "baseline has fewer steps than local run",
            baseline_steps=len(entries), local_steps=len(chain))
    for local, base in zip(chain, entries):
        step = local["step"]
        if base.get("step") != step:
            raise BaselineCorruptError("baseline step sequence broken",
                                       expected_step=step,
                                       found=base.get("step"))
        for field_name in COMPARE_FIELDS:
            if local[field_name] != base.get(field_name):
                raise BaselineDivergedError(
                    "first divergence located",
                    step=step,
                    field_path=f"chain[{step}].{field_name}",
                    local=local[field_name],
                    baseline=base.get(field_name))
    return {"status": "match", "steps_compared": len(chain),
            "baseline_steps": len(entries),
            "model_version": header.get("version")}

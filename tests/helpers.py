"""Shared fixtures for the opera-master test suite."""

import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

from opera_master.cli import build_demo_cue  # noqa: E402
from opera_master.cue import load_cue  # noqa: E402
from opera_master.synthesis import generate_channels  # noqa: E402


_WORKSPACE_CACHE = {}


def make_workspace(seed=20261118, block_samples=24000, shared=False):
    key = (seed, block_samples)
    if shared and key in _WORKSPACE_CACHE:
        return _WORKSPACE_CACHE[key]
    tmp = tempfile.mkdtemp(prefix="opera_test_")
    cue_raw = build_demo_cue(tmp, seed=seed)
    cue_raw["block_samples"] = block_samples
    cue_path = os.path.join(tmp, "cue.json")
    with open(cue_path, "w", encoding="utf-8") as fh:
        json.dump(cue_raw, fh, ensure_ascii=False, indent=2)
    cue = load_cue(cue_path)
    generate_channels(cue, base_dir=tmp)
    if shared:
        _WORKSPACE_CACHE[key] = (tmp, cue_path, cue)
    return tmp, cue_path, cue


def write_cue(cue_path, payload):
    with open(cue_path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2)

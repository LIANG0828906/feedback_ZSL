"""Error-code链路: E_META / E_JITTER / E_DECODE / E_ORDER / E_DUP."""
import json
import os
import shutil

import pytest

from jitter_workbench.errors import (DecodeError, DupError, JitterError,
                                     MetaError, OrderError)
from jitter_workbench.loader import load_sample


def _mutate(sample_dir, tmp_path, fn):
    d = str(tmp_path / "mut")
    shutil.copytree(sample_dir, d)
    mpath = os.path.join(d, "manifest.json")
    with open(mpath) as fh:
        m = json.load(fh)
    fn(m, d)
    with open(mpath, "w") as fh:
        json.dump(m, fh)
    return d


def test_ok(sample_dir):
    s = load_sample(sample_dir)
    assert s.num_frames == 300
    assert s.duration_samples == (300 - 1) * 160 + 320


def test_e_meta_missing_field(sample_dir, tmp_path):
    d = _mutate(sample_dir, tmp_path, lambda m, _: m.pop("sample_rate"))
    with pytest.raises(MetaError) as ei:
        load_sample(d)
    assert ei.value.code == "E_META"
    assert ei.value.field == "sample_rate"


def test_e_meta_sample_rate_mismatch(sample_dir, tmp_path):
    def fn(m, d):
        m["sample_rate"] = 8000
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(MetaError) as ei:
        load_sample(d)
    assert ei.value.field == "sample_rate"


def test_e_meta_non_monotonic_media_ts(sample_dir, tmp_path):
    def fn(m, _):
        m["frames"][5]["media_ts"] = m["frames"][4]["media_ts"] - 10
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(MetaError) as ei:
        load_sample(d)
    assert ei.value.field == "frames.media_ts"


def test_e_meta_frame_len_mismatch(sample_dir, tmp_path):
    def fn(m, _):
        m["frames"][3]["n_samples"] = 999
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(MetaError) as ei:
        load_sample(d)
    assert ei.value.field == "frames.n_samples"


def test_e_meta_negative_arrival(sample_dir, tmp_path):
    def fn(m, _):
        m["arrivals"][0]["arrival_ts"] = -5
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(MetaError) as ei:
        load_sample(d)
    assert ei.value.field == "arrivals.arrival_ts"


def test_e_jitter_unknown_pattern(sample_dir, tmp_path):
    def fn(m, _):
        m["jitter_patterns"].append("teleport")
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(JitterError) as ei:
        load_sample(d)
    assert ei.value.code == "E_JITTER"
    assert "teleport" in str(ei.value)


def test_e_jitter_unknown_frame_tag(sample_dir, tmp_path):
    def fn(m, _):
        m["frames"][7]["tags"].append("wormhole")
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(JitterError):
        load_sample(d)


def test_e_decode_truncated_frames(sample_dir, tmp_path):
    d = _mutate(sample_dir, tmp_path, lambda m, _: None)
    fpath = os.path.join(d, "frames.bin")
    with open(fpath, "r+b") as fh:
        fh.truncate(os.path.getsize(fpath) - 100)
    with pytest.raises(DecodeError) as ei:
        load_sample(d)
    assert ei.value.code == "E_DECODE"


def test_e_order_undeclared_inversion(sample_dir, tmp_path):
    def fn(m, _):
        # swap two arrivals' order without any reorder tag
        for e in m["frames"]:
            e["tags"] = [t for t in e["tags"] if t != "reorder"]
        a = m["arrivals"][10]
        b = m["arrivals"][11]
        a["arrival_ts"], b["arrival_ts"] = b["arrival_ts"], a["arrival_ts"]
        m["arrivals"][10], m["arrivals"][11] = b, a
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(OrderError) as ei:
        load_sample(d)
    assert ei.value.code == "E_ORDER"


def test_e_dup_beyond_tolerance(sample_dir, tmp_path):
    def fn(m, _):
        base = dict(m["arrivals"][0])
        dups = []
        for i in range(30):
            dup = dict(base)
            dup["arrival_ts"] = base["arrival_ts"] + 1 + i
            dup["dup"] = True
            dups.append(dup)
        m["arrivals"][1:1] = dups
    d = _mutate(sample_dir, tmp_path, fn)
    with pytest.raises(DupError) as ei:
        load_sample(d)
    assert ei.value.code == "E_DUP"

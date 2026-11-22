import json
import os
import sqlite3
import subprocess
import sys
import time

import pytest

from caption_pipeline.checkpoint import CheckpointStore
from caption_pipeline.engine import run_project
from caption_pipeline.errors import E_FIT, PipelineError
from tests.conftest import load_manifest, write_manifest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def _records(persist_dir):
    store = CheckpointStore(persist_dir)
    try:
        _, recs = store.inspect()
    finally:
        store.close()
    return [(r["line_id"], r["win_start"], r["win_end"], r["screens"],
             r["tradeoffs"], r["state"]) for r in recs]


def test_parallel_equals_serial(manifest_path, tmp_path):
    serial = run_project(manifest_path, str(tmp_path / "s"), workers=1)
    par = run_project(manifest_path, str(tmp_path / "p"), workers=8)
    assert serial["committed"] == serial["total"] == par["committed"]
    ka = {k: v.key() for k, v in serial["results"].items()}
    kb = {k: v.key() for k, v in par["results"].items()}
    assert ka == kb
    # 取舍记录逐句一致
    for lid in ka:
        assert serial["results"][lid].tradeoffs == par["results"][lid].tradeoffs


def test_resume_from_checkpoint(manifest_path, tmp_path):
    persist = str(tmp_path / "ckpt")
    part = run_project(manifest_path, persist, workers=4, max_commits=3)
    assert part["committed"] == 3
    full = run_project(manifest_path, persist, workers=4)
    assert full["committed"] == full["total"]
    ref = run_project(manifest_path, str(tmp_path / "ref"), workers=1)
    assert {k: v.key() for k, v in full["results"].items()} == \
           {k: v.key() for k, v in ref["results"].items()}


def test_version_mismatch_invalidates_snapshot(manifest_path, tmp_path):
    persist = str(tmp_path / "ckpt")
    run_project(manifest_path, persist, workers=2, max_commits=5)
    assert len([r for r in _records(persist) if r[5] == "COMMITTED"]) == 5
    # 修改参数版本 -> 快照整体作废并全量重算
    data = load_manifest(manifest_path)
    data["presentation"]["param_version"] = "pv2"
    path2 = write_manifest(str(tmp_path / "proj2"), data,
                           audio_from=manifest_path.replace(
                               "manifest.json", data["audio_file"]))
    result = run_project(path2, persist, workers=2)
    assert result["committed"] == result["total"]


def test_kill9_recovery(manifest_path, tmp_path):
    persist = str(tmp_path / "kill")
    env = dict(os.environ)
    proc = subprocess.Popen(
        [sys.executable, os.path.join(ROOT, "main.py"), "run",
         "--project", manifest_path, "--persist", persist,
         "--workers", "4", "--commit-delay", "0.4"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env)
    time.sleep(1.2)
    proc.kill()
    proc.wait()
    partial = [r for r in _records(persist) if r[5] == "COMMITTED"]
    assert 0 < len(partial) < 8  # 只保留已持久化的台词
    full = run_project(manifest_path, persist, workers=4)
    assert full["committed"] == full["total"]
    ref = run_project(manifest_path, str(tmp_path / "ref"), workers=1)
    assert {k: v.key() for k, v in full["results"].items()} == \
           {k: v.key() for k, v in ref["results"].items()}


def test_budget_blocks_but_never_drops(manifest_path, tmp_path):
    # 极小预算：任务只能逐个推进，最终仍全部完成且结果一致。
    tight = {"max_text_bytes": 256, "max_sample_bytes": 1 << 19}
    res = run_project(manifest_path, str(tmp_path / "tight"), workers=4,
                      budget_overrides=tight)
    assert res["committed"] == res["total"]
    ref = run_project(manifest_path, str(tmp_path / "ref"), workers=1)
    assert {k: v.key() for k, v in res["results"].items()} == \
           {k: v.key() for k, v in ref["results"].items()}


def test_fit_unresolvable_returns_e_fit(tmp_path, manifest_path):
    # 注入一句超长文本 + 极低语速上限 + 无空隙 -> 冲突不可收敛。
    data = load_manifest(manifest_path)
    data["subtitles"][0]["text"] = "长" * 60
    data["subtitles"][0]["max_cps"] = 0.5
    data["segments"][1]["start"] = data["segments"][0]["end"]  # 无空隙
    path = write_manifest(str(tmp_path / "proj_efit"), data,
                          audio_from=manifest_path.replace(
                              "manifest.json", data["audio_file"]))
    with pytest.raises(PipelineError) as ei:
        run_project(path, str(tmp_path / "ckpt_efit"), workers=2)
    assert ei.value.code == E_FIT
    lines = ei.value.details["lines"]
    assert lines and lines[0]["line_id"] == "L001"
    assert lines[0]["deficit_samples"] > 0

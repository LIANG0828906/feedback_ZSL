from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from minute_backfill.util import sha256_file

from minute_backfill.engine import BackfillEngine, EngineConfig
from minute_backfill.errors import EngineError
from minute_backfill.generator import GeneratorConfig, generate_dataset
from .helpers import ROOT, run_cli


def _data(tmp_path: Path, batches: int = 6) -> Path:
    config = GeneratorConfig(
        out_dir=tmp_path / "data",
        seed=41,
        payment_rows=250,
        refund_rows=150,
        batches=batches,
        minutes=max(batches, 10),
        delay_rate=0.1,
        out_of_order_rate=0.2,
        force=True,
    )
    generate_dataset(config)
    return config.out_dir


def test_bad_batch_returns_e_input_with_batch_id(tmp_path: Path) -> None:
    data = _data(tmp_path)
    bad = data / "refunds" / "batch-000003.jsonl"
    original = bad.read_text()
    bad.write_text(original + '{"broken":\n', encoding="utf-8")
    manifest_path = data / "refunds" / "batches.json"
    manifest = json.loads(manifest_path.read_text())
    for batch in manifest["batches"]:
        if batch["batch_id"] == "batch-000003":
            batch["sha256"] = sha256_file(bad)
            batch["rows"] = len(bad.read_text().splitlines())
    manifest_path.write_text(json.dumps(manifest))
    out = tmp_path / "bad-out"
    result = run_cli(
        "run",
        "--data", str(data),
        "--state", str(tmp_path / "bad.db"),
        "--out", str(out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
        "--reset-state",
    )
    assert result.returncode == 2
    assert "E_INPUT" in result.stderr
    assert "batch-000003" in result.stderr
    error = json.loads((out / "last_error.json").read_text())
    assert error["error"]["code"] == "E_INPUT"
    assert error["error"]["subject"] == "batch-000003"
    partial = json.loads((out / "report.partial.json").read_text())
    assert partial["status"] == "failed"
    assert partial["processing"]["consumed_batches"] == 2


def test_exclusive_lock_returns_e_state(tmp_path: Path) -> None:
    data = _data(tmp_path, batches=3)
    state = tmp_path / "locked.db"
    out = tmp_path / "locked-out"
    first = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
        "--reset-state",
    )
    assert first.returncode == 0, first.stderr
    code = (
        "import sys,time;"
        "from minute_backfill.state import FileLock;"
        "lock=FileLock(__import__('pathlib').Path(sys.argv[1]));"
        "lock.acquire();"
        "print('ready', flush=True);"
        "time.sleep(60)"
    )
    env = dict(os.environ)
    env["PYTHONPATH"] = str(ROOT) + (
        os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else ""
    )
    holder = subprocess.Popen(
        [sys.executable, "-c", code, str(state) + ".lock"],
        cwd=str(ROOT),
        env=env,
        stdout=subprocess.PIPE,
        text=True,
    )
    try:
        assert holder.stdout is not None
        assert holder.stdout.readline().strip() == "ready"
        with pytest.raises(EngineError) as exc:
            BackfillEngine(
                EngineConfig(
                    data_dir=data,
                    state_db=state,
                    out_dir=tmp_path / "locked-out-2",
                    workers=0,
                )
            )
        assert exc.value.code == "E_STATE"
    finally:
        holder.terminate()
        holder.wait(timeout=10)
    resumed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
    )
    assert resumed.returncode == 0, resumed.stderr


def test_corrupt_state_is_quarantined_and_poisoned(tmp_path: Path) -> None:
    data = _data(tmp_path, batches=2)
    state = tmp_path / "corrupt.db"
    out = tmp_path / "corrupt-out"
    first = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
        "--reset-state",
    )
    assert first.returncode == 0, first.stderr
    state.write_bytes(b"this is definitely not a sqlite database")
    failed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
    )
    assert failed.returncode == 3
    assert "E_STATE" in failed.stderr
    assert Path(str(state) + ".poisoned").exists()
    assert list(tmp_path.glob("corrupt.db.corrupt.*"))
    second = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
    )
    assert second.returncode == 3
    assert "quarantined" in second.stderr

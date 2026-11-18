from __future__ import annotations

import json
from pathlib import Path

from minute_backfill.generator import GeneratorConfig, generate_dataset

from .helpers import business_signature, run_engine, run_cli


def _data(tmp_path: Path) -> Path:
    config = GeneratorConfig(
        out_dir=tmp_path / "data",
        seed=61,
        payment_rows=600,
        refund_rows=340,
        batches=14,
        minutes=18,
        duplicate_rate=0.06,
        delay_rate=0.1,
        out_of_order_rate=0.25,
        force=True,
    )
    generate_dataset(config)
    return config.out_dir


def test_same_input_is_byte_stable_for_business_fields(tmp_path: Path) -> None:
    data = _data(tmp_path)
    first, _, first_out = run_engine(tmp_path, data, "det-a", workers=0, lateness=60)
    second, _, _ = run_engine(tmp_path, data, "det-b", workers=2, lateness=60)
    assert first["business_digest"] == second["business_digest"]
    assert business_signature(first) == business_signature(second)

    third_state = tmp_path / "det-c.db"
    third_out = tmp_path / "det-c-out"
    for _ in range(2):
        result = run_cli(
            "run",
            "--data", str(data),
            "--state", str(third_state),
            "--out", str(third_out),
            "--workers", "0",
            "--allowed-lateness-seconds", "60",
            *(["--reset-state"] if not third_state.exists() else []),
        )
        assert result.returncode == 0, result.stderr
    repeated = json.loads((third_out / "report.json").read_text())
    assert repeated["business_digest"] == first["business_digest"]
    assert repeated["recovery"]["count"] == 1

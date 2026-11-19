from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

from conftest import ROOT


def _key_structure(value: Any) -> Any:
    if isinstance(value, dict):
        return {key: _key_structure(item) for key, item in sorted(value.items())}
    if isinstance(value, list):
        return ["list"]
    return "value"


def _run_scenario(
    service,
    scenario_path: Path,
    output_dir: Path,
) -> dict[str, Any]:
    completed = subprocess.run(
        [
            sys.executable,
            str(ROOT / "scripts" / "run_scenario.py"),
            str(scenario_path),
            "--no-start-service",
            "--base-url",
            service.base_url,
            "--output-dir",
            str(output_dir),
        ],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert completed.returncode == 0, completed.stderr + completed.stdout
    return json.loads(
        (output_dir / "scenario-short.json").read_text(encoding="utf-8")
    )


def test_scenario_result_schema_is_repeatable(service_factory, tmp_path: Path) -> None:
    service = service_factory()
    scenario = {
        "schema_version": "scenario-v1",
        "name": "short",
        "base_url": service.base_url,
        "admin_token": service.admin_token,
        "duration_seconds": 1.2,
        "warmup_seconds": 0,
        "settle_seconds": 0.1,
        "max_workers": 6,
        "max_pending": 32,
        "tenants": {"tenant-alpha": 1.0},
        "statuses": ["created"],
        "page_size": 5,
        "traffic_profiles": [
            {
                "start_seconds": 0,
                "end_seconds": 1.2,
                "request_rate_per_second": 8,
                "read_ratio": 0.75,
            }
        ],
        "write_mix": {"create_ratio": 0.5},
        "faults": [],
        "request_timeout_seconds": 2,
    }
    scenario_path = tmp_path / "scenario.json"
    scenario_path.write_text(json.dumps(scenario), encoding="utf-8")
    first = _run_scenario(service, scenario_path, tmp_path / "first")
    second = _run_scenario(service, scenario_path, tmp_path / "second")
    assert _key_structure(first) == _key_structure(second)
    assert first["schema_version"] == "scenario-result-v1"
    assert "latency_ms" in first["tenants"]["tenant-alpha"]
    assert "error_budget" in first
    assert "events" in first["audit"]
    assert "capacity_report" in first

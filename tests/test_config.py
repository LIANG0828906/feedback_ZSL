from __future__ import annotations

import copy
import json
import subprocess
import sys
from pathlib import Path

import pytest

from order_guard.config import ConfigError, load_config


ROOT = Path(__file__).resolve().parents[1]


def _default_config() -> dict:
    return json.loads(
        (ROOT / "config" / "service.json").read_text(encoding="utf-8")
    )


def test_default_config_is_valid() -> None:
    config = load_config(ROOT / "config" / "service.json")
    assert config.service.port == 8000
    assert config.slis["availability"].kind == "availability"
    assert config.slis["latency"].target_ms == 350
    assert config.capacity.page_size_cap < config.query.max_page_size


def test_invalid_tenant_limit_reports_exact_path(tmp_path: Path) -> None:
    raw = _default_config()
    raw["tenants"][0]["rate_per_second"] = 0
    path = tmp_path / "invalid.json"
    path.write_text(json.dumps(raw), encoding="utf-8")
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "$.tenants[0].rate_per_second" in str(exc.value)


def test_invalid_sli_level_is_rejected(tmp_path: Path) -> None:
    raw = copy.deepcopy(_default_config())
    raw["slis"]["latency"]["degrade_level"] = 9
    path = tmp_path / "invalid-sli.json"
    path.write_text(json.dumps(raw), encoding="utf-8")
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "$.slis.latency.degrade_level" in str(exc.value)


def test_unknown_key_is_rejected(tmp_path: Path) -> None:
    raw = _default_config()
    raw["service"]["typo_threads"] = 3
    path = tmp_path / "unknown.json"
    path.write_text(json.dumps(raw), encoding="utf-8")
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "$.service.typo_threads" in str(exc.value)


def test_cli_refuses_to_start_with_invalid_config(tmp_path: Path) -> None:
    raw = _default_config()
    raw["tenants"][0]["max_inflight"] = 0
    path = tmp_path / "invalid-startup.json"
    path.write_text(json.dumps(raw), encoding="utf-8")
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "order_guard",
            "--config",
            str(path),
        ],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert completed.returncode == 2
    assert "$.tenants[0].max_inflight" in completed.stderr

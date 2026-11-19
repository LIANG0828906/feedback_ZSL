from __future__ import annotations

import json
from pathlib import Path

import pytest

from orders_migration.config import Config


def _write_config(
    path: Path,
    *,
    read_mode: str = "legacy",
    shard_count: int = 4,
) -> None:
    path.write_text(
        json.dumps(
            {
                "listen": {"host": "127.0.0.1", "port": 8090},
                "database": {
                    "driver": "pymssql",
                    "legacy_connection_string": (
                        "server=127.0.0.1;database=Legacy;"
                        "user=sa;password=${TEST_DB_PASSWORD}"
                    ),
                    "shard_connection_string_template": (
                        "server=127.0.0.1;database=Shard{shard};"
                        "user=sa;password=${TEST_DB_PASSWORD}"
                    ),
                    "shard_count": shard_count,
                },
                "migration": {
                    "batch_size": 100,
                    "read_mode_during_migration": read_mode,
                },
                "audit": {"sqlite_path": "audit.db"},
            }
        ),
        encoding="utf-8",
    )


def test_config_expands_environment_and_shard_template(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("TEST_DB_PASSWORD", "secret-value")
    path = tmp_path / "config.json"
    _write_config(path)
    config = Config.load(path)
    assert config.database.shard_count == 4
    assert "secret-value" in config.database.legacy_connection_string
    assert "Shard3" in config.database.shard_connection_string(3)
    assert config.sqlite_path == (tmp_path / "audit.db").resolve()


def test_config_rejects_unknown_read_mode(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("TEST_DB_PASSWORD", "secret-value")
    path = tmp_path / "config.json"
    _write_config(path, read_mode="mixed")
    with pytest.raises(ValueError, match="read_mode_during_migration"):
        Config.load(path)


def test_config_requires_environment_value(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("TEST_DB_PASSWORD", raising=False)
    path = tmp_path / "config.json"
    _write_config(path)
    with pytest.raises(ValueError, match="TEST_DB_PASSWORD"):
        Config.load(path)


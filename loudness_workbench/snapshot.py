"""Atomic JSON report snapshots with version and hash-chain validation."""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from typing import Mapping

from .errors import ErrorCode, MeterError, error
from .models import (
    SCHEMA_VERSION,
    AssetReport,
    verify_report_chain,
)


def _snapshot_dict(report: AssetReport) -> dict:
    payload = report.to_dict()
    payload["snapshot_schema_version"] = SCHEMA_VERSION
    return payload


def _atomic_write_json(path: Path, payload: Mapping) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(
        payload,
        sort_keys=True,
        indent=2,
        ensure_ascii=False,
        allow_nan=False,
    )
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(text)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        try:
            directory_fd = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            pass
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


class SnapshotStore:
    def __init__(self, path: str | Path):
        self.path = Path(path)

    def save(
        self,
        report: AssetReport,
        *,
        expected_parameter_version: int | None = None,
    ) -> Path:
        verify_report_chain(report)
        if (
            expected_parameter_version is not None
            and report.parameter_version != expected_parameter_version
        ):
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "refusing to save a report with the wrong parameter version",
                {
                    "expected": expected_parameter_version,
                    "actual": report.parameter_version,
                },
            )
        _atomic_write_json(self.path, _snapshot_dict(report))
        return self.path

    def load(
        self,
        *,
        expected_parameter_version: int | None = None,
        expected_config_hash: str | None = None,
    ) -> AssetReport:
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot file does not exist",
                {"path": str(self.path)},
            ) from None
        except (OSError, ValueError) as exc:
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot file cannot be parsed",
                {"path": str(self.path), "error": str(exc)},
            ) from None
        if not isinstance(raw, Mapping):
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot root must be an object",
                {"path": str(self.path)},
            )
        version = raw.get("snapshot_schema_version")
        if version != SCHEMA_VERSION or raw.get("schema_version") != SCHEMA_VERSION:
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot schema version mismatch",
                {
                    "path": str(self.path),
                    "expected": SCHEMA_VERSION,
                    "actual": version,
                },
            )
        raw_blocks = raw.get("blocks")
        if not isinstance(raw_blocks, list):
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot blocks must be an array",
                {"path": str(self.path)},
            )
        if "block_count" in raw:
            try:
                declared_count = int(raw["block_count"])
            except (TypeError, ValueError):
                raise error(
                    ErrorCode.E_REPORT_MISMATCH,
                    "snapshot block count is invalid",
                    {"path": str(self.path), "block_count": raw.get("block_count")},
                ) from None
            if declared_count != len(raw_blocks):
                raise error(
                    ErrorCode.E_REPORT_MISMATCH,
                    "snapshot block count does not match the block array",
                    {
                        "path": str(self.path),
                        "block_count": declared_count,
                        "actual": len(raw_blocks),
                    },
                )
        if (
            expected_parameter_version is not None
            and int(raw.get("parameter_version", -1)) != expected_parameter_version
        ):
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot parameter version mismatch",
                {
                    "path": str(self.path),
                    "expected": expected_parameter_version,
                    "actual": raw.get("parameter_version"),
                },
            )
        if (
            expected_config_hash is not None
            and raw.get("config_hash") != expected_config_hash
        ):
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot config hash mismatch",
                {
                    "path": str(self.path),
                    "expected": expected_config_hash,
                    "actual": raw.get("config_hash"),
                },
            )
        try:
            report = AssetReport.from_dict(raw)
            verify_report_chain(report)
        except MeterError:
            raise
        except (KeyError, TypeError, ValueError) as exc:
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "snapshot report structure is invalid",
                {"path": str(self.path), "error": str(exc)},
            ) from None
        return report


class RecoveryManager:
    """Recovery path that never trusts in-memory values after termination."""

    def __init__(self, snapshot_dir: str | Path):
        self.snapshot_dir = Path(snapshot_dir)

    def path_for(self, asset_id: str) -> Path:
        safe = "".join(
            character if character.isalnum() or character in {"-", "_"} else "_"
            for character in asset_id
        )
        return self.snapshot_dir / f"{safe}.json"

    def recover(
        self,
        asset_id: str,
        *,
        expected_parameter_version: int | None = None,
        expected_config_hash: str | None = None,
    ) -> AssetReport:
        store = SnapshotStore(self.path_for(asset_id))
        return store.load(
            expected_parameter_version=expected_parameter_version,
            expected_config_hash=expected_config_hash,
        )

    def save(self, report: AssetReport) -> Path:
        return SnapshotStore(self.path_for(report.asset_id)).save(report)

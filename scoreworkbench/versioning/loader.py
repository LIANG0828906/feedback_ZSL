"""Unified loading entry point for every supported schema version."""

from __future__ import annotations
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from ..constants import RESOURCE_LIMITS, SUPPORTED_SCHEMA_VERSIONS
from ..errors import DecodeError, SchemaError
from ..model import Score, TraceRecord
from ..resources import check_event_buffer, open_file_slot, parallel_map
from . import v1_loader, v2_loader, v3_loader

_LOADERS = {
    "1.0": v1_loader,
    "2.0": v2_loader,
    "3.0": v3_loader,
}


@dataclass
class LoadedProject:
    score: Score
    provenance: dict[str, Any]
    source_path: str
    source_version: str


def _read_json(path: str | Path) -> dict[str, Any]:
    source_path = str(path)
    try:
        with open_file_slot():
            with open(source_path, "r", encoding="utf-8") as handle:
                raw = handle.read()
    except OSError as exc:
        raise DecodeError(
            f"File cannot be read: {exc}",
            {"file_path": source_path},
        ) from exc
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise DecodeError(
            "JSON content is corrupted",
            {
                "file_path": source_path,
                "line": exc.lineno,
                "column": exc.colno,
                "first_violation": f"line {exc.lineno} column {exc.colno}",
            },
        ) from exc
    if not isinstance(data, dict):
        raise DecodeError(
            "Top-level JSON document must be an object",
            {"file_path": source_path},
        )
    return data


def _detect_version(data: dict[str, Any], source_path: str) -> str:
    version = data.get("schema_version")
    if version is None:
        version = data.get("schemaVersion")
    field_path = (
        "root.schema_version" if "schema_version" in data else "root.schemaVersion"
    )
    if version is None:
        raise SchemaError(
            "Schema version identifier is missing",
            {
                "file_path": source_path,
                "version": None,
                "field_path": "root.schema_version",
            },
        )
    if version not in SUPPORTED_SCHEMA_VERSIONS:
        raise SchemaError(
            f"Unknown schema version {version!r}",
            {
                "file_path": source_path,
                "version": version,
                "field_path": field_path,
                "supported_versions": list(SUPPORTED_SCHEMA_VERSIONS),
            },
        )
    return version


def load_score(path: str | Path) -> LoadedProject:
    """Load one score file into the canonical domain model."""
    source_path = str(Path(path))
    data = _read_json(source_path)
    version = _detect_version(data, source_path)
    traces: list[TraceRecord] = [
        TraceRecord(
            step=f"load {version}",
            source_version=version,
            target_version=version,
            object_path="root",
            field_path="schema_version",
            action="identify",
            detail=f"detected schema {version}",
        )
    ]
    module = _LOADERS[version]
    score, provenance = module.load(data, source_path, traces)
    check_event_buffer(len(score.events))
    score.rebuild_index()
    from .common import validate_canonical_enums

    validate_canonical_enums(score)
    provenance["source_version"] = version
    provenance["source_path"] = source_path
    return LoadedProject(
        score=score,
        provenance=provenance,
        source_path=source_path,
        source_version=version,
    )


def load_many(paths) -> list[LoadedProject]:
    """Parallel batch loading; ordering of results matches ``paths``."""
    paths = [str(path) for path in paths]
    if len(paths) > RESOURCE_LIMITS["max_open_files"] * 4:
        raise SchemaError(
            "Batch size exceeds the supported open-file budget",
            {"batch_size": len(paths),
             "limit": RESOURCE_LIMITS["max_open_files"] * 4},
        )
    return parallel_map(load_score, paths)

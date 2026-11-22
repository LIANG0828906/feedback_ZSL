"""Shared helpers for schema version implementations."""

from __future__ import annotations

from typing import Any, Mapping

from ..errors import VersionError


def require(doc: Mapping[str, Any], key: str, path: str) -> Any:
    """Fetch a required field; never silently substitute a default.

    ``path`` is the parent path of ``key`` ("" for the document root).
    """
    if key not in doc:
        full = f"{path}.{key}" if path else key
        raise VersionError(f"missing required field {full}", field_path=full)
    return doc[key]


def require_type(value: Any, types: tuple, path: str) -> Any:
    if not isinstance(value, types):
        raise VersionError(f"field {path} has wrong type", field_path=path)
    return value

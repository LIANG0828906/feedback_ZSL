from __future__ import annotations

from typing import Any

import numpy as np

from .errors import E_STITCH_FAIL, E_FIELD_STALE, fail
from .hashing import sha256_array


def validate_coefficients(coeffs: np.ndarray, expected_hash: str) -> None:
    if coeffs.ndim != 2 or coeffs.shape[0] != 6:
        fail(E_FIELD_STALE, "field coefficient matrix is not 5.1")
    actual = sha256_array(coeffs)
    if actual != expected_hash:
        fail(
            E_FIELD_STALE,
            "field coefficient content hash does not match its manifest",
            expected=expected_hash,
            actual=actual,
        )


def field_manifest(
    *,
    point_id: str,
    version: str,
    dependency_hash: str,
    dependencies: list[dict[str, Any]],
    coefficient_hash: str,
    summary: dict[str, Any],
    channel_layout: list[str],
) -> dict[str, Any]:
    if not version or not dependency_hash or not coefficient_hash:
        fail(E_STITCH_FAIL, "field manifest is missing required hashes")
    return {
        "schema": "acoustic-field/v1",
        "point_id": point_id,
        "version": version,
        "dependency_hash": dependency_hash,
        "dependencies": dependencies,
        "coefficient_hash": coefficient_hash,
        "channel_layout": list(channel_layout),
        "summary": summary,
    }


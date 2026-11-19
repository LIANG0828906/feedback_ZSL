"""Feature protocol declarations and request feature validation."""

from __future__ import annotations

import json
from dataclasses import dataclass
from numbers import Real
from pathlib import Path

from .errors import ErrorCode, GatewayError


@dataclass(frozen=True)
class ProtocolSpec:
    version: str
    model_version: str
    columns: tuple[str, ...]
    model_file: str

    @classmethod
    def from_mapping(cls, model_version: str, raw: dict) -> "ProtocolSpec":
        return cls(
            version=raw["protocol_version"],
            model_version=model_version,
            columns=tuple(raw["columns"]),
            model_file=raw["model_file"],
        )


def load_protocol_mappings(mapping_path: str | Path) -> dict[str, ProtocolSpec]:
    with open(mapping_path, "r", encoding="utf-8") as handle:
        raw = json.load(handle)
    return {version: ProtocolSpec.from_mapping(version, item) for version, item in raw.items()}


_BOOL = isinstance(True, int)


def _is_number(value) -> bool:
    # bool is a subclass of int but is not a valid feature scalar
    return isinstance(value, Real) and not isinstance(value, bool)


def validate_features(declared_protocol: str, columns, features, spec: ProtocolSpec) -> None:
    """Validate count, order and scalar type against the target model protocol.

    Raises GatewayError with a field path pinpointing the offending position.
    Nothing is returned: a request that passes this check is safe to enqueue.
    """

    if declared_protocol != spec.version:
        raise GatewayError(
            ErrorCode.PROTOCOL_MISMATCH,
            f"declared protocol '{declared_protocol}' does not match target "
            f"model {spec.model_version} protocol '{spec.version}'",
            field="protocol_version",
        )

    if not isinstance(columns, list) or not columns:
        raise GatewayError(
            ErrorCode.INVALID_REQUEST,
            "columns must be a non-empty list",
            field="columns",
        )
    if not isinstance(features, list) or not features:
        raise GatewayError(
            ErrorCode.INVALID_REQUEST,
            "features must be a non-empty list",
            field="features",
        )

    if len(columns) != len(spec.columns):
        raise GatewayError(
            ErrorCode.FEATURE_COUNT_MISMATCH,
            f"expected {len(spec.columns)} columns for protocol {spec.version}, "
            f"got {len(columns)}",
            field="columns",
        )
    if len(features) != len(spec.columns):
        raise GatewayError(
            ErrorCode.FEATURE_COUNT_MISMATCH,
            f"expected {len(spec.columns)} features for protocol {spec.version}, "
            f"got {len(features)}",
            field="features",
        )

    for index, (sent, expected) in enumerate(zip(columns, spec.columns)):
        if sent != expected:
            raise GatewayError(
                ErrorCode.FEATURE_COLUMN_MISMATCH,
                f"column position {index} expects '{expected}' but got '{sent}'",
                field=f"columns[{index}]",
            )

    for index, value in enumerate(features):
        if not _is_number(value):
            raise GatewayError(
                ErrorCode.FEATURE_TYPE_MISMATCH,
                f"feature at position {index} must be a number, got "
                f"{type(value).__name__}",
                field=f"features[{index}]",
            )

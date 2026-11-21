"""Public error codes and structured diagnostics."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_MARKER_MISSING = "E_MARKER_MISSING"
E_VERSION_LOSS = "E_VERSION_LOSS"
E_VERSION_INVALID = "E_VERSION_INVALID"
E_VERSION_STALE = "E_VERSION_STALE"
E_DRIFT_UNKNOWN = "E_DRIFT_UNKNOWN"
E_RESIDUAL_THRESHOLD = "E_RESIDUAL_THRESHOLD"
E_MODE_INVALID = "E_MODE_INVALID"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_WAV_INVALID = "E_WAV_INVALID"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_CHANNEL_INVALID = "E_CHANNEL_INVALID"
E_ALIGNMENT_FAILED = "E_ALIGNMENT_FAILED"


@dataclass
class Issue:
    code: str
    message: str
    severity: str = "error"
    channel: int | None = None
    marker_index: int | None = None
    segment_index: int | None = None
    fields: list[str] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "severity": self.severity,
        }
        for key in ("channel", "marker_index", "segment_index"):
            value = getattr(self, key)
            if value is not None:
                result[key] = value
        if self.fields:
            result["fields"] = list(self.fields)
        if self.details:
            result["details"] = _json_ready(self.details)
        return result

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Issue":
        return cls(
            code=str(data.get("code", "E_UNKNOWN")),
            message=str(data.get("message", "")),
            severity=str(data.get("severity", "error")),
            channel=_optional_int(data.get("channel")),
            marker_index=_optional_int(data.get("marker_index")),
            segment_index=_optional_int(data.get("segment_index")),
            fields=[str(value) for value in data.get("fields", [])],
            details=dict(data.get("details", {})),
        )


class SyncError(Exception):
    """A hard failure with a stable machine-readable error code."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        channel: int | None = None,
        marker_index: int | None = None,
        segment_index: int | None = None,
        fields: list[str] | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.channel = channel
        self.marker_index = marker_index
        self.segment_index = segment_index
        self.fields = list(fields or [])
        self.details = dict(details or {})

    def to_issue(self) -> Issue:
        return Issue(
            code=self.code,
            message=self.message,
            channel=self.channel,
            marker_index=self.marker_index,
            segment_index=self.segment_index,
            fields=self.fields,
            details=self.details,
        )

    def to_dict(self) -> dict[str, Any]:
        return self.to_issue().to_dict()


def _optional_int(value: Any) -> int | None:
    if value is None:
        return None
    return int(value)


def _json_ready(value: Any) -> Any:
    import numpy as np

    if isinstance(value, dict):
        return {str(key): _json_ready(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_ready(item) for item in value]
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        return value.tolist()
    return value


def deduplicate_issues(issues: list[Issue]) -> tuple[Issue, ...]:
    output: list[Issue] = []
    seen: set[tuple[Any, ...]] = set()
    for issue in issues:
        key = (
            issue.code,
            issue.channel,
            issue.marker_index,
            issue.segment_index,
            tuple(issue.fields),
        )
        if key in seen:
            continue
        seen.add(key)
        output.append(issue)
    return tuple(output)


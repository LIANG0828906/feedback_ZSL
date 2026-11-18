from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class Issue:
    code: str
    message: str
    channel: str | None = None
    group: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.channel:
            value["channel"] = self.channel
        if self.group:
            value["group"] = self.group
        if self.details:
            value["details"] = self.details
        return value


class BusinessError(Exception):
    """A validation or rendering failure with stable machine-readable codes."""

    def __init__(
        self,
        issues: Issue | list[Issue],
        exit_code: int = 2,
        *,
        context: dict[str, Any] | None = None,
    ) -> None:
        self.issues = issues if isinstance(issues, list) else [issues]
        self.exit_code = exit_code
        self.context = context or {}
        super().__init__(self.issues[0].message if self.issues else "business error")

    def payload(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "errors": [issue.as_dict() for issue in self.issues],
        }
        if self.context:
            result["context"] = self.context
        return result


def invalid(message: str, *, channel: str | None = None, group: str | None = None, **details: Any) -> Issue:
    return Issue("E_INVALID_MANIFEST", message, channel=channel, group=group, details=details)


def warning(code: str, message: str, *, channel: str | None = None, group: str | None = None, **details: Any) -> dict[str, Any]:
    item: dict[str, Any] = {"code": code, "message": message}
    if channel:
        item["channel"] = channel
    if group:
        item["group"] = group
    if details:
        item["details"] = details
    return item


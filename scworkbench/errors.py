from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


ERROR_MESSAGES: dict[str, str] = {
    "E_SCHEMA": "会话语义数据不符合协议结构或能力集取值非法",
    "E_DECODE": "帧序列或输入事件流内容损坏，无法解码",
    "E_NEGO": "双方能力集不存在可满足的能力交集",
    "E_STATE": "当前会话阶段拒绝该操作，阶段与进度保持不变",
    "E_SESSION": "媒体包会话标识不匹配，已丢弃",
    "E_LOSS": "媒体分片缺失，已发起有限次重传",
    "E_ORDER": "媒体分片乱序到达，已按序号重组",
    "E_CAP": "运行期能力变化被拒绝，已保留协商基线",
    "E_CORRUPT": "媒体分片损坏，已丢弃并请求重传",
    "E_DUP": "输入事件重复或过期，已幂等去重",
    "E_DIFF": "实时、恢复与重演结果不一致",
}


@dataclass
class WorkbenchError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}


class SchemaError(WorkbenchError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_SCHEMA", message, details)


class DecodeError(WorkbenchError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_DECODE", message, details)


class NegotiationError(WorkbenchError):
    def __init__(self, message: str, items: list[str], **details: Any) -> None:
        super().__init__("E_NEGO", message, {"items": items, **details})


class StateError(WorkbenchError):
    def __init__(self, message: str, current_phase: str, **details: Any) -> None:
        super().__init__(
            "E_STATE", message, {"current_phase": current_phase, **details}
        )


class SessionMismatchError(WorkbenchError):
    def __init__(self, expected: str, actual: str, **details: Any) -> None:
        super().__init__(
            "E_SESSION",
            "媒体包会话标识与当前会话不一致，包已丢弃",
            {"expected": expected, "actual": actual, **details},
        )


class LossError(WorkbenchError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_LOSS", message, details)


class OrderError(WorkbenchError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_ORDER", message, details)


class CapabilityChangeError(WorkbenchError):
    def __init__(self, message: str, items: list[str], **details: Any) -> None:
        super().__init__("E_CAP", message, {"items": items, **details})


class CorruptError(WorkbenchError):
    def __init__(self, message: str, **details: Any) -> None:
        super().__init__("E_CORRUPT", message, details)


class DuplicateInputError(WorkbenchError):
    def __init__(self, input_seq: int, last_seq: int, **details: Any) -> None:
        super().__init__(
            "E_DUP",
            "输入事件重复或过期，桌面状态未改变",
            {"input_seq": input_seq, "last_replayed_seq": last_seq, **details},
        )


class DiffError(WorkbenchError):
    def __init__(
        self,
        component: str,
        index: int | None,
        field_name: str,
        phase: str,
        left: Any,
        right: Any,
        **details: Any,
    ) -> None:
        super().__init__(
            "E_DIFF",
            "三条处理路径的结果不一致",
            {
                "component": component,
                "index": index,
                "field": field_name,
                "phase": phase,
                "left": left,
                "right": right,
                **details,
            },
        )


def error_payload(code: str, message: str | None = None, **details: Any) -> dict[str, Any]:
    return {
        "code": code,
        "message": message or ERROR_MESSAGES.get(code, code),
        "details": details,
    }

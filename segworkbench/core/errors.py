"""确定性的业务错误码与异常类型。

所有失败路径都通过 :class:`WorkbenchError` 携带稳定的错误码与可追溯
上下文，调用方可以只依赖 ``code`` 做断言，而不解析错误消息文本。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Optional


class ErrorCode:
    """业务错误码常量。命名以任务书为准，保持稳定不随实现变化。"""

    E_DECODE = "E_DECODE"
    E_META = "E_META"
    E_PARA = "E_PARA"
    E_SEED = "E_SEED"
    E_SPILL = "E_SPILL"
    E_REGION = "E_REGION"
    E_BLOCK = "E_BLOCK"
    E_STATE = "E_STATE"
    E_VERSION = "E_VERSION"
    E_HASH = "E_HASH"
    E_CANCELLED = "E_CANCELLED"
    E_IO = "E_IO"


@dataclass
class WorkbenchError(Exception):
    """带稳定错误码的业务异常。

    :param code: :class:`ErrorCode` 中的常量。
    :param message: 面向人的说明。
    :param context: 可序列化的追溯字段（块编号、阶段、注入点等）。
    """

    code: str
    message: str
    context: Dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:  # pragma: no cover - 仅展示用
        suffix = f" context={self.context}" if self.context else ""
        return f"[{self.code}] {self.message}{suffix}"


def fail(code: str, message: str, **context: Any) -> WorkbenchError:
    """构造异常的便捷函数。"""
    return WorkbenchError(code=code, message=message, context=context)


def missing_required(value: Optional[Any], field_name: str) -> None:
    """真值/参数缺字段时统一抛出 E_META 并指出字段名。"""
    if value is None:
        raise fail(ErrorCode.E_META, f"缺少必填字段: {field_name}", field=field_name)

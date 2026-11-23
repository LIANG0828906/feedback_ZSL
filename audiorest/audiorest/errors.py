"""工作台统一错误码与异常类型。

所有面向业务的失败都抛出 :class:`WorkbenchError`，携带稳定的错误码、
首个出错字段路径 ``path`` 以及结构化细节 ``details``，便于界面与测试定位。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_VER = "E_VER"          # 工程版本未知 / 必填字段缺失
E_LOSSY = "E_LOSSY"      # 降级导出存在无法表达的字段
E_PARA = "E_PARA"        # 修复参数越界
E_XVER = "E_XVER"        # 跨版本内部模型不等价
E_STATE = "E_STATE"      # 窗口状态机非法迁移
E_DIFF = "E_DIFF"        # 多路径输出逐样本不一致
E_CKPT = "E_CKPT"        # 检查点损坏或版本不匹配（内部使用）


@dataclass
class WorkbenchError(Exception):
    """工作台统一异常。

    :param code: 稳定错误码（模块级 ``E_*`` 常量）
    :param message: 人类可读描述
    :param path: 首个相关字段路径，如 ``stages[1].config.gain``
    :param details: 结构化细节（差异清单、损失清单等）
    """

    code: str
    message: str
    path: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:  # pragma: no cover - 调试辅助
        loc = f" @ {self.path}" if self.path else ""
        return f"[{self.code}] {self.message}{loc}"


def raise_ver(message: str, path: str | None = None, **details: Any) -> None:
    raise WorkbenchError(E_VER, message, path, details)


def raise_para(message: str, path: str | None = None, **details: Any) -> None:
    raise WorkbenchError(E_PARA, message, path, details)


def raise_state(message: str, path: str | None = None, **details: Any) -> None:
    raise WorkbenchError(E_STATE, message, path, details)

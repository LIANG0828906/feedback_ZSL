"""错误码与错误链路。

所有模块通过 :class:`CadError` 抛出带错误码的异常，``chain`` 字段按时间顺序
记录整条错误链路（含根因与上下文），可序列化到 JSON 供检查点与界面复用。
"""
from __future__ import annotations

import time
import traceback
from dataclasses import dataclass, field, asdict
from typing import Any, Dict, List, Optional


# ---- 业务错误码（与任务 1/2/4/5/7/8 对应）----
E_GEOM = "E_GEOM"      # 曲面描述损坏 / 几何非法
E_META = "E_META"      # 元数据 / 真值清单与曲面片数量不一致
E_PARA = "E_PARA"      # 参数越界
E_DIVERGE = "E_DIVERGE"  # 预览与精确帧一致性比对失败
E_VER = "E_VER"        # 版本快照冻结 / 版本不匹配
E_STATE = "E_STATE"    # 非法状态迁移
E_MEM = "E_MEM"        # 帧缓冲 / 缓冲预算分配失败
E_TILE = "E_TILE"      # 单块内部异常（可重试，不污染其他块）
E_IO = "E_IO"          # 持久化读写失败
E_INTERNAL = "E_INTERNAL"


@dataclass
class ErrorRecord:
    code: str
    message: str
    module: str = ""
    field_name: Optional[str] = None
    context: Dict[str, Any] = field(default_factory=dict)
    ts: float = field(default_factory=lambda: time.time())

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)


class CadError(Exception):
    """带错误码与链路的业务异常基类。"""

    default_code = E_INTERNAL

    def __init__(
        self,
        message: str,
        code: Optional[str] = None,
        *,
        module: str = "",
        field: Optional[str] = None,
        context: Optional[Dict[str, Any]] = None,
        cause: Optional[BaseException] = None,
    ) -> None:
        super().__init__(message)
        self.code = code or self.default_code
        self.chain: List[ErrorRecord] = [
            ErrorRecord(
                self.code,
                message,
                module=module,
                field_name=field,
                context=dict(context or {}),
            )
        ]
        if cause is not None:
            self.chain.append(
                ErrorRecord(
                    E_INTERNAL,
                    f"{type(cause).__name__}: {cause}",
                    module=module,
                    context={"traceback": traceback.format_exc()},
                )
            )

    def append(
        self,
        code: str,
        message: str,
        *,
        module: str = "",
        field: Optional[str] = None,
        context: Optional[Dict[str, Any]] = None,
    ) -> "CadError":
        self.chain.append(
            ErrorRecord(code, message, module=module, field_name=field,
                        context=dict(context or {}))
        )
        return self

    def to_dict(self) -> Dict[str, Any]:
        return {
            "code": self.code,
            "message": str(self),
            "chain": [r.to_dict() for r in self.chain],
        }


class GeomError(CadError):
    default_code = E_GEOM


class MetaError(CadError):
    default_code = E_META


class ParaError(CadError):
    default_code = E_PARA


class DivergeError(CadError):
    default_code = E_DIVERGE


class VersionError(CadError):
    default_code = E_VER


class StateError(CadError):
    default_code = E_STATE


class MemError(CadError):
    default_code = E_MEM


class TileError(CadError):
    default_code = E_TILE


class IoError(CadError):
    default_code = E_IO

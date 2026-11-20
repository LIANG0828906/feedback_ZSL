"""业务错误码与可定位的管线异常。

所有解析/映射/重定向/恢复失败都通过 :class:`PipelineError` 抛出，
其 :attr:`code` 为稳定的业务码字符串，:attr:`locations` 携带
``joint`` / ``field`` / ``frame`` 等定位信息，便于报告与追溯。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping as _Mapping

# ---- 稳定业务码 ---------------------------------------------------------
E_ANIM_FORMAT_INVALID = "E_ANIM_FORMAT_INVALID"
E_MAP_CONFLICT = "E_MAP_CONFLICT"
E_VERSION_LOSS = "E_VERSION_LOSS"
E_POSE_UNRESOLVED = "E_POSE_UNRESOLVED"
E_CONSISTENCY_FAIL = "E_CONSISTENCY_FAIL"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"

ALL_CODES = (
    E_ANIM_FORMAT_INVALID,
    E_MAP_CONFLICT,
    E_VERSION_LOSS,
    E_POSE_UNRESOLVED,
    E_CONSISTENCY_FAIL,
    E_RECOVERY_INVALID,
    E_BUSY,
    E_CANCELLED,
)


@dataclass
class PipelineError(Exception):
    """带业务码与定位信息的异常。

    :param code: 取自本模块的 ``E_*`` 常量。
    :param message: 人类可读描述。
    :param locations: 定位字典列表，例如
        ``[{"joint": "Spine", "field": "parent", "frame": 3}]``。
    """

    code: str
    message: str
    locations: list[dict[str, Any]] = field(default_factory=list)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "locations": list(self.locations),
        }

    def __str__(self) -> str:  # pragma: no cover - 调试便利
        loc = f" locations={self.locations}" if self.locations else ""
        return f"[{self.code}] {self.message}{loc}"


def format_locations(locations: _Mapping[str, Any] | list[_Mapping[str, Any]] | None) -> list[dict[str, Any]]:
    """把单个/多个定位字典规整成列表，剔除值为 ``None`` 的键。"""

    if locations is None:
        return []
    if isinstance(dict, type(locations)) or isinstance(locations, dict):
        locations = [locations]
    cleaned: list[dict[str, Any]] = []
    for item in locations:
        cleaned.append({k: v for k, v in dict(item).items() if v is not None})
    return cleaned


def raise_format(message: str, *, joint: str | None = None, field_name: str | None = None,
                 frame: int | None = None, extra: dict[str, Any] | None = None) -> None:
    """抛出 :data:`E_ANIM_FORMAT_INVALID`。"""

    loc: dict[str, Any] = {"joint": joint, "field": field_name, "frame": frame}
    if extra:
        loc.update(extra)
    raise PipelineError(E_ANIM_FORMAT_INVALID, message, format_locations(loc))

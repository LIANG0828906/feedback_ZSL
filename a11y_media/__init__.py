"""a11y_media: 游戏过场与互动媒体无障碍呈现领域组件。

统一定点时间单位(采样点)贯穿
"包络检测 -> 台词窗口推导 -> 字符高亮排程 -> 渲染与朗读输出" 全链路,
并在每个阶段输出可观测的误差统计与上界估计。
"""

from .api import MediaAccessibilityService
from .engine import Engine, ProjectResult, run_full
from .errors import (
    E_DIVERGE,
    E_ERR,
    E_PARA,
    E_SCHEMA,
    E_STATE,
    ComponentError,
)
from .loader import load_project

__all__ = [
    "MediaAccessibilityService",
    "Engine",
    "ProjectResult",
    "run_full",
    "load_project",
    "ComponentError",
    "E_SCHEMA",
    "E_PARA",
    "E_ERR",
    "E_STATE",
    "E_DIVERGE",
]

__version__ = "1.0.0"
